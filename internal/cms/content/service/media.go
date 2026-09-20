package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
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

	// Variants are the derived renditions (ADR-019), ADMIN AUDIENCE ONLY like
	// the four fields above, and for the same governance reason. Absent (not empty)
	// when the asset has no rows: a PDF, or an image uploaded before the
	// feature existed and never re-enqueued.
	Variants []MediaVariantDTO `json:"variants,omitempty"`

	// aud is set by ProjectMediaAsset and by nothing else — see EntryAudience.
	aud EntryAudience
}

// MediaVariantDTO is one row of media_variants as the admin sees it. The
// storage key is deliberately not here: the only way to reach bytes is a
// signed URL from ResolveMediaURL, for variants exactly as for originals.
type MediaVariantDTO struct {
	Preset string `json:"preset"`
	State  string `json:"state"`
	// ContentType, SizeBytes, WidthPx and HeightPx are what the WORKER measured
	// — the one place the platform observes an image's dimensions itself
	// (contrast MediaAssetDTO.WidthPx, which is the client's claim). Present
	// once State is done.
	ContentType string `json:"content_type,omitempty"`
	SizeBytes   int64  `json:"size_bytes,omitempty"`
	WidthPx     int    `json:"width_px,omitempty"`
	HeightPx    int    `json:"height_px,omitempty"`
	SkipReason  string `json:"skip_reason,omitempty"`
	Error       string `json:"error,omitempty"`
	Attempts    int    `json:"attempts"`
	// NextAttemptAt is set while a retry is scheduled, so an editor looking at
	// a pending row with attempts > 0 can see when it will be tried again.
	NextAttemptAt *time.Time `json:"next_attempt_at,omitempty"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

func projectMediaVariant(v *domain.MediaVariant) MediaVariantDTO {
	d := MediaVariantDTO{
		Preset:      v.Preset,
		State:       v.State,
		ContentType: v.ContentType,
		SizeBytes:   v.SizeBytes,
		WidthPx:     v.WidthPx,
		HeightPx:    v.HeightPx,
		SkipReason:  v.SkipReason,
		Error:       v.Error,
		Attempts:    v.Attempts,
		UpdatedAt:   v.UpdatedAt,
	}
	if v.State == domain.MediaVariantPending && v.Attempts > 0 {
		at := v.NextAttemptAt
		d.NextAttemptAt = &at
	}
	return d
}

// mediaWire is MediaAssetDTO without its methods, so MarshalJSON does not
// recurse into itself.
type mediaWire MediaAssetDTO

// MarshalJSON renders the asset for its audience, and re-applies the delivery
// projection at serialisation time rather than trusting the constructor.
//
// Same reasoning as EntryDTO.MarshalJSON, and the same refusal on an unset
// audience: a DTO built as a literal has had no audience decision made about
// it, and the field set here is frozen by governance, not merely by
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
		w.Variants = nil
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
// a design opinion: widening the delivery surface takes a ruling, and
// GetMediaAsset is reachable by a delivery credential. Alt text plainly BELONGS
// on a public surface eventually — a static-site build needs it — but that is a
// decision for whoever lifts the gate, not a side effect of adding a column.
// TestMedia_DeliveryDTOKeySetIsFrozen pins the key set so widening it cannot
// happen by accident.
//
// variants is optional and admin-only; a caller inside the service uses
// projectMedia, which fetches them for the right audience only.
func ProjectMediaAsset(a *domain.MediaAsset, sub authn.Subject, variants ...*domain.MediaVariant) MediaAssetDTO {
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
	for _, v := range variants {
		dto.Variants = append(dto.Variants, projectMediaVariant(v))
	}
	return dto
}

// projectMedia is ProjectMediaAsset plus the variant rows, fetched only for an
// audience that will see them. The delivery branch never touches the table:
// its DTO is frozen and a query whose result is discarded is a cost
// with no reader.
func (s *contentService) projectMedia(ctx context.Context, a *domain.MediaAsset, sub authn.Subject) (MediaAssetDTO, error) {
	if audienceFor(sub) == audienceDelivery {
		return ProjectMediaAsset(a, sub), nil
	}
	variants, err := s.repo.ListMediaVariants(ctx, a.TenantID, a.ID)
	if err != nil {
		return MediaAssetDTO{}, err
	}
	return ProjectMediaAsset(a, sub, variants...), nil
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
		return MediaAssetDTO{}, ErrMediaNotUploaded
	}
	if info.Size > MaxUploadBytes {
		return MediaAssetDTO{}, s.discardUpload(ctx, sub.TenantID, a, ErrMediaTooLarge)
	}
	if landed := normalizeContentType(info.ContentType); !uploadTypeAllowed(landed) {
		return MediaAssetDTO{}, s.discardUpload(ctx, sub.TenantID, a, ErrMediaTypeNotAllowed)
	}
	// Byte check (sniffable images only): the declared type is a client claim
	// echoed back as object metadata, so equality with it is unenforced until
	// here. Reads at most sniffWindow bytes — a 25 MiB object never enters
	// memory whole. A Get failure between Stat and now is treated as "no
	// upload", the same answer Stat failing gets above.
	if sniffEnforced(a.ContentType) {
		rc, _, err := s.store.Get(ctx, a.StorageKey)
		if err != nil {
			return MediaAssetDTO{}, ErrMediaNotUploaded
		}
		head, readErr := readSniffWindow(rc)
		_ = rc.Close()
		if readErr != nil {
			return MediaAssetDTO{}, ErrMediaNotUploaded
		}
		if !bytesMatchDeclared(head, a.ContentType) {
			return MediaAssetDTO{}, s.discardUpload(ctx, sub.TenantID, a, ErrMediaBytesMismatch)
		}
	}
	// Marking uploaded and enqueueing the variant rows commit together, so
	// "this is an uploaded image" and "its renditions are on the way" are one
	// fact (ADR-019 §3). A separate insert after the mark would leave a window
	// in which the admin DTO shows an image with no variants and nothing ever
	// scheduled to make them.
	err = s.repo.WithTx(ctx, sub.TenantID, func(tx repository.ContentRepository) error {
		if err := tx.MarkMediaUploaded(ctx, sub.TenantID, id, info.Size, info.ContentType); err != nil {
			return err
		}
		if !domain.IsImageContentType(info.ContentType) {
			return nil
		}
		return tx.EnqueueMediaVariants(ctx, sub.TenantID, id, a.StorageKey)
	})
	if err != nil {
		return MediaAssetDTO{}, err
	}
	a.SizeBytes, a.ContentType = info.Size, info.ContentType
	now := time.Now().UTC()
	a.UploadedAt = &now
	// Admin: this path is ActionContentUpdate, which authorize() refuses to every
	// delivery credential, so no public reader can reach it.
	return s.projectMedia(ctx, a, sub)
}

// EnqueueMediaVariants puts every preset of an uploaded image back on the
// worker's queue — the manual backfill for assets uploaded before ADR-019
// and the retry button for a `failed` row. There is no automatic backfill:
// walking every tenant's bucket on deploy is exactly the unbounded byte
// traffic the ADR keeps off the platform, and an editor who wants a thumbnail
// for a two-year-old image can ask for it.
//
// ActionContentUpdate, like CompleteMediaUpload: it changes the asset's
// derived state, and the verb is already closed to delivery credentials at
// the authorize() chokepoint.
func (s *contentService) EnqueueMediaVariants(ctx context.Context, id uuid.UUID) (MediaAssetDTO, error) {
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
	if !a.IsUploaded() {
		return MediaAssetDTO{}, ErrMediaNotUploaded
	}
	if !domain.IsImageContentType(a.ContentType) {
		return MediaAssetDTO{}, ErrMediaNotImage
	}
	if err := s.repo.EnqueueMediaVariants(ctx, sub.TenantID, id, a.StorageKey); err != nil {
		return MediaAssetDTO{}, err
	}
	return s.projectMedia(ctx, a, sub)
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
	return s.projectMedia(ctx, a, sub)
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
	_, _ = s.repo.DeleteMediaAsset(ctx, tenantID, a.ID)
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
		return s.projectMedia(ctx, a, sub)
	}
	return s.projectMedia(ctx, a, sub)
}

// MediaReferencedBy answers "what points at this asset" (ADR-024, 2.8a) — the
// reverse of a `file`/richtext media reference, read from entry_media /
// entry_media_published. Authorized like GetMediaAsset (ActionRead, "" content
// type — media is not scoped to one type); existence is confirmed the same
// way, by fetching the asset before the reverse query so a missing id 404s
// rather than answering an empty page for it.
func (s *contentService) MediaReferencedBy(ctx context.Context, id uuid.UUID, limit, offset int) (ReferencedByDTO, error) {
	sub, err := s.authorize(ctx, ActionContentRead, id.String(), "")
	if err != nil {
		return ReferencedByDTO{}, err
	}
	if _, err := s.repo.GetMediaAsset(ctx, sub.TenantID, id); err != nil {
		return ReferencedByDTO{}, err
	}
	limit, offset = clampReferencedByPage(limit, offset)
	res, err := s.repo.MediaReferencedBy(ctx, sub.TenantID, id, limit, offset)
	if err != nil {
		return ReferencedByDTO{}, err
	}
	typesByName, err := s.contentTypesByName(ctx, sub.TenantID)
	if err != nil {
		return ReferencedByDTO{}, err
	}
	return projectReferencedBy(res, sub, typesByName), nil
}

// ListMediaInput is the fully-validated input to ListMediaAssets.
type ListMediaInput struct {
	// Query narrows to filenames containing it, case-insensitively. Empty
	// means unfiltered.
	Query string
	// Kind narrows by content type: "image", "video" or "document". Empty
	// means every kind; anything else is ErrMediaKindUnknown.
	Kind   string
	Limit  int
	Offset int
	// Orphan, when true, narrows to assets no entry references (draft or
	// published) — ADR-024.
	Orphan bool
}

// MediaListResult is the admin media library page.
type MediaListResult struct {
	Items  []MediaAssetDTO `json:"items"`
	Total  int             `json:"total"`
	Limit  int             `json:"limit"`
	Offset int             `json:"offset"`
}

// mediaKindPrefixes maps the wire `kind` values to the content_type prefix
// each one narrows to. image/video/document rather than a raw content-type
// filter, because the caller (a media picker) thinks in these three buckets,
// not in MIME types — and a raw filter would let a typo silently match
// nothing instead of failing loudly.
var mediaKindPrefixes = map[string]string{
	"image":    "image/",
	"video":    "video/",
	"document": "application/",
}

// ListMediaAssets is the admin media library: one COUNT(*) and one SELECT,
// never N+1 — items carry no variants (ProjectMediaAsset is called with none),
// because loading them per row on a list page is exactly the query-per-item
// cost ADR-019's variant rows would otherwise impose; a panel that needs a
// specific asset's renditions already has GetMediaAsset for that.
//
// Only uploaded assets are listed (repository.ListMediaAssets' WHERE), for the
// same reason CompleteMediaUpload and EnqueueMediaVariants refuse a bare
// reservation: a row with no bytes behind it is not content yet.
func (s *contentService) ListMediaAssets(ctx context.Context, in ListMediaInput) (MediaListResult, error) {
	sub, err := s.authorize(ctx, ActionContentList, "media", "")
	if err != nil {
		return MediaListResult{}, err
	}
	// Same refusal as ExportSchema and Usage, and the same reasoning: a public
	// delivery credential has no business enumerating the shape of a
	// collection, and this console surface has never been reachable by one —
	// the edge resolves media by id (GetMediaAsset / ResolveMediaURL), never by
	// listing.
	if sub.PublicDelivery {
		return MediaListResult{}, apperrors.New(
			"CONTENT_MEDIA_LIST_FORBIDDEN", "media listing is an admin operation", http.StatusForbidden)
	}

	var prefix string
	if in.Kind != "" {
		p, ok := mediaKindPrefixes[in.Kind]
		if !ok {
			return MediaListResult{}, ErrMediaKindUnknown.WithDetail("kind", in.Kind)
		}
		prefix = p
	}

	limit := in.Limit
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	offset := in.Offset
	if offset < 0 {
		offset = 0
	}

	assets, total, err := s.repo.ListMediaAssets(ctx, sub.TenantID, repository.MediaListFilter{
		Query:             in.Query,
		ContentTypePrefix: prefix,
		Limit:             limit,
		Offset:            offset,
		Orphan:            in.Orphan,
	})
	if err != nil {
		return MediaListResult{}, err
	}
	items := make([]MediaAssetDTO, 0, len(assets))
	for _, a := range assets {
		items = append(items, ProjectMediaAsset(a, sub))
	}
	return MediaListResult{Items: items, Total: total, Limit: limit, Offset: offset}, nil
}

// ResolveMediaURL issues a short-lived read URL. For a delivery credential the
// asset must be referenced by at least one PUBLISHED entry — this is what makes
// published-only true for the bytes and not just the metadata. An admin may
// read their own tenant's assets regardless, to preview drafts.
//
// preset names a rendition (ADR-019); "" is the original. An unknown name is a
// 400 — a typo in a template should fail loudly in development, not quietly
// serve the full-size image forever. A KNOWN preset whose variant is not
// `done` (still pending, skipped because the source was small, failed) falls
// back to the original: the page must render either way, and a 404 or a 202
// here would turn "the thumbnail is still being made" into a broken image.
// The published-only gate applies before any of this; a variant is never more
// readable than its source.
func (s *contentService) ResolveMediaURL(ctx context.Context, id uuid.UUID, preset string) (string, time.Time, error) {
	sub, err := s.authorize(ctx, ActionContentRead, id.String(), "")
	if err != nil {
		return "", time.Time{}, err
	}
	if preset != "" && !domain.ValidMediaPreset(preset) {
		return "", time.Time{}, ErrMediaPresetUnknown
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
	key := a.StorageKey
	if preset != "" {
		variants, err := s.repo.ListMediaVariants(ctx, sub.TenantID, id)
		if err != nil {
			return "", time.Time{}, err
		}
		for _, v := range variants {
			if v.Preset == preset && v.State == domain.MediaVariantDone && v.StorageKey != "" {
				key = v.StorageKey
				break
			}
		}
	}
	url, err := s.store.PresignGet(ctx, key, deliveryTTL)
	if err != nil {
		return "", time.Time{}, err
	}
	return url, time.Now().UTC().Add(deliveryTTL), nil
}

// DeleteMediaAsset removes the metadata and the stored bytes. Entry links go
// with it via ON DELETE CASCADE, so an entry that still names the asset simply
// fails validation on its next write — UNLESS this call is refused first.
//
// force=false (the default a bare DELETE sends) checks entry_media and
// entry_media_published before touching anything: an asset a live entry still
// names is refused with 409 CONTENT_MEDIA_IN_USE rather than deleted out from
// under it, because ON DELETE CASCADE's actual behavior is "the referencing
// entry keeps a dangling id and starts failing validation on its NEXT write" —
// a surprise the caller has had no chance to see coming. force=true is the
// pre-ADR-024 behavior verbatim: the check is skipped and the delete proceeds
// unconditionally, for the operator who has already confirmed (or does not
// care) that something still points at this asset — see ADR-024 §Force delete
// semantics.
func (s *contentService) DeleteMediaAsset(ctx context.Context, id uuid.UUID, force bool) error {
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
	if !force {
		// limit=20: the 409's `details.entries` is a preview for a human to act
		// on, not a full audit — the SAME cap and the SAME row shape the
		// referenced-by endpoint's own repository call already returns, so this
		// reuses it rather than a second query.
		refs, err := s.repo.MediaReferencedBy(ctx, sub.TenantID, id, 20, 0)
		if err != nil {
			return err
		}
		if refs.Total > 0 {
			// The block itself does not depend on visibility — an asset a
			// restricted type still names must stay refused even for a caller
			// who cannot see that type — but the PREVIEW the 409 carries must
			// not leak entries the caller could not GetEntry directly. Same
			// role filter EntryReferencedBy/MediaReferencedBy apply, same
			// reason: readableReferencedByRows.
			typesByName, err := s.contentTypesByName(ctx, sub.TenantID)
			if err != nil {
				return err
			}
			refs.Items = readableReferencedByRows(refs.Items, sub, typesByName)
			return errMediaInUse(refs)
		}
	}
	variantKeys, err := s.repo.DeleteMediaAsset(ctx, sub.TenantID, id)
	if err != nil {
		return err
	}
	// Metadata first, bytes second: a stray object is recoverable waste, whereas
	// a row pointing at deleted bytes is a broken read. The variant objects go
	// best-effort for the same reason discardUpload swallows its errors: the
	// rows are already gone, so nothing can reach those keys again, and failing
	// the request over an orphan the operator can sweep would report a problem
	// the caller cannot act on.
	for _, k := range variantKeys {
		_ = s.store.Delete(ctx, k)
	}
	// A reservation has no bytes: deleting its key would be a no-op against
	// real storage (object deletes are idempotent) but a lie in every log and
	// fake that records it. The row is already gone above; skip the call.
	if a.IsUploaded() {
		if err := s.store.Delete(ctx, a.StorageKey); err != nil {
			return err
		}
	}
	return nil
}

// ErrMediaNotUploaded is returned when an operation needs the bytes to have
// landed and they have not (CompleteMediaUpload found no object; an enqueue was
// asked for a reservation).
var ErrMediaNotUploaded = apperrors.New(
	"CONTENT_MEDIA_NOT_UPLOADED",
	"no object found for this asset; upload first",
	http.StatusConflict,
)

// ErrMediaNotImage is returned when variants are requested for an asset that
// has no renditions to make (PDF, video).
var ErrMediaNotImage = apperrors.New(
	"CONTENT_MEDIA_NOT_IMAGE",
	"variants can only be generated for image assets",
	http.StatusUnprocessableEntity,
)

// ErrMediaPresetUnknown is returned for a `preset` that names nothing in
// domain.MediaPresets. 400 rather than a silent fallback: see ResolveMediaURL.
var ErrMediaPresetUnknown = apperrors.New(
	"CONTENT_MEDIA_PRESET_UNKNOWN",
	"unknown media preset; valid presets are thumb, small, medium, large",
	http.StatusBadRequest,
)

// ErrMediaKindUnknown rejects a `kind` on ListMediaAssets outside
// mediaKindPrefixes ("image", "video", "document"). 400, not a silent
// no-match: a typo in a console filter should fail loudly, the same call
// ErrMediaPresetUnknown makes for `preset`.
var ErrMediaKindUnknown = apperrors.New(
	"CONTENT_MEDIA_KIND_UNKNOWN",
	"unknown media kind; valid kinds are image, video, document",
	http.StatusBadRequest,
)

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

// ErrMediaBytesMismatch rejects an object whose leading bytes are not what
// the reservation declared. Reaching this means the stored bytes were never
// the declared type: the declaration is a client claim carried through the
// signed POST form, so equality with it is what stops an HTML file wearing an
// image/png name from becoming a referenceable asset.
//
// 422, not 415: 415 (ErrMediaTypeNotAllowed) answers "this type may never be
// uploaded"; this answers "these bytes are not that type". The distinction is
// what lets a console tell an editor "the file contents do not match the
// format" instead of "this format is banned".
var ErrMediaBytesMismatch = apperrors.New(
	"CONTENT_MEDIA_BYTES_MISMATCH",
	"the uploaded bytes do not match the declared content type",
	http.StatusUnprocessableEntity,
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

// errMediaInUse is DeleteMediaAsset's refusal (ADR-024 §Force delete
// semantics), shaped like errComponentInUse (component.go): a 409 naming what
// still points at the thing the caller tried to remove, so the response is
// actionable rather than a bare conflict code. `entries` caps at 20 rows
// (refs.Items is already limit=20 from the caller) with `total` alongside so
// the response says honestly when the preview is partial.
func errMediaInUse(refs repository.ReferencedByResult) error {
	entries := make([]map[string]any, len(refs.Items))
	for i, it := range refs.Items {
		entries[i] = map[string]any{
			"entry_id":  it.EntryID,
			"type":      it.TypeName,
			"published": it.Published,
			"draft":     it.Draft,
		}
	}
	return apperrors.New("CONTENT_MEDIA_IN_USE", "media asset is still referenced by one or more entries", 409).
		WithDetails(map[string]any{"entries": entries, "total": refs.Total})
}

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
