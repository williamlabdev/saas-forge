package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/cms/content/repository"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
	"github.com/williamlabdev/saas-forge/internal/pkg/objectstore"
)

// uploadTTL bounds how long a presigned upload stays usable. Short, because the
// client asks for it immediately before uploading.
const uploadTTL = 15 * time.Minute

// deliveryTTL bounds a signed read URL. This is also the revocation window: an
// unpublished entry's media stays reachable through an already-issued URL until
// it expires, and no longer.
const deliveryTTL = 5 * time.Minute

// MaxUploadBytes caps a single asset. This is a platform backstop against abuse
// of the upload path, NOT a plan dimension: unlike Quota.MaxEntryBytes it is the
// same for every tenant. Making it per-plan means a plans-table migration and a
// pricing decision — see ADR-005's trigger conditions.
const MaxUploadBytes int64 = 25 << 20 // 25 MiB

// allowedUploadTypes is the whitelist of content types an asset may declare.
//
// Deliberately absent: image/svg+xml. An SVG is a document that can carry
// script, so serving one from storage makes the bucket host an XSS surface the
// moment it shares an origin with anything that matters. Adding it needs
// sanitising or a forced download disposition first.
var allowedUploadTypes = map[string]struct{}{
	"image/png":       {},
	"image/jpeg":      {},
	"image/gif":       {},
	"image/webp":      {},
	"image/avif":      {},
	"application/pdf": {},
	"video/mp4":       {},
}

// normalizeContentType reduces a declared type to its bare, comparable form:
// "IMAGE/PNG; charset=utf-8" and "image/png" must not be two different answers
// to the whitelist.
func normalizeContentType(ct string) string {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.ToLower(strings.TrimSpace(ct))
}

func uploadTypeAllowed(ct string) bool {
	_, ok := allowedUploadTypes[ct]
	return ok
}

// MediaUploadDTO is what a client needs to upload bytes directly to storage.
//
// The upload is a multipart/form-data POST to UploadURL carrying every entry of
// Fields as a form value, with the bytes last in a part named "file". The form
// is signed with the size and type conditions, so storage refuses a body that
// breaks them — MaxBytes is reported so a client can fail early rather than
// spend the upload first.
type MediaUploadDTO struct {
	AssetID   uuid.UUID         `json:"asset_id"`
	UploadURL string            `json:"upload_url"`
	Fields    map[string]string `json:"fields"`
	MaxBytes  int64             `json:"max_bytes"`
	ExpiresAt time.Time         `json:"expires_at"`
}

