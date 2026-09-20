package service

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/cms/content/repository"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// The cursor is deliberately OPAQUE to callers: base64url over a small JSON
// object. Opaque is not obfuscation — it is the only thing that lets the sort
// key change later (add a tiebreaker, switch the ordering column) without every
// consumer that hand-built a token breaking. The API contract is "hand back
// exactly what you were given"; nothing else about the token is promised.
//
// That promise is what made ADR-006 Amendment 5 a change to this file and
// nothing else on the wire: the token gained three fields, and no consumer that
// kept its side of the contract noticed.
//
// It is NOT signed, and does not need to be. It encodes a position in a result
// set — a created_at or a sort value, plus an entry id — all of which the same
// caller can already read off the page it came from, and it can only ever move
// a scan window WITHIN the result set the caller's own credential already
// scopes (tenant + type + status=published are applied independently of the
// cursor). Forging one buys nothing.
//
// TWO SHAPES, and the shape is chosen by the request, not by the token:
//
//	{"t": RFC3339Nano, "i": uuid}                   — no sort: (created_at, id)
//	{"k": key, "d": "asc"|"desc", "v": …, "i": uuid} — sorted: (value, id)
//
// The first shape is the one every cursor had before Amendment 5, and it is
// still minted for every unsorted page: a token issued by the old code is a
// token this code accepts, so a consumer mid-page-through across a deploy is
// not handed a 400.

// timeCursor is the unsorted shape, byte for byte what every cursor was before
// Amendment 5. T is RFC3339 with nanoseconds so the round-trip is lossless
// against a Postgres timestamptz.
type timeCursor struct {
	T string `json:"t"`
	I string `json:"i"`
}

// sortCursor is the sorted shape. K/D restate the sort the page was produced
// under; they are NOT how the repository learns the sort — the request carries
// that — they exist so a cursor can be REFUSED when it does not describe the
// order the caller is now asking for (see decodeCursor).
//
// V is written even when nil, as an explicit `"v": null`, because "the row had
// no value there" is a real position — the trailing null block — and a reader
// that saw an omitted field would have to guess which.
type sortCursor struct {
	K string  `json:"k"`
	D string  `json:"d"`
	V *string `json:"v"`
	I string  `json:"i"`
}

// cursorPayload is the DECODE form: a superset that accepts either shape. Which
// one arrived is read off K, never off the presence of a value.
type cursorPayload struct {
	T string  `json:"t"`
	I string  `json:"i"`
	K string  `json:"k"`
	D string  `json:"d"`
	V *string `json:"v"`
}

// ErrCursorInvalid is a malformed, tampered, or MISMATCHED cursor. It is a 400
// rather than a silent "start from the beginning": quietly restarting a
// page-through loop is how a consumer ends up in an infinite loop re-reading
// page 1 forever.
var ErrCursorInvalid = apperrors.New("CONTENT_CURSOR_INVALID", "cursor is not a cursor this API issued", 400)

// sortDir renders a SortSpec's direction the way the cursor and the error
// details spell it.
func sortDir(s *repository.SortSpec) string {
	if s == nil {
		return ""
	}
	if s.Desc {
		return "desc"
	}
	return "asc"
}

