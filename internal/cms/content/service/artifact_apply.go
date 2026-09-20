package service

import (
	"context"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/cms/content/repository"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// Applying an artifact (ADR-008).
//
// The governing rule is one line, and everything below follows from it:
//
//	Apply is NOT a new write path. It decomposes into the schema verbs that
//	already exist, so the refusals those verbs carry apply unchanged.
//
// An importer that parsed the document and wrote content_type_fields itself
// would be a second implementation of ADR-007's invariant, and the person who
// breaks it months from now will be someone who never read that ADR. It is the
// same failure mode migration 000024 answered by pinning field_type in the
// database — this is the larger version of it.

// PlanStep is one change plus what the database says about it.
type PlanStep struct {
	domain.SchemaChange
	// Blocked reports that a guarded change cannot proceed against the stored
	// data. Entries is the count that decides it — the same number the verb's
	// own 409 would carry.
	Blocked bool `json:"blocked"`
	Entries int  `json:"entries,omitempty"`
	// Skipped marks a destructive change left alone because prune was not set.
	Skipped bool `json:"skipped,omitempty"`
}

type PlanResult struct {
	Steps []PlanStep `json:"steps"`
	// Applicable is the count of steps an apply would actually execute.
	Applicable int `json:"applicable"`
	// Refused and Blocked are why the rest will not run.
	Refused int `json:"refused"`
	Blocked int `json:"blocked"`
	// AppliedSteps is set only on ApproveSchemaProposal's response (缺口計畫
	// 5.1): the indices into Steps that actually ran. It is never set by
	// PlanSchema, ApplySchema or planScoped — those answer "what would run",
	// which is Steps and Applicable, not "what did". omitempty so a plan/apply
	// response is unchanged wherever nothing populates it, and so a stored
	// plan's JSON round-trip (planMatches) never sees this key: planScoped never
	// sets it on either side of that comparison.
	AppliedSteps []int `json:"applied_steps,omitempty"`
}

func (p PlanResult) canApply() bool { return p.Refused == 0 && p.Blocked == 0 }

// PlanSchema reports what applying art would do, and writes nothing.
//
// It diffs against a LIVE export rather than any stored copy, so "what the
// artifact would change" cannot go stale. Guarded steps are resolved against
// the actual entry counts, because whether a narrowing is safe is a question
// only the database can answer — that is what separates guarded from additive.
func (s *contentService) PlanSchema(ctx context.Context, art domain.Artifact, prune bool) (_ PlanResult, err error) {
	// A whole-schema verb names no single type, so the row carries none — the
	// same "" that keeps an agent out of the untyped paths (ADR-013 §4).
	// Recorded as a READ because a plan changes nothing; an agent's successful
	// plan still lands, because the dominating rule is about what an agent did,
	// not about whether it wrote.
	act := s.activityRead(ctx, domain.ActivitySchemaPlan, "")
	defer func() { act.finish(ctx, err) }()
	// Its own verb, and deliberately not the read verb: a plan reports entry
	// counts and which changes are blocked, which is schema administration
	// rather than reading content. Same tenant roles as apply — the split exists
	// so a caller CAN be given the dry run without the apply (ADR-013 §3), which
	// a shared verb makes unexpressible however the request arrives.
	// authorizeArtifact, not authorize: this path's content types are in the
	// document, so an agent's whitelist is enforced against THEM (ADR-013 補裁
	// E). Passing "" here is what made the verb §3 created for agents refuse
	// every agent that used it.
	sub, err := s.authorizeArtifact(ctx, ActionContentSchemaPlan, "collection", art)
	if err != nil {
		return PlanResult{}, err
	}
	return s.planWith(ctx, sub, art, prune)
}

func (s *contentService) planWith(ctx context.Context, sub authn.Subject, art domain.Artifact, prune bool) (PlanResult, error) {
	return s.planScoped(ctx, sub, art, prune, true)
}

// planScoped is planWith with the narrowing made explicit, because ONE caller
// needs it off: ProposeSchema stores the plan an APPROVER would get, and an
// approver's live schema is whole (ADR-013 補裁 Q).
//
// narrowToAgent is a parameter rather than a doctored subject on purpose. The
// alternative — handing planWith a copy of the subject with its agent fields
// cleared — would put a credential that claims not to be an agent into a
// function that also authorizes nothing and trusts what it is given, one edit
// away from being passed somewhere that decides access from it.
func (s *contentService) planScoped(ctx context.Context, sub authn.Subject, art domain.Artifact,
	prune, narrowToAgent bool) (PlanResult, error) {
	cts, err := s.repo.ListContentTypes(ctx, sub.TenantID)
	if err != nil {
		return PlanResult{}, err
	}
	if narrowToAgent {
		cts = visibleToAgent(sub, cts)
	}
	comps, err := s.repo.ListComponents(ctx, sub.TenantID)
	if err != nil {
		return PlanResult{}, err
	}
	if narrowToAgent {
		comps = componentsReferencedBy(sub, cts, comps)
	}
	live := domain.NewSchemaArtifact(cts, comps)
	byName := make(map[string]*domain.ContentType, len(cts))
	for _, ct := range cts {
		byName[ct.Name] = ct
	}
	compByName := make(map[string]*domain.Component, len(comps))
	for _, c := range comps {
		compByName[c.Name] = c
	}

	var out PlanResult
	for _, c := range domain.DiffSchemas(live, art) {
		step := PlanStep{SchemaChange: c}
		switch {
		case c.Grade == domain.GradeRefused:
			out.Refused++
		case isDestructive(c.Op) && !prune:
			step.Skipped = true
		case c.Grade == domain.GradeGuarded:
			var n int
			if c.Component != "" {
				n, err = s.componentBlockingCount(ctx, sub, compByName[c.Component], c, art)
			} else {
				n, err = s.blockingCount(ctx, sub.TenantID, byName[c.Type], c)
			}
			if err != nil {
				return PlanResult{}, err
			}
			if n > 0 {
				step.Blocked, step.Entries = true, n
				out.Blocked++
			} else {
				out.Applicable++
			}
		default:
			out.Applicable++
		}
		out.Steps = append(out.Steps, step)
	}
	return out, nil
}

// visibleToAgent narrows the LIVE side of the diff to the credential's
// whitelist. Found while landing 補裁 E, and it is the half the ruling did not
// cover: gating the ARTIFACT alone still leaks.
//
// DiffSchemas emits an OpDeleteType for every live type absent from the
// document, so an agent scoped to `post` that plans an artifact containing only
// `post` gets back a step naming every other content type in the tenant —
// invoice, salary, whatever they are called. Nothing was written and nothing was
// read, but §4's guarantee is about REACH, and a list of the tenant's type names
// is exactly what GET /types is closed to an agent for (§A). Gating one door and
// leaving the answer visible through the other is not a smaller version of the
// rule, it is the rule not holding.
//
// Narrowing the live side rather than filtering the steps afterwards, because
// the counts (Applicable/Refused/Blocked) are computed from the same diff and a
// post-filter would leave them describing rows the caller cannot see. What an
// agent gets is a plan of its own scope, which is the only plan it could act on:
// apply is a verb it does not hold (§3), and every entry verb is confined to the
// same whitelist.
//
// A human is untouched — sub.IsAgent() is false and the live schema is whole.
func visibleToAgent(sub authn.Subject, cts []*domain.ContentType) []*domain.ContentType {
	if !sub.IsAgent() {
		return cts
	}
	out := make([]*domain.ContentType, 0, len(cts))
	for _, ct := range cts {
		if sub.AllowsContentType(ct.Name) {
			out = append(out, ct)
		}
	}
	return out
}

// componentsReferencedBy narrows a tenant's components, for an AGENT, to the
// ones some type it may see embeds. It is the component twin of visibleToAgent:
// an agent's whitelist names types, and a component reaches the agent only
// through a type on that list (ADR-020 §6). Everyone else sees all of them.
func componentsReferencedBy(sub authn.Subject, cts []*domain.ContentType, comps []*domain.Component) []*domain.Component {
	if !sub.IsAgent() {
		return comps
	}
	used := map[string]bool{}
	for _, ct := range cts {
		for _, f := range ct.Fields {
			if f.Type == domain.FieldTypeComponent {
				used[f.ComponentName] = true
			}
		}
	}
	out := make([]*domain.Component, 0, len(comps))
	for _, c := range comps {
		if used[c.Name] {
			out = append(out, c)
		}
	}
	return out
}

func isDestructive(op domain.ChangeOp) bool {
	switch op {
	case domain.OpDeleteField, domain.OpDeleteType, domain.OpDeleteComponentField, domain.OpDeleteComponent:
		return true
	}
	return false
}

// componentBlockingCount is blockingCount for a component change. The guards
// are the same four the component verbs run (required backfill, enum in use,
// constraint tightening, data under a sub-field), counted over the ITEMS of
// every referring type's entries, plus the one that is the component's own:
// a component still embedded by a type field the artifact keeps cannot be
// pruned, and the count returned is how many such fields remain.
func (s *contentService) componentBlockingCount(ctx context.Context, sub authn.Subject, comp *domain.Component,
	c domain.SchemaChange, art domain.Artifact) (int, error) {
	if comp == nil {
		return 0, nil
	}
	refs, err := s.repo.ListComponentReferrers(ctx, sub.TenantID, comp.ID)
	if err != nil {
		return 0, err
	}
	if sub.IsAgent() {
		kept := refs[:0]
		for _, ref := range refs {
			if sub.AllowsContentType(ref.TypeName) {
				kept = append(kept, ref)
			}
		}
		refs = kept
	}
	switch c.Code {
	case "CONTENT_FIELD_REQUIRED_BACKFILL":
		return s.countComponentEntries(ctx, sub.TenantID, comp.Name, refs, itemLacks(c.Field))
	case "CONTENT_ENUM_VALUE_IN_USE":
		f, ok := comp.FieldByKey(c.Field)
		if !ok {
			return 0, nil
		}
		f.EnumValues = allowedFor(c, f)
		return s.countComponentEntries(ctx, sub.TenantID, comp.Name, refs, itemFails(f))
	case "CONTENT_FIELD_CONSTRAINT_BACKFILL":
		f, ok := comp.FieldByKey(c.Field)
		if !ok || c.Constraints == nil {
			return 0, nil
		}
		f.FieldConstraints = *c.Constraints
		return s.countComponentEntries(ctx, sub.TenantID, comp.Name, refs, itemFails(f))
	case "CONTENT_FIELD_HAS_DATA":
		n := 0
		for _, ref := range refs {
			k, err := s.repo.CountEntriesWithComponentSubKey(ctx, sub.TenantID, ref, comp.Name, c.Field)
			if err != nil {
				return 0, err
			}
			n += k
		}
		return n, nil
	case "CONTENT_COMPONENT_IN_USE":
		// Referrers the artifact KEEPS. A field the same artifact drops is
		// deleted before the component is (DiffSchemas orders it so), so it
		// does not stand in the way; a field the artifact keeps does.
		n := 0
		for _, ref := range refs {
			if artifactKeepsReference(art, ref, comp.Name) {
				n++
			}
		}
		return n, nil
	}
	return 0, nil
}

// artifactKeepsReference asks whether the referring FIELD, as the artifact
// describes it, still points at this component. For an embedding field that is
// just "does the field survive". For a zone it is narrower: a field that
// survives with the component dropped from its allowed list is no longer a
// reference at all, and the OpUpdateField that drops it is ordered BEFORE the
// component delete (DiffSchemas appends component deletes last), so by the time
// the delete runs the reference is gone. Counting it would refuse an artifact
// that is internally consistent.
func artifactKeepsReference(art domain.Artifact, ref repository.ComponentRef, componentName string) bool {
	for _, t := range art.Types {
		if t.Name != ref.TypeName {
			continue
		}
		f, ok := fieldOf(t, ref.FieldKey)
		if !ok {
			return false
		}
		if !ref.Zone {
			return true
		}
		for _, name := range f.Components {
			if name == componentName {
				return true
			}
		}
		return false
	}
	return false
}

// blockingCount asks the database the one question that decides a guarded
// change, using the SAME repository call the verb's own guard uses. A second
// counting rule here would be a plan that disagrees with the apply it predicts.
func (s *contentService) blockingCount(ctx context.Context, tenantID string, ct *domain.ContentType, c domain.SchemaChange) (int, error) {
	if ct == nil {
		// A guarded change against a type that does not exist yet cannot be
		// blocked by data that cannot exist.
		return 0, nil
	}
	switch c.Code {
	case "CONTENT_FIELD_REQUIRED_BACKFILL":
		return s.repo.CountEntriesMissingField(ctx, tenantID, ct.ID, c.Field)
	case "CONTENT_ENUM_VALUE_IN_USE":
		f, ok := ct.FieldByKey(c.Field)
		if !ok {
			return 0, nil
		}
		return s.repo.CountEntriesWithValuesOutside(ctx, tenantID, ct.ID, f, allowedFor(c, f))
	case "CONTENT_FIELD_UNIQUE_DUPLICATES":
		f, ok := ct.FieldByKey(c.Field)
		if !ok {
			return 0, nil
		}
		dups, err := s.repo.ListDuplicateFieldValues(ctx, tenantID, ct.ID, f)
		return len(dups), err
	case "CONTENT_FIELD_CONSTRAINT_BACKFILL":
		f, ok := ct.FieldByKey(c.Field)
		if !ok || c.Constraints == nil {
			return 0, nil
		}
		f.FieldConstraints = *c.Constraints
		return s.countConstraintViolations(ctx, tenantID, ct.ID, f)
	case "CONTENT_FIELD_HAS_DATA":
		return s.repo.CountEntriesWithField(ctx, tenantID, ct.ID, c.Field)
	case "CONTENT_ZONE_COMPONENT_IN_USE":
		// Counted per REMOVED name and summed, not counted once: the question
		// the verb answers is "may this name leave the list", and a plan that
		// reported only the first offender would under-report the work an
		// editor has to do before the artifact applies.
		n := 0
		for _, name := range c.RemovedComponents {
			k, err := s.repo.CountEntriesWithZoneComponent(ctx, tenantID,
				repository.ComponentRef{TypeID: ct.ID, TypeName: ct.Name, FieldKey: c.Field, Zone: true}, name)
			if err != nil {
				return 0, err
			}
			n += k
		}
		return n, nil
	case "CONTENT_ENTRY_AUTHOR_MISSING":
		return s.repo.CountEntriesWithoutAuthor(ctx, tenantID, ct.ID)
	case "CONTENT_TYPE_HAS_ENTRIES|CONTENT_TYPE_REFERENCED":
		n, err := s.repo.CountEntriesForType(ctx, tenantID, ct.ID)
		if err != nil || n > 0 {
			return n, err
		}
		refs, err := s.repo.ListRelationReferrers(ctx, tenantID, ct.Name)
		return len(refs), err
	}
	return 0, nil
}

// allowedFor recovers the target enum set for a narrowing step. The diff states
// what is REMOVED rather than what remains, so the surviving set is the live
// field's values minus those.
func allowedFor(c domain.SchemaChange, f domain.Field) []string {
	gone := map[string]struct{}{}
	for _, v := range c.RemovedValues {
		gone[v] = struct{}{}
	}
	out := make([]string, 0, len(f.EnumValues))
	for _, v := range f.EnumValues {
		if _, drop := gone[v]; !drop {
			out = append(out, v)
		}
	}
	return out
}

// ApplySchema executes a plan, in ONE transaction.
//
// Whole or nothing. A half-applied schema is the state ADR-007 exists to
// prevent: a type that gained a required field while its entries went
// un-migrated has no PATCHable entry left, and the error names a field the
// caller never touched. Compensating afterwards is not available either — the
// reverse of a schema change is another data migration, which is what ADR-007
// spends its ruling table refusing.
//
// The plan is recomputed INSIDE the transaction and re-checked. A plan built
// outside it describes a schema that another writer may already have moved;
// schema editing is last-write-wins (ADR-007's open item), so the window is
// closed here rather than left as a race the caller cannot see.
func (s *contentService) ApplySchema(ctx context.Context, art domain.Artifact, prune bool) (_ PlanResult, err error) {
	// One apply produces N+1 activity rows: this one, plus one per verb execute
	// drives. That is deliberate. The apply row alone can name no type (a
	// whole-schema artifact concerns them all), so it cannot answer "which
	// collection changed" — the per-verb rows can, and they are the ones §2's
	// stream needs. They also inherit the transaction: execute runs against a
	// tx-bound repository, so an apply that rolls back takes its rows with it,
	// while this row lands afterwards saying it was refused.
	act := s.activityWrite(ctx, domain.ActivitySchemaApply, "")
	defer func() { act.finish(ctx, err) }()
	sub, err := s.authorize(ctx, ActionContentSchemaWrite, "collection", "")
	if err != nil {
		return PlanResult{}, err
	}
	var result PlanResult
	err = s.repo.WithTx(ctx, sub.TenantID, func(r repository.ContentRepository) error {
		bound := *s
		bound.repo = r
		plan, err := bound.planWith(ctx, sub, art, prune)
		if err != nil {
			return err
		}
		if !plan.canApply() {
			return apperrors.New("CONTENT_SCHEMA_NOT_APPLICABLE", "the artifact cannot be applied as-is", 409).
				WithDetails(map[string]any{"refused": plan.Refused, "blocked": plan.Blocked, "steps": plan.Steps})
		}
		if err := bound.execute(ctx, art, plan); err != nil {
			return err
		}
		result = plan
		return nil
	})
	if err != nil {
		return PlanResult{}, err
	}
	return result, nil
}

// execute runs each step through the verb that owns it. Nothing here writes to
// the repository directly.
func (s *contentService) execute(ctx context.Context, art domain.Artifact, plan PlanResult) error {
	types := make(map[string]domain.ArtifactType, len(art.Types))
	for _, t := range art.Types {
		types[t.Name] = t
	}
	comps := make(map[string]domain.ArtifactComponent, len(art.Components))
	for _, c := range art.Components {
		comps[c.Name] = c
	}
	for _, step := range plan.Steps {
		if step.Skipped {
			continue
		}
		if step.Component != "" {
			if err := s.executeComponentStep(ctx, comps[step.Component], step); err != nil {
				return err
			}
			continue
		}
		t := types[step.Type]
		var err error
		switch step.Op {
		case domain.OpCreateType:
			err = s.createFromArtifact(ctx, t)
		case domain.OpUpdateType:
			// The permission lists are sent ALWAYS, including when the artifact
			// omits them — the same rule OpUpdateField follows, and it is the rule
			// that makes an artifact a state rather than a delta. Sending them only
			// when non-empty would make a file that revokes a grant, by deleting the
			// key, apply as a no-op: the one direction of this feature that must
			// never silently fail.
			readRoles, writeRoles, ownOnly := t.ReadRoles, t.WriteRoles, t.OwnOnlyRoles
			_, err = s.UpdateContentType(ctx, t.Name, UpdateTypeInput{
				Label:     t.Label,
				ReadRoles: &readRoles, WriteRoles: &writeRoles, OwnOnlyRoles: &ownOnly,
			})
		case domain.OpAddField:
			f, ok := fieldOf(t, step.Field)
			if !ok {
				continue
			}
			_, err = s.AddField(ctx, t.Name, fieldInput(f))
		case domain.OpUpdateField:
			f, ok := fieldOf(t, step.Field)
			if !ok {
				continue
			}
			label, required, enums := f.Label, f.Required, f.EnumValues
			readRoles, writeRoles := f.ReadRoles, f.WriteRoles
			// The permission lists are sent ALWAYS, including when the artifact
			// omits them — an omitted list means unrestricted, and an artifact
			// describes states, not deltas. Sending them only when non-empty would
			// make a file that revokes a grant (by deleting the key) apply as a
			// no-op, which is the one direction of this feature that must never
			// silently fail.
			unique, format, pattern := f.Unique, f.Format, f.Pattern
			// description travels with label, and is sent ALWAYS for the reason
			// the lists are: a file that deletes the sentence means "no
			// description", not "leave the old one".
			description := f.Description
			in := UpdateFieldInput{
				Label: &label, Required: &required, Description: &description,
				ReadRoles: &readRoles, WriteRoles: &writeRoles,
				// Constraints are sent ALWAYS for the reason the permission
				// lists are: an artifact describes states, and a file that drops
				// `pattern` means "no pattern", not "leave it".
				Unique: &unique, Format: &format, Pattern: &pattern,
				Min: OptionalNumber{Set: true, Value: f.Min},
				Max: OptionalNumber{Set: true, Value: f.Max},
			}
			// enum_values is only meaningful on an enum field, and sending it
			// elsewhere is its own named refusal.
			if f.Type == domain.FieldTypeEnum {
				in.EnumValues = &enums
			}
			// components is only meaningful on a dynamic zone, and sending it
			// elsewhere is its own named refusal — the same rule enum_values
			// follows one line up. Sent ALWAYS for a zone, so an artifact that
			// shortens the list applies as the removal it describes rather than
			// as a no-op.
			if f.Type == domain.FieldTypeDynamicZone {
				comps := f.Components
				in.Components = &comps
			}
			_, err = s.UpdateField(ctx, t.Name, f.Key, in)
		case domain.OpDeleteField:
			// force is set because prune already said so, ONCE, at the top
			// level. Per-field prompting inside a batch is how a caller learns
			// to answer yes without reading.
			_, err = s.DeleteField(ctx, step.Type, step.Field, true)
		case domain.OpDeleteType:
			err = s.DeleteContentType(ctx, step.Type)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// executeComponentStep runs one component step through the component verbs,
// the way execute runs a type step through the type verbs: the artifact is a
// second spelling of the API, never a second implementation of its guards.
func (s *contentService) executeComponentStep(ctx context.Context, c domain.ArtifactComponent, step PlanStep) error {
	var err error
	switch step.Op {
	case domain.OpCreateComponent:
		in := CreateComponentInput{Name: c.Name, Label: c.Label}
		for _, f := range c.Fields {
			in.Fields = append(in.Fields, fieldInput(f))
		}
		_, err = s.CreateComponent(ctx, in)
	case domain.OpUpdateComponent:
		label := c.Label
		_, err = s.UpdateComponent(ctx, c.Name, UpdateComponentInput{Label: &label})
	case domain.OpAddComponentField:
		f, ok := fieldOf(domain.ArtifactType{Fields: c.Fields}, step.Field)
		if !ok {
			return nil
		}
		_, err = s.AddComponentField(ctx, c.Name, fieldInput(f))
	case domain.OpUpdateComponentField:
		f, ok := fieldOf(domain.ArtifactType{Fields: c.Fields}, step.Field)
		if !ok {
			return nil
		}
		// No unique / read_roles / write_roles here: a sub-field cannot carry
		// them (ADR-020 §2) and the verb refuses their PRESENCE, so the
		// artifact must not even send the zero value.
		label, required, enums := f.Label, f.Required, f.EnumValues
		format, pattern := f.Format, f.Pattern
		description := f.Description
		in := UpdateFieldInput{
			Label: &label, Required: &required, Description: &description,
			Format: &format, Pattern: &pattern,
			Min: OptionalNumber{Set: true, Value: f.Min},
			Max: OptionalNumber{Set: true, Value: f.Max},
		}
		if f.Type == domain.FieldTypeEnum {
			in.EnumValues = &enums
		}
		_, err = s.UpdateComponentField(ctx, c.Name, f.Key, in)
	case domain.OpDeleteComponentField:
		_, err = s.DeleteComponentField(ctx, step.Component, step.Field, true)
	case domain.OpDeleteComponent:
		err = s.DeleteComponent(ctx, step.Component)
	}
	return err
}

func (s *contentService) createFromArtifact(ctx context.Context, t domain.ArtifactType) error {
	in := CreateTypeInput{
		Name: t.Name, Label: t.Label,
		ReadRoles: t.ReadRoles, WriteRoles: t.WriteRoles, OwnOnlyRoles: t.OwnOnlyRoles,
	}
	for _, f := range t.Fields {
		in.Fields = append(in.Fields, fieldInput(f))
	}
	_, err := s.CreateContentType(ctx, in)
	return err
}

func fieldOf(t domain.ArtifactType, key string) (domain.ArtifactField, bool) {
	for _, f := range t.Fields {
		if f.Key == key {
			return f, true
		}
	}
	return domain.ArtifactField{}, false
}

func fieldInput(f domain.ArtifactField) FieldInput {
	return FieldInput{
		Key: f.Key, Type: f.Type, Label: f.Label,
		Required: f.Required, Multiple: f.Multiple,
		EnumValues: f.EnumValues,
		ReadRoles:  f.ReadRoles, WriteRoles: f.WriteRoles,
		RelationEntity:   f.RelationEntity,
		Component:        f.Component,
		Components:       f.Components,
		Description:      f.Description,
		FieldConstraints: f.FieldConstraints,
	}
}