// MediaAssetDTO is an asset's metadata.
type MediaAssetDTO struct {
	ID          uuid.UUID  `json:"id"`
	ContentType string     `json:"content_type,omitempty"`
	SizeBytes   int64      `json:"size_bytes"`
	Uploaded    bool       `json:"uploaded"`
	UploadedAt  *time.Time `json:"uploaded_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	// Filename / AltText / WidthPx / HeightPx are client-declared and ADMIN
	// AUDIENCE ONLY — see ProjectMediaAsset for why the delivery response is frozen
	// rather than merely lacking them.
	//
	// Pointers with omitempty so absence and emptiness stay distinct on the wire:
	// a missing `alt_text` key means nobody has described the image, while
	// `"alt_text": ""` is an editor stating it is decorative. A plain string would
	// serialise both as the same thing and hand a renderer the wrong licence to
	// emit alt="" (migration 000022 has the full argument).
	Filename *string `json:"filename,omitempty"`
	AltText  *string `json:"alt_text,omitempty"`
	WidthPx  *int    `json:"width_px,omitempty"`
	HeightPx *int    `json:"height_px,omitempty"`

	// aud is set by ProjectMediaAsset and by nothing else — see EntryAudience.
	aud EntryAudience
}

// mediaWire is MediaAssetDTO without its methods, so MarshalJSON does not
// recurse into itself.
type mediaWire MediaAssetDTO

// MarshalJSON renders the asset for its audience, and re-applies the delivery
// projection at serialisation time rather than trusting the constructor.
//
// Same reasoning as EntryDTO.MarshalJSON, and the same refusal on an unset
// audience: a DTO built as a literal has had no audience decision made about
// it, and the field set here is frozen by governance (OD-007), not merely by
// preference. Guessing would widen a public surface by accident.
func (d MediaAssetDTO) MarshalJSON() ([]byte, error) {
	w := mediaWire(d)
	switch d.aud {
	case audienceAdmin:
		// Everything stays.
	case audienceDelivery:
		// The declared metadata, cleared unconditionally.
		// TestMedia_DeliveryDTOKeySetIsFrozen names any key added and not
		// classified here.
		w.Filename, w.AltText, w.WidthPx, w.HeightPx = nil, nil, nil, nil
	default:
		return nil, fmt.Errorf("content: media asset %s has no audience — build DTOs with ProjectMediaAsset, never as a literal", d.ID)
	}
	return json.Marshal(w)
}

// ProjectMediaAsset renders an asset for the credential that asked for it. It is
// the only constructor of a renderable MediaAssetDTO.
//
// The audience is derived from the Subject rather than passed in, for the reason
// EntryAudience's doc gives: a caller that can choose can choose wrong, and the
// wrong answer is a leak. The type is called EntryAudience because entries are
// where it started; renaming it is a separate change.
//
// The delivery branch returns EXACTLY the field set delivery received before
// migration 000022 existed, byte for byte. That is a governance constraint, not
// a design opinion: organon OD-007 forbids new delivery-surface work, and
// GetMediaAsset is reachable by a delivery credential. Alt text plainly BELONGS
// on a public surface eventually — a static-site build needs it — but that is a
// decision for whoever lifts the gate, not a side effect of adding a column.
// TestMedia_DeliveryDTOKeySetIsFrozen pins the key set so widening it cannot
// happen by accident.
func ProjectMediaAsset(a *domain.MediaAsset, sub authn.Subject) MediaAssetDTO {
	aud := audienceFor(sub)
	dto := MediaAssetDTO{
		ID:          a.ID,
		ContentType: a.ContentType,
		SizeBytes:   a.SizeBytes,
		Uploaded:    a.IsUploaded(),
		UploadedAt:  a.UploadedAt,
		CreatedAt:   a.CreatedAt,
		aud:         aud,
	}
	if aud == audienceDelivery {
		return dto
	}
	// Admin-only from here down. Gathered below the return on purpose, mirroring
	// ProjectEntry: assigning an admin-only field into the shared literal above and
	// trusting the delivery branch to unset it is the shape that shipped OD2-023
	// F2. Add new admin-only fields HERE, never above.
	dto.Filename, dto.AltText = a.Filename, a.AltText
	dto.WidthPx, dto.HeightPx = a.WidthPx, a.HeightPx
	return dto
}

// MediaMetadataInput is the client-declared metadata, in the three-state form
// the PATCH surface needs (see Optional). It is shared with the reservation
// endpoint even though a create has no use for the absent/null distinction —
// there they mean the same thing — because one struct means ONE validation path,
// and two validators for one set of columns is how the create surface and the
// patch surface come to disagree about what a legal filename is.
type MediaMetadataInput struct {
	Filename Optional[string] `json:"filename"`
	AltText  Optional[string] `json:"alt_text"`
	WidthPx  Optional[int]    `json:"width_px"`
	HeightPx Optional[int]    `json:"height_px"`
}

// CreateMediaUploadInput reserves an asset. ContentType is pre-populated by the
// handler from the legacy ?content_type= query parameter and then overwritten if
// the body names one, so existing callers that pass only the query keep working.
type CreateMediaUploadInput struct {
	ContentType string `json:"content_type"`
	MediaMetadataInput
}

// UpdateMediaAssetInput is a per-field PATCH of the declared metadata.
type UpdateMediaAssetInput struct {
	MediaMetadataInput
}

// toPatch validates the declared metadata and turns it into a repository patch.
//
// Every rejection here happens BEFORE any repository call, which is what makes
// these assertable against the in-memory fake without the fake's fidelity
// mattering. Each rule mirrors a CHECK in migration 000022; the DB is still the
// authority, and this layer exists so a violation is a 422 naming the field
// rather than a constraint error surfacing as a 500.
func (in MediaMetadataInput) toPatch() (repository.MediaAssetPatch, error) {
	var p repository.MediaAssetPatch

	if in.Filename.Set {
		if v := in.Filename.Value; v != nil {
			if err := validateFilename(*v); err != nil {
				return repository.MediaAssetPatch{}, err
			}
		}
		p.SetFilename, p.Filename = true, in.Filename.Value
	}

	if in.AltText.Set {
		// No emptiness check: "" is a MEANINGFUL value here (decorative), which is
		// the whole reason the column is nullable instead of NOT NULL DEFAULT ''.
		if v := in.AltText.Value; v != nil && utf8.RuneCountInString(*v) > domain.MaxAltTextLen {
			return repository.MediaAssetPatch{}, ErrMediaAltTextTooLong
		}
		p.SetAltText, p.AltText = true, in.AltText.Value
	}

	// Dimensions are one decision, not two. A caller that mentions one and not the
	// other, or nulls one and sets the other, is refused rather than half-applied:
	// the pair is what reserves layout space, and half of it is a wrong aspect
	// ratio rather than a missing one.
	switch {
	case !in.WidthPx.Set && !in.HeightPx.Set:
		// Neither mentioned: leave the stored pair alone.
	case in.WidthPx.Set != in.HeightPx.Set:
		return repository.MediaAssetPatch{}, ErrMediaDimensionsIncomplete
	case (in.WidthPx.Value == nil) != (in.HeightPx.Value == nil):
		return repository.MediaAssetPatch{}, ErrMediaDimensionsIncomplete
	default:
		if in.WidthPx.Value != nil {
			if err := validateDimension("width_px", *in.WidthPx.Value); err != nil {
				return repository.MediaAssetPatch{}, err
			}
			if err := validateDimension("height_px", *in.HeightPx.Value); err != nil {
				return repository.MediaAssetPatch{}, err
			}
		}
		p.SetDimensions = true
		p.WidthPx, p.HeightPx = in.WidthPx.Value, in.HeightPx.Value
	}

	return p, nil
}

// validateFilename mirrors media_assets_filename_check.
func validateFilename(name string) error {
	if !utf8.ValidString(name) {
		// Postgres would refuse the byte sequence at the wire protocol, which
		// surfaces as a driver error and therefore a 500. Caught here it is a 422.
		return ErrMediaFilenameInvalid.WithDetail("reason", "not valid UTF-8")
	}
	// Counted in runes, not bytes: char_length() counts characters, so a byte
	// count would refuse a legal name in any non-ASCII script.
	switch n := utf8.RuneCountInString(name); {
	case n == 0:
		// An empty name is not a name. Callers clearing a filename send null.
		return ErrMediaFilenameInvalid.WithDetail("reason", "must not be empty; send null to clear it")
	case n > domain.MaxFilenameLen:
		return ErrMediaFilenameInvalid.WithDetails(map[string]any{
			"reason": "too long", "max": domain.MaxFilenameLen,
		})
	}
	if strings.ContainsAny(name, `/\`) {
		return ErrMediaFilenameInvalid.WithDetail("reason", `must not contain "/" or "\"`)
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return ErrMediaFilenameInvalid.WithDetail("reason", "must not contain control characters")
		}
	}
	return nil
}