// encodeCursor mints the token that resumes AFTER `last`, under the sort the
// page was produced with.
//
// matchPublished picks the payload the sort value is read from, and it is the
// same flag the repository's ORDER BY reads (predicateColumn). Reading the
// working copy here while the database ordered by the snapshot would mint a
// cursor pointing at a position that does not exist in the caller's order —
// the page-through would skip or repeat rows, and only on entries with an
// unpublished edit, which is precisely the set nobody can see to debug it.
func encodeCursor(last *domain.Entry, sort *repository.SortSpec, matchPublished bool) string {
	var v any
	if sort == nil {
		v = timeCursor{T: last.CreatedAt.UTC().Format(time.RFC3339Nano), I: last.ID.String()}
	} else {
		payload := last.Payload
		if matchPublished {
			payload = last.PublishedPayload
		}
		v = sortCursor{
			K: sort.Field.Key,
			D: sortDir(sort),
			V: sortValueText(payload, sort.Field.Key),
			I: last.ID.String(),
		}
	}
	raw, err := json.Marshal(v)
	if err != nil {
		// Unreachable: every field is a string or a *string. Returning ""
		// degrades to "no next page", which is the fail-closed direction — a
		// caller stops early rather than looping on a cursor that does not
		// advance.
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// decodeCursor turns the token back into a keyset, and refuses one that does
// not describe the order being asked for.
//
// The mismatch check is not defensive tidiness. A keyset window is only correct
// against the ORDER BY it was minted under: resume a `price:asc` cursor inside
// a `title:desc` query and the database happily answers, having compared a
// price against titles — pages that silently skip most of the collection and
// repeat the rest, with no error anywhere. There is no correct way to translate
// a position from one order into another (the row it names sits somewhere else
// entirely), so the only honest answers are "refuse" and "silently restart at
// page 1", and the second is the infinite loop ErrCursorInvalid exists to
// avoid. Cursors therefore do not travel across sorts: change the sort, start
// the page-through again.
func decodeCursor(raw string, sort *repository.SortSpec) (*repository.EntryCursor, error) {
	if raw == "" {
		return nil, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, ErrCursorInvalid
	}
	var p cursorPayload
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, ErrCursorInvalid
	}
	id, err := uuid.Parse(p.I)
	if err != nil {
		return nil, ErrCursorInvalid
	}

	wantKey, wantDir := "", ""
	if sort != nil {
		wantKey, wantDir = sort.Field.Key, sortDir(sort)
	}
	if p.K != wantKey || p.D != wantDir {
		// Named in both directions — "you sent a cursor for X while asking for
		// Y" is actionable, "invalid cursor" is not, and the mismatch is the
		// one cursor failure a correct client can hit by changing one query
		// parameter mid-loop.
		return nil, ErrCursorInvalid.WithDetails(map[string]any{
			"reason":       "cursor does not match the requested sort; restart the page-through",
			"cursor_sort":  sortPair(p.K, p.D),
			"request_sort": sortPair(wantKey, wantDir),
		})
	}
	if sort != nil {
		return &repository.EntryCursor{ID: id, SortValue: p.V}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, p.T)
	if err != nil {
		return nil, ErrCursorInvalid
	}
	return &repository.EntryCursor{CreatedAt: t, ID: id}, nil
}

// sortPair renders a key/direction for an error detail; "" means "no sort",
// which is a distinct thing from a sort the caller spelled oddly.
func sortPair(key, dir string) any {
	if key == "" {
		return nil
	}
	return key + ":" + dir
}

// sortValueText renders one payload key exactly as Postgres's `->>` would, so
// the value bound into the keyset predicate compares equal to the value the
// ORDER BY extracted from the same row. nil is `->>`'s NULL: no such key, or a
// JSON null.
//
// The types that need to agree BYTE for byte are the ones orderedExpr does not
// cast — string/text/enum/date/file/relation — and for those `->>` yields the
// unescaped scalar, which is what a Go decode yields too.
//
// number and datetime are compared after a cast (::numeric, ::timestamptz), so
// they need only agree as VALUES: json.Number preserves the literal digits from
// the payload, and `1.0` and `1` are one numeric either way. Decoding into a
// float64 instead would be the bug here, and the reason is PRECISION, not
// spelling: ::numeric accepts scientific notation perfectly well (`1e+21` and
// `1e21` both cast to 1000000000000000000000), so a re-spelled exponent would
// still compare equal. What does not survive is a value wider than float64's
// 53-bit mantissa — a payload holding 12345678901234567890 comes back from
// float64 as 12345678901234567000, which is a DIFFERENT numeric. The keyset
// window would then be bound to a value no row holds, and the page-through
// would resume in the wrong place with nothing raising an error.
//
// Booleans are "true"/"false", which is `->>`'s spelling.
//
// A composite value cannot reach this: parseSort refuses multi-valued and
// richtext fields, and every remaining type is scalar. The fallback marshals
// rather than dropping the value, because a nil here does not mean "unknown",
// it means NULL — and claiming NULL for a row that has a value would resume the
// page-through in the wrong block.
func sortValueText(payload []byte, key string) *string {
	if len(payload) == 0 {
		return nil
	}
	var doc map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil
	}
	raw, ok := doc[key]
	if !ok {
		return nil
	}
	var v any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&v); err != nil {
		return nil
	}
	var s string
	switch tv := v.(type) {
	case nil:
		return nil
	case string:
		s = tv
	case json.Number:
		s = tv.String()
	case bool:
		s = strconv.FormatBool(tv)
	default:
		s = string(bytes.TrimSpace(raw))
	}
	return &s
}
