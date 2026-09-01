package upstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
)

func serve(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// The gateway guard is GLOBAL middleware on the Domain API (TKT-R7), so it runs
// before authentication: a client that forgets this header is refused for a
// reason that has nothing to do with the credential it was debugging.
func TestEveryRequestCarriesTheBearerAndTheGatewaySecret(t *testing.T) {
	var gotAuth, gotGateway string
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotGateway = r.Header.Get(authn.GatewayHeader)
		_, _ = w.Write([]byte(`{"data":{},"error":null,"meta":{}}`))
	})

	c := NewClient(srv.URL, "gw-secret", 5*time.Second)
	_, err := c.GetType(context.Background(), "tok-123", "post")
	require.NoError(t, err)

	assert.Equal(t, "Bearer tok-123", gotAuth)
	assert.Equal(t, "gw-secret", gotGateway)
}

// The bearer is a per-call argument, not client state: the HTTP transport
// serves many callers from one process, and a stored token would make
// whose-credential a property of the process instead of the request.
func TestTheBearerIsPerCallNotPerClient(t *testing.T) {
	var seen []string
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"data":{},"error":null,"meta":{}}`))
	})

	c := NewClient(srv.URL, "", 5*time.Second)
	_, err := c.GetType(context.Background(), "alice", "post")
	require.NoError(t, err)
	_, err = c.GetType(context.Background(), "bob", "post")
	require.NoError(t, err)

	assert.Equal(t, []string{"Bearer alice", "Bearer bob"}, seen)
}

func TestErrorEnvelopesKeepTheirDetails(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"data":null,"error":{"code":"CONTENT_FIELD_ENUM_INVALID",` +
			`"message":"not an allowed value","details":{"allowed":["a","b"]}},"meta":{}}`))
	})

	c := NewClient(srv.URL, "", 5*time.Second)
	_, err := c.GetType(context.Background(), "tok", "post")

	se, ok := err.(*StatusError)
	require.True(t, ok, "the status must survive: 403-outside-scope and 404-no-such-type are different repairs")
	assert.Equal(t, http.StatusUnprocessableEntity, se.Status)
	assert.Equal(t, "CONTENT_FIELD_ENUM_INVALID", se.Body().Code)
	assert.Equal(t, []any{"a", "b"}, se.Body().Details["allowed"])
}

// A gateway rejection or a proxy error page is not an envelope. Callers must
// still get the same SHAPE back, or the tools' error contract holds only when
// the CMS itself answered.
func TestANonEnvelopeFailureStillProducesAStructuredBody(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>502 Bad Gateway</html>"))
	})

	c := NewClient(srv.URL, "", 5*time.Second)
	_, err := c.GetType(context.Background(), "tok", "post")

	se, ok := err.(*StatusError)
	require.True(t, ok)
	assert.Equal(t, "UPSTREAM_502", se.Body().Code)
	assert.Contains(t, se.Body().Message, "502")
}

// A 200 whose body will not parse must not read as success with no data —
// that renders to an agent as "this type has no fields" or "there are no
// entries", which is a fact rather than a fault.
func TestAnUnparseableSuccessIsAnError(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json at all"))
	})

	c := NewClient(srv.URL, "", 5*time.Second)
	data, err := c.GetType(context.Background(), "tok", "post")
	require.Error(t, err)
	assert.Nil(t, data)
}

// An envelope can carry an error alongside a 200 in principle; trusting the
// status alone would hand that body back as content.
func TestAnErrorEnvelopeIsAnErrorEvenWithA200(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":null,"error":{"code":"NOPE","message":"no"},"meta":{}}`))
	})

	c := NewClient(srv.URL, "", 5*time.Second)
	_, err := c.GetType(context.Background(), "tok", "post")
	require.Error(t, err)
	se, ok := err.(*StatusError)
	require.True(t, ok)
	assert.Equal(t, "NOPE", se.Body().Code)
}

func TestPathsAndQueriesMatchTheContentAPI(t *testing.T) {
	var path, escaped, query string
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		path, escaped, query = r.URL.Path, r.URL.EscapedPath(), r.URL.RawQuery
		_, _ = w.Write([]byte(`{"data":{},"error":null,"meta":{}}`))
	})
	c := NewClient(srv.URL, "", 5*time.Second)
	ctx := context.Background()

	_, err := c.ListTranslations(ctx, "tok", "post", "11111111-1111-1111-1111-111111111111")
	require.NoError(t, err)
	assert.Equal(t, "/api/v1/content/entries/11111111-1111-1111-1111-111111111111/translations", path)
	assert.Equal(t, "type=post", query)

	// A tool argument reaches the URL, so it is escaped rather than pasted in.
	//
	// The assertion is on the ESCAPED path because that is what goes on the
	// wire; r.URL.Path is the decoded convenience form and shows the slashes
	// back. That difference is the honest limit of this escaping: it makes the
	// request well-formed and keeps the name in one path segment as sent, but
	// the receiving router matches on the decoded path, so this is not what
	// stops a caller naming a type it may not touch. The whitelist is.
	_, err = c.GetType(ctx, "tok", "post/../../admin")
	require.NoError(t, err)
	assert.Equal(t, "/api/v1/content/types/post%2F..%2F..%2Fadmin", escaped)
}