// validateDimension mirrors the range half of media_assets_dimensions_check.
func validateDimension(field string, v int) error {
	if v < 1 || v > domain.MaxImageDimension {
		return ErrMediaDimensionOutOfRange.WithDetails(map[string]any{
			"field": field, "value": v, "min": 1, "max": domain.MaxImageDimension,
		})
	}
	return nil
}

// storageKey builds an unguessable, tenant-prefixed object key. The random
// suffix is defence in depth: the bucket is private, but a key that could be
// derived from the asset id would make a future misconfiguration far worse.
func storageKey(tenantID string, id uuid.UUID) (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("storage key entropy: %w", err)
	}
	return fmt.Sprintf("%s/%s-%s", tenantID, id, hex.EncodeToString(buf)), nil
}

// CreateMediaUpload reserves an asset and returns a direct-to-storage upload
// URL. The row exists but is NOT usable until CompleteMediaUpload confirms the
// bytes landed — a reservation must never be referenceable.
// The client-declared metadata lands HERE, at reservation, and not at
// completion. CompleteMediaUpload's job is to record what actually landed in the
// bucket — MarkMediaUploaded says "never from the client" in as many words — and
// threading a client claim through that path would make the sentence false for
// the columns it still governs. Reservation is also the moment the client is
// holding the file, so it is when it knows the original name and the pixel
// dimensions; by completion it has only an id.
func (s *contentService) CreateMediaUpload(ctx context.Context, in CreateMediaUploadInput) (MediaUploadDTO, error) {
	sub, err := s.authorize(ctx, ActionContentCreate, "media", "")
	if err != nil {
		return MediaUploadDTO{}, err
	}
	if s.store == nil {
		return MediaUploadDTO{}, ErrMediaDisabled
	}
	// Checked before the row is written: a refused type must not leave a
	// reservation behind, and there is nothing to clean up if we never start.
	ct := normalizeContentType(in.ContentType)
	if !uploadTypeAllowed(ct) {
		return MediaUploadDTO{}, ErrMediaTypeNotAllowed
	}
	// Same reasoning applied to the declared metadata: refused before the
	// reservation exists, so a bad filename cannot leave an orphan row behind.
	patch, err := in.toPatch()
	if err != nil {
		return MediaUploadDTO{}, err
	}
	id := uuid.New()
	key, err := storageKey(sub.TenantID, id)
	if err != nil {
		return MediaUploadDTO{}, err
	}
	now := time.Now().UTC()
	a := &domain.MediaAsset{
		ID: id, TenantID: sub.TenantID, StorageKey: key,
		ContentType: ct, CreatedAt: now,
		// On a create, "absent" and "explicit null" mean the same thing, so the
		// Set flags carry no information and the values go straight through.
		Filename: patch.Filename, AltText: patch.AltText,
		WidthPx: patch.WidthPx, HeightPx: patch.HeightPx,
	}
	if err := s.repo.CreateMediaAsset(ctx, a); err != nil {
		return MediaUploadDTO{}, err
	}
	up, err := s.store.PresignPost(ctx, key, uploadTTL, objectstore.UploadConstraints{
		MaxBytes:    MaxUploadBytes,
		ContentType: ct,
	})
	if err != nil {
		return MediaUploadDTO{}, err
	}
	return MediaUploadDTO{
		AssetID:   id,
		UploadURL: up.URL,
		Fields:    up.Fields,
		MaxBytes:  MaxUploadBytes,
		ExpiresAt: now.Add(uploadTTL),
	}, nil
}

