package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"net/http"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/cms/content/repository"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// ErrAgentCredentialUnidentified refuses an idempotency key from an agent
// credential that carries no CredentialID.
//
// It fails closed rather than falling back to the minter's user id, and the
// fallback is the tempting wrong answer: it would work, and it would quietly
// merge every agent minted by one person into a single key namespace — the
// tenant-wide scope that 000036's actor_key column exists to avoid, wearing the
// per-credential scope's name. jwt sets CredentialID whenever Kind is agent, so
// nothing reaches this today; what it buys is that a future credential shape
// that forgets to cannot silently widen the namespace.
var ErrAgentCredentialUnidentified = apperrors.New(
	"AGENT_CREDENTIAL_UNIDENTIFIED",
	"this agent credential carries no credential id and cannot use Idempotency-Key",
	http.StatusForbidden,
)

// ErrIdempotencyRecordVanished is the one shape replayCreate cannot answer: a
// key that was spent moments ago names an entry that is not there.
//
// The cascade in 000036 makes this near-unreachable — deleting the entry takes
// the record with it — so the reachable cause is a content type dropped and
// recreated under the same name between the two calls. Erroring is the honest
// answer; creating a second entry would be the duplicate the key was sent to
// prevent, produced at exactly the moment the mechanism was asked for.
var ErrIdempotencyRecordVanished = apperrors.New(
	"CONTENT_IDEMPOTENCY_RECORD_VANISHED",
	"the entry this Idempotency-Key produced no longer exists; use a new key",
	http.StatusConflict,
)

// idempotencyActorKey names the issuer that owns a key's namespace — 000036's
// actor_key, whose column comment carries the ruling and the reason the two
// spellings differ.
func idempotencyActorKey(sub authn.Subject) (string, error) {
	if !sub.IsAgent() {
		return "human:" + sub.UserID.String(), nil
	}
	if sub.CredentialID == nil {
		return "", ErrAgentCredentialUnidentified
	}
	return "agent:" + sub.CredentialID.String(), nil
}

// createRequestFingerprint digests everything about a create request that
// decides which row it produces: the type, the locale, the translation source,
// and the document.
//
// EVERY PART IS LENGTH-PREFIXED. Concatenating the parts raw would let two
// different requests hash the same — a type named "post" with locale "en" and
// one named "poste" with locale "n" produce identical bytes — and a fingerprint
// collision is precisely the false MATCH the column comment says must not be
// possible. The lengths make the parts unambiguous to read back, so no pair of
// distinct requests shares a digest.
//
// The payload is compacted, not canonicalised. See 000036: re-serialising the
// same document with different key order yields a different digest and so a 409,
// which is wrong but loud, and the loud direction is the one chosen.
func createRequestFingerprint(typeName string, in CreateLocalizedInput) ([]byte, error) {
	var compact bytes.Buffer
	if err := json.Compact(&compact, in.Payload); err != nil {
		// Unreachable through the handler, which rejects malformed JSON before the
		// service sees it. Returned rather than ignored because a service-level
		// caller has no such gate, and hashing bytes we could not parse would make
		// the digest depend on formatting we never validated.
		return nil, apperrors.New("CONTENT_PAYLOAD_INVALID", "payload must be a JSON object", http.StatusBadRequest)
	}

	locale := in.Locale
	if locale == "" {
		// Digest what the request MEANS, not what it omitted: an explicit
		// "en" and a defaulted "en" produce the same row, so a retry that spells
		// the default out must not read as a different request.
		locale = domain.DefaultLocale
	}
	translationOf := ""
	if in.TranslationOf != nil {
		translationOf = in.TranslationOf.String()
	}

	h := sha256.New()
	for _, part := range [][]byte{
		[]byte(typeName),
		[]byte(locale),
		[]byte(translationOf),
		compact.Bytes(),
	} {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(part)))
		h.Write(n[:])
		h.Write(part)
	}
	return h.Sum(nil), nil
}

// replayCreate answers what a spent key produced, or nil if the key is unspent.
//
// It re-reads the entry and projects it through ProjectEntry rather than
// returning anything stored alongside the key. Storing the response would freeze
// a projection made under the permissions of the first call: field permission
// can be narrowed between the create and the retry, and a replay serving the
// original body would hand back a key the caller may no longer read — the write
// path answering a question the read path would have refused.
func (s *contentService) replayCreate(ctx context.Context, sub authn.Subject, ct *domain.ContentType, actorKey, idemKey string, fingerprint []byte) (*EntryDTO, error) {
	rec, err := s.repo.FindEntryIdempotency(ctx, sub.TenantID, actorKey, idemKey)
	if err != nil || rec == nil {
		return nil, err
	}
	if !bytes.Equal(rec.Fingerprint, fingerprint) {
		return nil, repository.ErrIdempotencyFingerprintMismatch
	}
	e, err := s.repo.GetEntry(ctx, sub.TenantID, ct.ID, rec.EntryID)
	if err != nil {
		return nil, err
	}
	dto := ProjectEntry(ct, e, sub)
	return &dto, nil
}
