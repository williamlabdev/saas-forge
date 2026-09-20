package service

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/cms/content/repository"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// Reusable components (ADR-020).
//
// A component is a named group of fields a content type field of type
// `component` takes its shape from; the value lives inline in the entry.
// The verbs here are the component-side twins of the type-side schema verbs,
// and every rule they apply is stated once in that file's terms:
//
//   - the definition-time checks are buildField's, plus the sub-field
//     refusals (nesting, unique, roles) domain.CheckComponentSubField holds;
//   - the data guards (required backfill, enum in use, constraint backfill,
//     has-data) are the type-side guards fanned out over EVERY referring
//     type, because a sub-field change reaches every item of every entry
//     that carries the component;
//   - rename and delete rewrite the inline values in the same transaction
//     as the definition change — the repository does it set-wise, with the
//     same version / provenance / revision rules as RenameField and
//     DeleteField.
//
// Audience: the component endpoints are admin surfaces. They name no
// content type, so an agent credential is refused every WRITE by
// construction (ADR-013 §4, the untyped rule). Reads are the one widening,
// and it is gated by what the component is USED BY: an agent may read a
// component when at least one type in its whitelist references it, and the
// used_by it sees is narrowed to that whitelist — the other referrers are
// somebody else's scope. A delivery credential is refused both, as it is
// refused the schema export: a component is schema, not published content.

// CreateComponentInput is the body of POST /components.
type CreateComponentInput struct {
	Name   string       `json:"name"`
	Label  string       `json:"label"`
	Fields []FieldInput `json:"fields"`
}

// UpdateComponentInput is the body of PATCH /components/{name}: label only.
// The rename is its own verb (POST /components/{name}/rename), for the reason
// every other rename in this package is: a routine label edit must not be able
// to rewrite stored documents because someone put the wrong string in a field.
type UpdateComponentInput struct {
	Label *string `json:"label"`
}