// CompleteMediaUpload confirms the bytes landed. Size and content type are read
// back from storage rather than taken from the caller — the client could
// otherwise declare a 1-byte image and upload a gigabyte.
//
// The size and type limits are re-checked here even though the signed form
// already carries them as conditions. That is not belt-and-braces for its own
// sake: the policy is only as good as the provider enforcing it, and an object
// can reach the key by another route (operator tooling, a restored backup, a
// future change to how uploads are signed). This check is what decides whether
// the asset becomes referenceable, so it is the one that has to hold.
func (s *contentService) CompleteMediaUpload(ctx context.Context, id uuid.UUID) (MediaAssetDTO, error) {
	sub, err := s.authorize(ctx, ActionContentUpdate, id.String(), "")
	if err != nil {
		return MediaAssetDTO{}, err
	}
	if s.store == nil {
		return MediaAssetDTO{}, ErrMediaDisabled
	}
	a, err := s.repo.GetMediaAsset(ctx, sub.TenantID, id)
	if err != nil {
		return MediaAssetDTO{}, err
	}
	info, err := s.store.Stat(ctx, a.StorageKey)
	if err != nil {
		// No object under the reserved key: the upload never happened.
		return MediaAssetDTO{}, apperrors.New("CONTENT_MEDIA_NOT_UPLOADED", "no object found for this asset; upload first", 409)
	}
	if info.Size > MaxUploadBytes {
		return MediaAssetDTO{}, s.discardUpload(ctx, sub.TenantID, a, ErrMediaTooLarge)
	}
	if landed := normalizeContentType(info.ContentType); !uploadTypeAllowed(landed) {
		return MediaAssetDTO{}, s.discardUpload(ctx, sub.TenantID, a, ErrMediaTypeNotAllowed)
	}
	if err := s.repo.MarkMediaUploaded(ctx, sub.TenantID, id, info.Size, info.ContentType); err != nil {
		return MediaAssetDTO{}, err
	}
	a.SizeBytes, a.ContentType = info.Size, info.ContentType
	now := time.Now().UTC()
	a.UploadedAt = &now
	// Admin: this path is ActionContentUpdate, which authorize() refuses to every
	// delivery credential, so no public reader can reach it.
	return ProjectMediaAsset(a, sub), nil
}

