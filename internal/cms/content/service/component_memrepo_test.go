package service

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/cms/content/repository"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// memRepo's component half (ADR-020). Two things Postgres does are mirrored
// here on purpose, because a fake without them would make the service tests
// ratify the wrong thing:
//
//   - a referring type's Field.ComponentFields is a COPY attached at load, so
//     every component mutation re-attaches it (syncComponentFields) the way a
//     fresh loadFieldsByType would;
//   - a sub-field rename / delete rewrites the ITEMS of every referrer's entries
//     in both copies, through rewriteEntryDocs, so the service-level tests see
//     the same shape the set-based SQL produces.

func (m *memRepo) CreateComponent(_ context.Context, c *domain.Component) error {
	for _, have := range m.components {
		if have.TenantID == c.TenantID && have.Name == c.Name {
			return apperrors.New("CONTENT_COMPONENT_EXISTS", "exists", 409)
		}
	}
	cp := *c
	cp.Fields = append([]domain.Field(nil), c.Fields...)
	m.components = append(m.components, &cp)
	return nil
}

func (m *memRepo) GetComponentByName(_ context.Context, tenantID, name string) (*domain.Component, error) {
	for _, c := range m.components {
		if c.TenantID == tenantID && c.Name == name {
			return copyComponent(c), nil
		}
	}
	return nil, apperrors.ErrNotFound
}

func (m *memRepo) GetComponentByID(_ context.Context, tenantID string, id uuid.UUID) (*domain.Component, error) {
	c := m.findComponent(tenantID, id)
	if c == nil {
		return nil, apperrors.ErrNotFound
	}
	return copyComponent(c), nil
}