// ComponentDTO is the wire shape of a component. UsedBy lists the content
// types (names, sorted, distinct) with a field of this component — for the
// caller that is about to delete it, and for the agent gate above.
type ComponentDTO struct {
	Name      string     `json:"name"`
	Label     string     `json:"label"`
	Fields    []FieldDTO `json:"fields"`
	UsedBy    []string   `json:"used_by"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// ComponentTemplateDTO is the wire shape of a built-in component template
// (ADR-020 Amendment 2). Fields are FieldDTO-shaped — the same projection
// GET /components/{name} uses — so a client renders a template preview with
// the code it already has for a real component's fields.
type ComponentTemplateDTO struct {
	Name        string     `json:"name"`
	Label       string     `json:"label"`
	Description string     `json:"description,omitempty"`
	Fields      []FieldDTO `json:"fields"`
}

func (s *contentService) CreateComponent(ctx context.Context, in CreateComponentInput) (_ ComponentDTO, err error) {
	act := s.activityWrite(ctx, domain.ActivitySchemaWrite, "")
	defer func() { act.finish(ctx, err) }()
	sub, err := s.authorize(ctx, ActionContentCreate, "collection", "")
	if err != nil {
		return ComponentDTO{}, err
	}
	return s.createComponentAs(ctx, sub, in)
}

// createComponentAs is CreateComponent's body once the caller is authorized —
// factored out so InstallComponentTemplate (component_template.go) can run a
// built-in template through the SAME definition, validation, quota and
// persistence path a hand-authored POST /components takes, under the ONE
// activity record and authorize() call its own wrapper already did. Calling
// the exported CreateComponent instead would work too, but would double both.
func (s *contentService) createComponentAs(ctx context.Context, sub authn.Subject, in CreateComponentInput) (ComponentDTO, error) {
	name := strings.TrimSpace(in.Name)
	if err := validateComponentName(name); err != nil {
		return ComponentDTO{}, err
	}
	if len(in.Fields) == 0 {
		return ComponentDTO{}, errComponentNoFields(name)
	}
	if len(in.Fields) > domain.MaxComponentFields {
		return ComponentDTO{}, ErrQuotaExceeded.WithDetails(map[string]any{
			"resource": "component_fields", "limit": domain.MaxComponentFields,
		})
	}
	count, err := s.repo.CountComponents(ctx, sub.TenantID)
	if err != nil {
		return ComponentDTO{}, err
	}
	if count >= domain.MaxComponentsPerTenant {
		return ComponentDTO{}, ErrQuotaExceeded.WithDetails(map[string]any{
			"resource": "components", "limit": domain.MaxComponentsPerTenant,
		})
	}
	now := time.Now().UTC()
	c := &domain.Component{
		ID:        uuid.New(),
		TenantID:  sub.TenantID,
		Name:      name,
		Label:     strings.TrimSpace(in.Label),
		CreatedAt: now,
		UpdatedAt: now,
	}
	seen := map[string]bool{}
	for _, fi := range in.Fields {
		f, err := buildComponentField(fi, now)
		if err != nil {
			return ComponentDTO{}, err
		}
		if seen[f.Key] {
			return ComponentDTO{}, apperrors.New("CONTENT_FIELD_DUPLICATE", "duplicate field key", 422).
				WithDetails(map[string]any{"field": f.Key})
		}
		seen[f.Key] = true
		c.Fields = append(c.Fields, f)
	}
	if err := s.repo.CreateComponent(ctx, c); err != nil {
		return ComponentDTO{}, err
	}
	return toComponentDTO(c, nil, sub), nil
}

func (s *contentService) ListComponents(ctx context.Context) (_ []ComponentDTO, err error) {
	act := s.activityRead(ctx, domain.ActivityTypeList, "")
	defer func() { act.finish(ctx, err) }()
	sub, err := s.authorizeComponentRead(ctx, ActionContentList)
	if err != nil {
		return nil, err
	}
	cs, err := s.repo.ListComponents(ctx, sub.TenantID)
	if err != nil {
		return nil, err
	}
	out := make([]ComponentDTO, 0, len(cs))
	for _, c := range cs {
		refs, err := s.repo.ListComponentReferrers(ctx, sub.TenantID, c.ID)
		if err != nil {
			return nil, err
		}
		// An agent's list is the components it may GET, and nothing else: a
		// name it cannot read is a fact about another scope.
		if sub.IsAgent() && refuseComponentOutsideAgentScope(sub, refs) != nil {
			continue
		}
		out = append(out, toComponentDTO(c, refs, sub))
	}
	return out, nil
}

// ListComponentTemplates lists the built-in component templates (ADR-020
// Amendment 2). Same authorization as ListComponents — templates are schema
// catalog, not tenant data, so there is no per-template referrer scope to
// narrow an agent's view by; every audience that may list components sees
// every template.
func (s *contentService) ListComponentTemplates(ctx context.Context) (_ []ComponentTemplateDTO, err error) {
	act := s.activityRead(ctx, domain.ActivityTypeList, "")
	defer func() { act.finish(ctx, err) }()
	sub, err := s.authorizeComponentRead(ctx, ActionContentList)
	if err != nil {
		return nil, err
	}
	templates := domain.ComponentTemplates()
	out := make([]ComponentTemplateDTO, len(templates))
	for i, t := range templates {
		out[i] = toComponentTemplateDTO(t, sub)
	}
	return out, nil
}

// InstallComponentTemplate creates a real, tenant-owned Component from a
// built-in template (ADR-020 Amendment 2): same authorization as
// CreateComponent, and the SAME createComponentAs path, so validation, quota,
// audit and any future outbox event are exactly what a hand-authored
// POST /components with the template's fields would produce. A duplicate name
// surfaces the same CONTENT_COMPONENT_EXISTS a second POST /components would.
func (s *contentService) InstallComponentTemplate(ctx context.Context, name string) (_ ComponentDTO, err error) {
	act := s.activityWrite(ctx, domain.ActivitySchemaWrite, "")
	defer func() { act.finish(ctx, err) }()
	sub, err := s.authorize(ctx, ActionContentCreate, "collection", "")
	if err != nil {
		return ComponentDTO{}, err
	}
	tmpl, ok := domain.ComponentTemplateByName(name)
	if !ok {
		return ComponentDTO{}, errComponentTemplateNotFound(name)
	}
	return s.createComponentAs(ctx, sub, CreateComponentInput{
		Name:   tmpl.Name,
		Label:  tmpl.Label,
		Fields: templateFieldInputs(tmpl.Fields),
	})
}

func (s *contentService) GetComponent(ctx context.Context, name string) (_ ComponentDTO, err error) {
	act := s.activityRead(ctx, domain.ActivityTypeRead, "")
	defer func() { act.finish(ctx, err) }()
	sub, err := s.authorizeComponentRead(ctx, ActionContentRead)
	if err != nil {
		return ComponentDTO{}, err
	}
	c, refs, err := s.loadComponent(ctx, sub.TenantID, name)
	if err != nil {
		return ComponentDTO{}, err
	}
	if err := refuseComponentOutsideAgentScope(sub, refs); err != nil {
		return ComponentDTO{}, err
	}
	return toComponentDTO(c, refs, sub), nil
}

func (s *contentService) UpdateComponent(ctx context.Context, name string, in UpdateComponentInput) (_ ComponentDTO, err error) {
	act := s.activityWrite(ctx, domain.ActivitySchemaWrite, "")
	defer func() { act.finish(ctx, err) }()
	sub, err := s.authorize(ctx, ActionContentSchemaAmend, "collection", "")
	if err != nil {
		return ComponentDTO{}, err
	}
	c, refs, err := s.loadComponent(ctx, sub.TenantID, name)
	if err != nil {
		return ComponentDTO{}, err
	}
	if in.Label != nil {
		c.Label = strings.TrimSpace(*in.Label)
	}
	now := time.Now().UTC()
	if err := s.repo.UpdateComponentDefinition(ctx, sub.TenantID, c, now); err != nil {
		return ComponentDTO{}, err
	}
	c.UpdatedAt = now
	return toComponentDTO(c, refs, sub), nil
}

// RenameComponent moves a component's name, rewriting every place the OLD name
// is stored data rather than a reference.
//
// ADR-020 deferred this verb, and the reason it did no longer holds. Then, the
// name was referenced only through `ref_component_id`, so a rename touched one
// row and the only open question was how an artifact plan would tell a rename
// from a delete-and-recreate. A dynamic zone stores NAMES — in the field's
// allowed list, and in the `__component` of every item of every entry — so
// after Amendment 1 the name is content, and a tenant with no rename verb has
// no way to fix a typo in it at all.
//
// Three things move, in ONE transaction (the repository does it set-wise):
// every zone item's `__component`, in both payload and published_payload; every
// zone field's allowed list; and the component row. Entry REVISIONS are left
// alone, exactly as the sub-field rename leaves them — a revision is a record
// of what was written at the time, and rewriting history to match a later
// schema is the one thing an audit trail may not do.
func (s *contentService) RenameComponent(ctx context.Context, name string, in RenameInput) (_ ComponentDTO, err error) {
	act := s.activityWrite(ctx, domain.ActivitySchemaWrite, "")
	defer func() { act.finish(ctx, err) }()
	sub, err := s.authorize(ctx, ActionContentSchemaWrite, "collection", "")
	if err != nil {
		return ComponentDTO{}, err
	}
	newName := strings.TrimSpace(in.Name)
	if err := validateComponentName(newName); err != nil {
		return ComponentDTO{}, err
	}
	c, refs, err := s.loadComponent(ctx, sub.TenantID, name)
	if err != nil {
		return ComponentDTO{}, err
	}
	if newName == name {
		return toComponentDTO(c, refs, sub), nil
	}
	now := time.Now().UTC()
	if err := s.repo.RenameComponent(ctx, sub.TenantID, c, refs, newName, writeActorOf(sub), now); err != nil {
		return ComponentDTO{}, err
	}
	c.Name, c.UpdatedAt = newName, now
	return toComponentDTO(c, refs, sub), nil
}

func (s *contentService) DeleteComponent(ctx context.Context, name string) (err error) {
	act := s.activityWrite(ctx, domain.ActivitySchemaWrite, "")
	defer func() { act.finish(ctx, err) }()
	sub, err := s.authorize(ctx, ActionContentSchemaWrite, "collection", "")
	if err != nil {
		return err
	}
	c, refs, err := s.loadComponent(ctx, sub.TenantID, name)
	if err != nil {
		return err
	}
	if len(refs) > 0 {
		return errComponentInUse(name, usedBy(refs))
	}
	return s.repo.DeleteComponent(ctx, sub.TenantID, c.ID)
}

func (s *contentService) AddComponentField(ctx context.Context, name string, in FieldInput) (_ ComponentDTO, err error) {
	act := s.activityWrite(ctx, domain.ActivitySchemaWrite, "")
	defer func() { act.finish(ctx, err) }()
	sub, err := s.authorize(ctx, ActionContentSchemaAmend, "collection", "")
	if err != nil {
		return ComponentDTO{}, err
	}
	c, refs, err := s.loadComponent(ctx, sub.TenantID, name)
	if err != nil {
		return ComponentDTO{}, err
	}
	now := time.Now().UTC()
	f, err := buildComponentField(in, now)
	if err != nil {
		return ComponentDTO{}, err
	}
	if _, exists := c.FieldByKey(f.Key); exists {
		return ComponentDTO{}, apperrors.New("CONTENT_FIELD_EXISTS", "field already defined", 409).
			WithDetails(map[string]any{"field": f.Key})
	}
	if len(c.Fields) >= domain.MaxComponentFields {
		return ComponentDTO{}, ErrQuotaExceeded.WithDetails(map[string]any{
			"resource": "component_fields", "limit": domain.MaxComponentFields,
		})
	}
	// A required sub-field is a backfill across every referrer (ADR-007
	// Amendment 2): any item of any entry of any referring type that lacks
	// it would fail its next PATCH on a key the caller never sent.
	if f.Required {
		n, err := s.countComponentEntries(ctx, sub.TenantID, c.Name, refs, itemLacks(f.Key))
		if err != nil {
			return ComponentDTO{}, err
		}
		if n > 0 {
			return ComponentDTO{}, errRequiredBackfill(f.Key, n)
		}
	}
	if err := s.repo.AddComponentField(ctx, sub.TenantID, c.ID, &f); err != nil {
		return ComponentDTO{}, err
	}
	c.Fields = append(c.Fields, f)
	c.UpdatedAt = now
	return toComponentDTO(c, refs, sub), nil
}

func (s *contentService) UpdateComponentField(ctx context.Context, name, key string, in UpdateFieldInput) (_ ComponentDTO, err error) {
	act := s.activityWrite(ctx, domain.ActivitySchemaWrite, "")
	defer func() { act.finish(ctx, err) }()
	sub, err := s.authorize(ctx, ActionContentSchemaAmend, "collection", "")
	if err != nil {
		return ComponentDTO{}, err
	}
	if err := refuseImmutableFieldProps(key, in); err != nil {
		return ComponentDTO{}, err
	}
	// The attributes a sub-field cannot carry are refused on PRESENCE, as
	// UpdateField refuses the immutable ones: `"unique": false` is still a
	// request to decide something this field has no say in. `components` joins
	// them because a sub-field can never be a dynamic zone (nesting is refused,
	// ADR-020 Amendment 1), so the attribute has nothing it could apply to.
	for attr, sent := range map[string]bool{
		"unique": in.Unique != nil, "read_roles": in.ReadRoles != nil, "write_roles": in.WriteRoles != nil,
		"components": in.Components != nil,
	} {
		if sent {
			return ComponentDTO{}, errComponentSubField(key, &domain.ComponentSubFieldViolation{
				Code: "CONTENT_COMPONENT_FIELD_ATTRIBUTE_UNSUPPORTED", Attribute: attr,
				Detail: "component sub-fields cannot carry " + attr,
			})
		}
	}
	c, refs, err := s.loadComponent(ctx, sub.TenantID, name)
	if err != nil {
		return ComponentDTO{}, err
	}
	f, ok := c.FieldByKey(key)
	if !ok {
		return ComponentDTO{}, errFieldNotFound(key)
	}
	if in.Label != nil {
		f.Label = strings.TrimSpace(*in.Label)
	}
	if in.Description != nil {
		f.Description = strings.TrimSpace(*in.Description)
	}
	if in.EnumValues != nil {
		if f.Type != domain.FieldTypeEnum {
			return ComponentDTO{}, errEnumNotApplicable(key, f.Type)
		}
		vals := *in.EnumValues
		if len(vals) == 0 {
			return ComponentDTO{}, apperrors.New("CONTENT_FIELD_ENUM_EMPTY", "enum field needs enum_values", 422).
				WithDetails(map[string]any{"field": key})
		}
		if dup, ok := firstDuplicate(vals); ok {
			return ComponentDTO{}, errEnumDuplicate(key, dup)
		}
		if removed := missingFrom(f.EnumValues, vals); len(removed) > 0 {
			probe := f
			probe.EnumValues = vals
			n, err := s.countComponentEntries(ctx, sub.TenantID, c.Name, refs, itemFails(probe))
			if err != nil {
				return ComponentDTO{}, err
			}
			if n > 0 {
				return ComponentDTO{}, apperrors.New("CONTENT_ENUM_VALUE_IN_USE", "enum values are still in use", 409).
					WithDetails(map[string]any{"field": key, "values": removed, "entries": n})
			}
		}
		f.EnumValues = vals
	}
	if in.Required != nil {
		if *in.Required && !f.Required {
			n, err := s.countComponentEntries(ctx, sub.TenantID, c.Name, refs, itemLacks(key))
			if err != nil {
				return ComponentDTO{}, err
			}
			if n > 0 {
				return ComponentDTO{}, errRequiredBackfill(key, n)
			}
		}
		f.Required = *in.Required
	}
	if in.Format != nil || in.Pattern != nil || in.Min.Set || in.Max.Set {
		next := f.FieldConstraints
		if in.Format != nil {
			next.Format = *in.Format
		}
		if in.Pattern != nil {
			next.Pattern = *in.Pattern
		}
		if in.Min.Set {
			next.Min = in.Min.Value
		}
		if in.Max.Set {
			next.Max = in.Max.Value
		}
		next = normalizeConstraints(next)
		if cerr := domain.ValidateFieldConstraints(f.Type, f.Multiple, next); cerr != nil {
			return ComponentDTO{}, errConstraintDefinition(key, cerr)
		}
		if next.Tightens(f.FieldConstraints) {
			probe := f
			probe.FieldConstraints = next
			n, err := s.countComponentEntries(ctx, sub.TenantID, c.Name, refs, itemFails(probe))
			if err != nil {
				return ComponentDTO{}, err
			}
			if n > 0 {
				return ComponentDTO{}, apperrors.New("CONTENT_FIELD_CONSTRAINT_BACKFILL", "entries hold values that would fail the new constraints", 409).
					WithDetails(map[string]any{"field": key, "entries": n, "constraints": next.Describe()})
			}
		}
		f.FieldConstraints = next
	}
	now := time.Now().UTC()
	if err := s.repo.UpdateComponentFieldDefinition(ctx, sub.TenantID, c, f, now); err != nil {
		return ComponentDTO{}, err
	}
	return toComponentDTO(withComponentFieldAt(c, key, f, now), refs, sub), nil
}

func (s *contentService) RenameComponentField(ctx context.Context, name, key string, in RenameInput) (_ ComponentDTO, err error) {
	act := s.activityWrite(ctx, domain.ActivitySchemaWrite, "")
	defer func() { act.finish(ctx, err) }()
	sub, err := s.authorize(ctx, ActionContentSchemaWrite, "collection", "")
	if err != nil {
		return ComponentDTO{}, err
	}
	newKey := strings.TrimSpace(in.Key)
	if !domain.ValidFieldKey(newKey) {
		return ComponentDTO{}, apperrors.New("CONTENT_FIELD_KEY_INVALID", "invalid field key", 422).
			WithDetails(map[string]any{"field": in.Key})
	}
	c, refs, err := s.loadComponent(ctx, sub.TenantID, name)
	if err != nil {
		return ComponentDTO{}, err
	}
	f, ok := c.FieldByKey(key)
	if !ok {
		return ComponentDTO{}, errFieldNotFound(key)
	}
	if newKey == key {
		return toComponentDTO(c, refs, sub), nil
	}
	if _, taken := c.FieldByKey(newKey); taken {
		return ComponentDTO{}, apperrors.New("CONTENT_FIELD_EXISTS", "field already defined", 409).
			WithDetails(map[string]any{"field": newKey})
	}
	now := time.Now().UTC()
	if err := s.repo.RenameComponentField(ctx, sub.TenantID, c, refs, key, newKey, writeActorOf(sub), now); err != nil {
		return ComponentDTO{}, err
	}
	f.Key = newKey
	return toComponentDTO(withComponentFieldAt(c, key, f, now), refs, sub), nil
}

func (s *contentService) DeleteComponentField(ctx context.Context, name, key string, force bool) (_ ComponentDTO, err error) {
	act := s.activityWrite(ctx, domain.ActivitySchemaWrite, "")
	defer func() { act.finish(ctx, err) }()
	sub, err := s.authorize(ctx, ActionContentSchemaWrite, "collection", "")
	if err != nil {
		return ComponentDTO{}, err
	}
	c, refs, err := s.loadComponent(ctx, sub.TenantID, name)
	if err != nil {
		return ComponentDTO{}, err
	}
	if _, ok := c.FieldByKey(key); !ok {
		return ComponentDTO{}, errFieldNotFound(key)
	}
	if len(c.Fields) == 1 {
		return ComponentDTO{}, errComponentNoFields(name)
	}
	if !force {
		n := 0
		for _, ref := range refs {
			k, err := s.repo.CountEntriesWithComponentSubKey(ctx, sub.TenantID, ref, c.Name, key)
			if err != nil {
				return ComponentDTO{}, err
			}
			n += k
		}
		if n > 0 {
			return ComponentDTO{}, apperrors.New("CONTENT_FIELD_HAS_DATA", "field still holds data", 409).
				WithDetails(map[string]any{"field": key, "entries": n})
		}
	}
	now := time.Now().UTC()
	if err := s.repo.DeleteComponentField(ctx, sub.TenantID, c, refs, key, writeActorOf(sub), now); err != nil {
		return ComponentDTO{}, err
	}
	out := *c
	out.Fields = nil
	for _, x := range c.Fields {
		if x.Key != key {
			out.Fields = append(out.Fields, x)
		}
	}
	out.UpdatedAt = now
	return toComponentDTO(&out, refs, sub), nil
}

// --- helpers ----------------------------------------------------------------

// authorizeComponentRead is authorize() for the two component reads. The
// agent whitelist cannot be applied here — it is a function of the
// component's referrers, which are not loaded yet — so the scope closure
// only asks the credential questions and the caller applies
// refuseComponentOutsideAgentScope once it holds the refs. A delivery
// credential is refused outright, as ExportSchema refuses it.
func (s *contentService) authorizeComponentRead(ctx context.Context, action string) (authn.Subject, error) {
	sub, err := s.authorizeAgentScope(ctx, action, "collection", refuseUnscopedAgentCredential)
	if err != nil {
		return authn.Subject{}, err
	}
	if sub.PublicDelivery {
		return authn.Subject{}, apperrors.New("CONTENT_COMPONENT_ADMIN_ONLY", "components are an admin surface", 403)
	}
	return sub, nil
}

// refuseComponentOutsideAgentScope is the read gate for an agent credential:
// at least one referring type must be in its whitelist. The refusal names
// no referrer — those are the part of the answer the credential is not
// scoped to see.
func refuseComponentOutsideAgentScope(sub authn.Subject, refs []repository.ComponentRef) error {
	if !sub.IsAgent() {
		return nil
	}
	for _, ref := range refs {
		if sub.AllowsContentType(ref.TypeName) {
			return nil
		}
	}
	return apperrors.New(
		"CONTENT_AGENT_COMPONENT_NOT_ALLOWED",
		"this agent credential is not scoped to any content type using this component",
		403,
	)
}

func (s *contentService) loadComponent(ctx context.Context, tenantID, name string) (*domain.Component, []repository.ComponentRef, error) {
	c, err := s.repo.GetComponentByName(ctx, tenantID, name)
	if err != nil {
		return nil, nil, componentNotFound(err, name)
	}
	refs, err := s.repo.ListComponentReferrers(ctx, tenantID, c.ID)
	if err != nil {
		return nil, nil, err
	}
	return c, refs, nil
}

// buildComponentField is buildField for a sub-field: the ordinary checks
// first, then the sub-field refusals. Nesting is checked BEFORE buildField
// so a `component` sub-field is refused as nesting rather than as "component
// name missing", which would send the caller off to supply one.
func buildComponentField(in FieldInput, now time.Time) (domain.Field, error) {
	key := strings.TrimSpace(in.Key)
	if in.Type == domain.FieldTypeComponent {
		return domain.Field{}, errComponentSubField(key, domain.CheckComponentSubField(domain.Field{Type: in.Type}))
	}
	f, err := buildField(uuid.Nil, in, now)
	if err != nil {
		return domain.Field{}, err
	}
	if v := domain.CheckComponentSubField(f); v != nil {
		return domain.Field{}, errComponentSubField(f.Key, v)
	}
	return f, nil
}

// countComponentEntries counts, across every referrer, the entries in which
// ANY item of the component value — in the working copy or the live
// snapshot — satisfies pred. Each entry counts once. It is the sub-field
// twin of countConstraintViolations and runs on the same decoded shapes.
func (s *contentService) countComponentEntries(ctx context.Context, tenantID, componentName string,
	refs []repository.ComponentRef, pred func(item map[string]any) bool) (int, error) {
	n := 0
	for _, ref := range refs {
		field := domain.Field{Key: ref.FieldKey, Type: domain.FieldTypeComponent, Multiple: ref.Multiple}
		hit := func(v any) bool {
			// A zone holds items of SEVERAL components, so only the ones that
			// are this component are asked the question. Counting the others
			// would refuse a sub-field change because of content that has
			// nothing to do with the component being changed — the loudest
			// possible false positive on a schema verb.
			if ref.Zone {
				for _, item := range zoneItems(v) {
					if zoneItemComponent(item) == componentName && pred(item) {
						return true
					}
				}
				return false
			}
			for _, item := range componentItems(field, v) {
				if pred(item) {
					return true
				}
			}
			return false
		}
		err := s.repo.ScanFieldValues(ctx, tenantID, ref.TypeID, ref.FieldKey, func(_ uuid.UUID, working, live any) error {
			if hit(working) || hit(live) {
				n++
			}
			return nil
		})
		if err != nil {
			return 0, err
		}
	}
	return n, nil
}

// itemLacks is the predicate behind the required-backfill guard.
func itemLacks(key string) func(map[string]any) bool {
	return func(item map[string]any) bool {
		v, ok := item[key]
		return !ok || v == nil
	}
}

// itemFails is the predicate behind the enum-in-use and constraint-backfill
// guards: the item's value for f would not pass f as proposed. It runs the
// validator's own rule (validateValue) rather than a second spelling of it.
func itemFails(f domain.Field) func(map[string]any) bool {
	return func(item map[string]any) bool {
		v, ok := item[f.Key]
		if !ok || v == nil {
			return false
		}
		return validateValue(f, v) != nil
	}
}

func errRequiredBackfill(key string, n int) error {
	return apperrors.New("CONTENT_FIELD_REQUIRED_BACKFILL", "entries are missing this field", 409).
		WithDetails(map[string]any{"field": key, "entries": n})
}

// validateComponentName is the name check CreateComponent and RenameComponent
// share: a legal identifier that is also not the one name reserved for the
// GET/POST /components/templates route (ADR-020 Amendment 2) — chi resolves
// that static route ahead of /components/{name} regardless of registration
// order, so a component actually named "templates" would be permanently
// unreachable through GET/PATCH/DELETE /components/{name}.
func validateComponentName(name string) error {
	if !domain.ValidFieldKey(name) {
		return apperrors.New("CONTENT_COMPONENT_NAME_INVALID", "invalid component name", 422).
			WithDetails(map[string]any{"name": name})
	}
	if domain.ReservedComponentName(name) {
		return apperrors.New("CONTENT_COMPONENT_NAME_RESERVED", "component name is reserved for the templates route", 422).
			WithDetails(map[string]any{"name": name})
	}
	return nil
}

func errComponentNoFields(name string) error {
	return apperrors.New("CONTENT_COMPONENT_NO_FIELDS", "a component needs at least one field", 422).
		WithDetails(map[string]any{"component": name})
}

func errComponentInUse(name string, types []string) error {
	return apperrors.New("CONTENT_COMPONENT_IN_USE", "component is still used by content types", 409).
		WithDetails(map[string]any{"component": name, "used_by": types})
}

// usedBy reduces referrers to distinct, sorted type names.
func usedBy(refs []repository.ComponentRef) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, ref := range refs {
		if !seen[ref.TypeName] {
			seen[ref.TypeName] = true
			out = append(out, ref.TypeName)
		}
	}
	sort.Strings(out)
	return out
}

func withComponentFieldAt(c *domain.Component, atKey string, f domain.Field, now time.Time) *domain.Component {
	out := *c
	out.Fields = make([]domain.Field, len(c.Fields))
	copy(out.Fields, c.Fields)
	for i := range out.Fields {
		if out.Fields[i].Key == atKey {
			out.Fields[i] = f
		}
	}
	out.UpdatedAt = now
	return &out
}

// componentFieldToDTO renders one sub-field the way toComponentDTO and
// componentTemplateToDTO both need it: `supported` is always empty, because
// nothing under a component (built-in template or tenant-authored) is
// queryable (ADR-020 §3).
func componentFieldToDTO(f domain.Field, sub authn.Subject) FieldDTO {
	return FieldDTO{
		Readable:       canReadField(f, sub),
		Writable:       canWriteField(f, sub),
		Key:            f.Key,
		Type:           f.Type,
		Label:          f.Label,
		Required:       f.Required,
		Multiple:       f.Multiple,
		EnumValues:     f.EnumValues,
		ReadRoles:      f.ReadRoles,
		WriteRoles:     f.WriteRoles,
		Supported:      []repository.Op{},
		RelationEntity: f.RelationEntity,
		Description:    f.Description,
		Format:         f.Format,
		Pattern:        f.Pattern,
		Min:            f.Min,
		Max:            f.Max,
	}
}

// toComponentDTO renders a component for the caller. Sub-fields carry no
// permission lists, so readable/writable are the unrestricted answer for
// everyone who got this far; `supported` is empty because nothing under a
// component is queryable (ADR-020 §3). used_by is narrowed to the agent's
// whitelist when the caller is one.
func toComponentDTO(c *domain.Component, refs []repository.ComponentRef, sub authn.Subject) ComponentDTO {
	fields := make([]FieldDTO, len(c.Fields))
	for i, f := range c.Fields {
		fields[i] = componentFieldToDTO(f, sub)
	}
	visible := refs
	if sub.IsAgent() {
		visible = nil
		for _, ref := range refs {
			if sub.AllowsContentType(ref.TypeName) {
				visible = append(visible, ref)
			}
		}
	}
	return ComponentDTO{
		Name:      c.Name,
		Label:     c.Label,
		Fields:    fields,
		UsedBy:    usedBy(visible),
		CreatedAt: c.CreatedAt,
		UpdatedAt: c.UpdatedAt,
	}
}

// componentNotFound maps the repository's not-found onto the 404 the
// handler wants to say about a component rather than a generic resource.
func componentNotFound(err error, name string) error {
	if errors.Is(err, apperrors.ErrNotFound) {
		return apperrors.New("CONTENT_COMPONENT_NOT_FOUND", "component is not defined", 404).
			WithDetails(map[string]any{"component": name})
	}
	return err
}

// toComponentTemplateDTO renders a built-in template with the same FieldDTO
// projection GET /components/{name} uses. Templates carry no ReadRoles /
// WriteRoles, so Readable/Writable come back true for every subject —
// component fields becomes visible and settable exactly the way an
// unrestricted tenant-authored field is.
func toComponentTemplateDTO(t domain.ComponentTemplate, sub authn.Subject) ComponentTemplateDTO {
	fields := make([]FieldDTO, len(t.Fields))
	for i, f := range t.Fields {
		fields[i] = componentFieldToDTO(f, sub)
	}
	return ComponentTemplateDTO{
		Name:        t.Name,
		Label:       t.Label,
		Description: t.Description,
		Fields:      fields,
	}
}

// templateFieldInputs turns a template's domain.Field declarations into the
// FieldInput shape createComponentAs takes — the same conversion decodeJSON
// does for a POST /components body, just skipping the wire round-trip.
func templateFieldInputs(fields []domain.Field) []FieldInput {
	out := make([]FieldInput, len(fields))
	for i, f := range fields {
		out[i] = FieldInput{
			Key:              f.Key,
			Type:             f.Type,
			Label:            f.Label,
			Required:         f.Required,
			Multiple:         f.Multiple,
			EnumValues:       f.EnumValues,
			ReadRoles:        f.ReadRoles,
			WriteRoles:       f.WriteRoles,
			RelationEntity:   f.RelationEntity,
			Component:        f.ComponentName,
			Components:       f.ZoneComponents,
			Description:      f.Description,
			FieldConstraints: f.FieldConstraints,
		}
	}
	return out
}

// errComponentTemplateNotFound is the 404 InstallComponentTemplate returns
// for a template name that is not one of domain.ComponentTemplates() — its
// own code rather than CONTENT_COMPONENT_NOT_FOUND, because the two 404s name
// different catalogs (built-in templates vs. this tenant's components) and a
// caller who mixed them up needs to be told which one it was.
func errComponentTemplateNotFound(name string) error {
	return apperrors.New("CONTENT_COMPONENT_TEMPLATE_NOT_FOUND", "component template is not defined", 404).
		WithDetails(map[string]any{"template": name})
}