// UpdateMediaAsset patches the client-declared metadata, one field at a time.
//
// No If-Match. Optimistic locking guards entries.version, which is the counter
// for the entry PAYLOAD; media metadata is neither part of that document nor
// versioned by it. Coupling the two would mean an editor fixing alt text on an
// image conflicts with a colleague editing the article's text — two people doing
// unrelated work, told they collided. The concurrent-write risk that remains is
// last-writer-wins on a single descriptive field, which is the ordinary and
// recoverable outcome, not a lost payload.
//
// ActionContentUpdate is reused rather than a new verb, for the reason
// SetEntryStatus gives: authorize() refuses every non-read action to a delivery
// credential at one chokepoint, so this endpoint is closed to the public surface
// for free and stays closed if the RBAC roles are ever re-cut.
//
// No s.store check either. This writes a Postgres row; a deployment with no
// object store configured can still correct an alt text on assets it already
// has, and answering 501 would be reporting a dependency this call does not use.
func (s *contentService) UpdateMediaAsset(ctx context.Context, id uuid.UUID, in UpdateMediaAssetInput) (MediaAssetDTO, error) {
	sub, err := s.authorize(ctx, ActionContentUpdate, id.String(), "")
	if err != nil {
		return MediaAssetDTO{}, err
	}
	patch, err := in.toPatch()
	if err != nil {
		return MediaAssetDTO{}, err
	}
	a, err := s.repo.UpdateMediaAssetMetadata(ctx, sub.TenantID, id, patch)
	if err != nil {
		return MediaAssetDTO{}, err
	}
	return ProjectMediaAsset(a, sub), nil
}

// discardUpload throws away an asset that broke a limit and returns cause.
//
// Both the row and the bytes go: keeping the row would let the client retry
// completion forever, and keeping the object would turn every rejected upload
// into permanent unreferenced storage — the orphan problem ADR-005 already
// records as unsolved. A violation should not be a way to reach it on purpose.
// Cleanup failures are swallowed deliberately: the caller must still learn why
// the upload was refused, and a stray object is recoverable waste.
func (s *contentService) discardUpload(ctx context.Context, tenantID string, a *domain.MediaAsset, cause error) error {
	_ = s.repo.DeleteMediaAsset(ctx, tenantID, a.ID)
	_ = s.store.Delete(ctx, a.StorageKey)
	return cause
}

func (s *contentService) GetMediaAsset(ctx context.Context, id uuid.UUID) (MediaAssetDTO, error) {
	sub, err := s.authorize(ctx, ActionContentRead, id.String(), "")
	if err != nil {
		return MediaAssetDTO{}, err
	}
	a, err := s.repo.GetMediaAsset(ctx, sub.TenantID, id)
	if err != nil {
		return MediaAssetDTO{}, err
	}
	// The same gate ResolveMediaURL applies, for the same reason. Without it the
	// two endpoints disagreed about the same asset for the same credential:
	// /media/{id}/url answered 404 while /media/{id} answered 200 with size and
	// content type — which defeats the anti-oracle design ResolveMediaURL was
	// written with. The public edge never calls this endpoint, so nothing
	// legitimate loses access; a delivery credential simply has no business
	// reading metadata for bytes it may not read.
	if sub.PublicDelivery {
		if !a.IsUploaded() {
			return MediaAssetDTO{}, apperrors.ErrNotFound
		}
		published, err := s.repo.AssetIsPublished(ctx, sub.TenantID, id)
		if err != nil {
			return MediaAssetDTO{}, err
		}
		// 404, not 403 — a distinguishable refusal confirms the asset exists.
		if !published {
			return MediaAssetDTO{}, apperrors.ErrNotFound
		}
		s.delivery.Record(sub.TenantID)
		// The one place in this service where a delivery audience reaches a media
		// DTO. Everything about the response below this line is frozen — see
		// ProjectMediaAsset.
		return ProjectMediaAsset(a, sub), nil
	}
	return ProjectMediaAsset(a, sub), nil
}

// ResolveMediaURL issues a short-lived read URL. For a delivery credential the
// asset must be referenced by at least one PUBLISHED entry — this is what makes
// published-only true for the bytes and not just the metadata. An admin may
// read their own tenant's assets regardless, to preview drafts.
func (s *contentService) ResolveMediaURL(ctx context.Context, id uuid.UUID) (string, time.Time, error) {
	sub, err := s.authorize(ctx, ActionContentRead, id.String(), "")
	if err != nil {
		return "", time.Time{}, err
	}
	if s.store == nil {
		return "", time.Time{}, ErrMediaDisabled
	}
	a, err := s.repo.GetMediaAsset(ctx, sub.TenantID, id)
	if err != nil {
		return "", time.Time{}, err
	}
	if !a.IsUploaded() {
		return "", time.Time{}, apperrors.ErrNotFound
	}
	if sub.PublicDelivery {
		published, err := s.repo.AssetIsPublished(ctx, sub.TenantID, id)
		if err != nil {
			return "", time.Time{}, err
		}
		// 404, not 403: a distinguishable refusal would confirm the asset exists.
		if !published {
			return "", time.Time{}, apperrors.ErrNotFound
		}
		s.delivery.Record(sub.TenantID)
	}
	url, err := s.store.PresignGet(ctx, a.StorageKey, deliveryTTL)
	if err != nil {
		return "", time.Time{}, err
	}
	return url, time.Now().UTC().Add(deliveryTTL), nil
}