func (m *memRepo) ListComponents(_ context.Context, tenantID string) ([]*domain.Component, error) {
	var out []*domain.Component
	for _, c := range m.components {
		if c.TenantID == tenantID {
			out = append(out, copyComponent(c))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *memRepo) CountComponents(_ context.Context, tenantID string) (int, error) {
	n := 0
	for _, c := range m.components {
		if c.TenantID == tenantID {
			n++
		}
	}
	return n, nil
}

func (m *memRepo) UpdateComponentDefinition(_ context.Context, tenantID string, in *domain.Component, now time.Time) error {
	c := m.findComponent(tenantID, in.ID)
	if c == nil {
		return apperrors.ErrNotFound
	}
	c.Label, c.UpdatedAt = in.Label, now
	return nil
}

func (m *memRepo) DeleteComponent(_ context.Context, tenantID string, id uuid.UUID) error {
	for i, c := range m.components {
		if c.TenantID == tenantID && c.ID == id {
			m.components = append(m.components[:i], m.components[i+1:]...)
			return nil
		}
	}
	return apperrors.ErrNotFound
}

func (m *memRepo) ListComponentReferrers(_ context.Context, tenantID string, componentID uuid.UUID) ([]repository.ComponentRef, error) {
	var out []repository.ComponentRef
	for _, t := range m.types {
		if t.TenantID != tenantID {
			continue
		}
		for _, f := range t.Fields {
			switch {
			case f.Type == domain.FieldTypeComponent && f.ComponentID != nil && *f.ComponentID == componentID:
				out = append(out, repository.ComponentRef{TypeID: t.ID, TypeName: t.Name, FieldKey: f.Key, Multiple: f.Multiple})
			case f.Type == domain.FieldTypeDynamicZone && m.zoneAllows(tenantID, f, componentID):
				// Zone referrers come back from the SAME call, marked, because
				// that is what the UNION in postgres_components.go produces —
				// and it is what makes the component-delete refusal cover zones
				// without a second list to keep in step.
				out = append(out, repository.ComponentRef{TypeID: t.ID, TypeName: t.Name, FieldKey: f.Key, Multiple: f.Multiple, Zone: true})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TypeName != out[j].TypeName {
			return out[i].TypeName < out[j].TypeName
		}
		return out[i].FieldKey < out[j].FieldKey
	})
	return out, nil
}

func (m *memRepo) AddComponentField(_ context.Context, tenantID string, componentID uuid.UUID, f *domain.Field) error {
	c := m.findComponent(tenantID, componentID)
	if c == nil {
		return apperrors.ErrNotFound
	}
	for _, have := range c.Fields {
		if have.Key == f.Key {
			return apperrors.New("CONTENT_FIELD_EXISTS", "exists", 409)
		}
	}
	c.Fields = append(c.Fields, *f)
	c.UpdatedAt = f.CreatedAt
	m.syncComponentFields(c)
	return nil
}

func (m *memRepo) UpdateComponentFieldDefinition(_ context.Context, tenantID string, in *domain.Component, f domain.Field, now time.Time) error {
	c := m.findComponent(tenantID, in.ID)
	if c == nil {
		return apperrors.ErrNotFound
	}
	for i := range c.Fields {
		if c.Fields[i].Key == f.Key {
			c.Fields[i] = f
			c.UpdatedAt = now
			m.syncComponentFields(c)
			return nil
		}
	}
	return apperrors.ErrNotFound
}

func (m *memRepo) RenameComponentField(_ context.Context, tenantID string, in *domain.Component, refs []repository.ComponentRef,
	oldKey, newKey string, actor domain.WriteActor, now time.Time) error {
	c := m.findComponent(tenantID, in.ID)
	if c == nil {
		return apperrors.ErrNotFound
	}
	for _, ref := range refs {
		m.rewriteEntryDocs(tenantID, ref.TypeID, rewriteItems(ref, c.Name, func(item map[string]any) bool {
			v, ok := item[oldKey]
			if !ok {
				return false
			}
			delete(item, oldKey)
			item[newKey] = v
			return true
		}), actor, now)
		m.touchType(ref.TypeID, now)
	}
	for i := range c.Fields {
		if c.Fields[i].Key == oldKey {
			c.Fields[i].Key = newKey
		}
	}
	c.UpdatedAt = now
	m.syncComponentFields(c)
	return nil
}

func (m *memRepo) DeleteComponentField(_ context.Context, tenantID string, in *domain.Component, refs []repository.ComponentRef,
	key string, actor domain.WriteActor, now time.Time) error {
	c := m.findComponent(tenantID, in.ID)
	if c == nil {
		return apperrors.ErrNotFound
	}
	for _, ref := range refs {
		m.rewriteEntryDocs(tenantID, ref.TypeID, rewriteItems(ref, c.Name, func(item map[string]any) bool {
			if _, ok := item[key]; !ok {
				return false
			}
			delete(item, key)
			return true
		}), actor, now)
		m.touchType(ref.TypeID, now)
	}
	for i := range c.Fields {
		if c.Fields[i].Key == key {
			c.Fields = append(c.Fields[:i], c.Fields[i+1:]...)
			break
		}
	}
	c.UpdatedAt = now
	m.syncComponentFields(c)
	return nil
}

func (m *memRepo) CountEntriesWithComponentSubKey(_ context.Context, tenantID string, ref repository.ComponentRef,
	componentName, subKey string) (int, error) {
	return m.countItems(tenantID, ref, componentName, func(item map[string]any) bool {
		_, ok := item[subKey]
		return ok
	}), nil
}

func (m *memRepo) CountEntriesWithZoneComponent(_ context.Context, tenantID string, ref repository.ComponentRef,
	componentName string) (int, error) {
	return m.countItems(tenantID, ref, componentName, func(map[string]any) bool { return true }), nil
}

// countItems counts ENTRIES (not items) of the referrer whose working copy or
// live snapshot holds an item of componentName satisfying pred — the shape
// both count queries in postgres_components.go produce.
func (m *memRepo) countItems(tenantID string, ref repository.ComponentRef, componentName string,
	pred func(map[string]any) bool) int {
	n := 0
	for _, docs := range m.entryPayloads(tenantID, ref.TypeID) {
		hit := false
		probe := rewriteItems(ref, componentName, func(item map[string]any) bool {
			if pred(item) {
				hit = true
			}
			return false
		})
		for _, doc := range docs {
			probe(doc)
		}
		if hit {
			n++
		}
	}
	return n
}

func (m *memRepo) RenameComponent(_ context.Context, tenantID string, in *domain.Component, refs []repository.ComponentRef,
	newName string, actor domain.WriteActor, now time.Time) error {
	c := m.findComponent(tenantID, in.ID)
	if c == nil {
		return apperrors.ErrNotFound
	}
	for _, have := range m.components {
		if have.TenantID == tenantID && have.Name == newName {
			return apperrors.New("CONTENT_COMPONENT_EXISTS", "exists", 409)
		}
	}
	old := c.Name
	// The discriminator inside every zone item, then the allowed list on every
	// zone field: both are stored data spelling the old name, and a rename that
	// moved only one of them would leave a payload the validator refuses.
	for _, ref := range refs {
		if !ref.Zone {
			continue
		}
		m.rewriteEntryDocs(tenantID, ref.TypeID, rewriteItems(ref, old, func(item map[string]any) bool {
			item[domain.ZoneDiscriminator] = newName
			return true
		}), actor, now)
		m.touchType(ref.TypeID, now)
	}
	for _, t := range m.types {
		if t.TenantID != tenantID {
			continue
		}
		for i := range t.Fields {
			f := &t.Fields[i]
			if f.Type != domain.FieldTypeDynamicZone {
				continue
			}
			for j, name := range f.ZoneComponents {
				if name == old {
					f.ZoneComponents[j] = newName
				}
			}
		}
	}
	c.Name, c.UpdatedAt = newName, now
	m.syncComponentFields(c)
	return nil
}

// zoneAllows reports whether f's allowed list names the component with this id.
func (m *memRepo) zoneAllows(tenantID string, f domain.Field, componentID uuid.UUID) bool {
	c := m.findComponent(tenantID, componentID)
	if c == nil {
		return false
	}
	for _, name := range f.ZoneComponents {
		if name == c.Name {
			return true
		}
	}
	return false
}

// rewriteItems lifts a per-item mutation to a per-document one. For a component
// field the value under the key is one object or an array of them (ADR-020 §4);
// for a zone it is always an array, and only the items whose __component is
// this component are touched — a zone item of ANOTHER component has its own
// sub-fields and must not be rewritten by this one's rename.
func rewriteItems(ref repository.ComponentRef, componentName string, mutate func(map[string]any) bool) func(map[string]any) bool {
	return func(doc map[string]any) bool {
		changed := false
		switch v := doc[ref.FieldKey].(type) {
		case map[string]any:
			if !ref.Zone {
				changed = mutate(v)
			}
		case []any:
			for _, it := range v {
				item, ok := it.(map[string]any)
				if !ok {
					continue
				}
				if ref.Zone {
					if name, _ := item[domain.ZoneDiscriminator].(string); name != componentName {
						continue
					}
				}
				if mutate(item) {
					changed = true
				}
			}
		}
		return changed
	}
}

func (m *memRepo) findComponent(tenantID string, id uuid.UUID) *domain.Component {
	for _, c := range m.components {
		if c.TenantID == tenantID && c.ID == id {
			return c
		}
	}
	return nil
}

func (m *memRepo) touchType(id uuid.UUID, now time.Time) {
	if t := m.findType(id); t != nil {
		t.UpdatedAt = now
	}
}

// syncComponentFields re-attaches the component's sub-fields to every field
// that embeds it, which is what loading the type afresh would do.
func (m *memRepo) syncComponentFields(c *domain.Component) {
	for _, t := range m.types {
		for i := range t.Fields {
			f := &t.Fields[i]
			switch {
			case f.Type == domain.FieldTypeComponent && f.ComponentID != nil && *f.ComponentID == c.ID:
				f.ComponentFields = append([]domain.Field(nil), c.Fields...)
			case f.Type == domain.FieldTypeDynamicZone && f.ZoneAllows(c.Name):
				if f.ZoneFields == nil {
					f.ZoneFields = map[string][]domain.Field{}
				}
				f.ZoneFields[c.Name] = append([]domain.Field(nil), c.Fields...)
			}
		}
	}
}

func copyComponent(c *domain.Component) *domain.Component {
	cp := *c
	cp.Fields = append([]domain.Field(nil), c.Fields...)
	return &cp
}