// DeleteMediaAsset removes the metadata and the stored bytes. Entry links go
// with it via ON DELETE CASCADE, so an entry that still names the asset simply
// fails validation on its next write.
func (s *contentService) DeleteMediaAsset(ctx context.Context, id uuid.UUID) error {
	sub, err := s.authorize(ctx, ActionContentDelete, id.String(), "")
	if err != nil {
		return err
	}
	if s.store == nil {
		return ErrMediaDisabled
	}
	a, err := s.repo.GetMediaAsset(ctx, sub.TenantID, id)
	if err != nil {
		return err
	}
	if err := s.repo.DeleteMediaAsset(ctx, sub.TenantID, id); err != nil {
		return err
	}
	// Metadata first, bytes second: a stray object is recoverable waste, whereas
	// a row pointing at deleted bytes is a broken read.
	if err := s.store.Delete(ctx, a.StorageKey); err != nil {
		return err
	}
	return nil
}

// ErrMediaDisabled is returned when no object store is configured.
var ErrMediaDisabled = apperrors.New(
	"CONTENT_MEDIA_DISABLED",
	"media storage is not configured on this deployment",
	501,
)

// ErrMediaTypeNotAllowed rejects a content type outside allowedUploadTypes,
// whether declared at reservation time or observed on the stored object.
var ErrMediaTypeNotAllowed = apperrors.New(
	"CONTENT_MEDIA_TYPE_NOT_ALLOWED",
	"this content type may not be uploaded",
	415,
)

// ErrMediaTooLarge rejects an object over MaxUploadBytes. Reaching this means
// the storage server accepted a body its signed policy should have refused.
var ErrMediaTooLarge = apperrors.New(
	"CONTENT_MEDIA_TOO_LARGE",
	"the uploaded file exceeds the maximum allowed size",
	413,
)

// The declared-metadata refusals. All 422: the request was well-formed JSON that
// violated a rule the database also enforces, which is the same shape as the
// entry-payload errors in errors.go. Each carries `details` so the caller learns
// WHICH rule, because "invalid filename" alone is not actionable.

// ErrMediaFilenameInvalid rejects a declared filename that is empty, over
// domain.MaxFilenameLen, or carries a path separator or a control character.
var ErrMediaFilenameInvalid = apperrors.New(
	"CONTENT_MEDIA_FILENAME_INVALID",
	"filename is not acceptable",
	422,
)

// ErrMediaAltTextTooLong rejects alt text over domain.MaxAltTextLen. Note there
// is no "alt text is empty" error: "" is a legal, meaningful value.
var ErrMediaAltTextTooLong = apperrors.New(
	"CONTENT_MEDIA_ALT_TEXT_TOO_LONG",
	"alt text exceeds the maximum length",
	422,
)

// ErrMediaDimensionsIncomplete rejects a width without a height or vice versa.
// Refusing beats storing half: the pair exists to reserve layout space, and one
// number produces a confidently wrong aspect ratio rather than an absent one.
var ErrMediaDimensionsIncomplete = apperrors.New(
	"CONTENT_MEDIA_DIMENSIONS_INCOMPLETE",
	"width_px and height_px must be provided together, or both null",
	422,
)

// ErrMediaDimensionOutOfRange rejects a dimension outside 1..MaxImageDimension.
var ErrMediaDimensionOutOfRange = apperrors.New(
	"CONTENT_MEDIA_DIMENSION_OUT_OF_RANGE",
	"image dimension is out of range",
	422,
)

// WithMediaStore enables the media flow on an existing service. Exported as a
// function rather than a constructor parameter so the two composition roots
// (cmd/server's wire graph and platform.BuildApp) can both opt in without
// another N-argument constructor.
func WithMediaStore(svc ContentService, store objectstore.Store) ContentService {
	if cs, ok := svc.(*contentService); ok {
		return cs.WithObjectStore(store)
	}
	return svc
}
