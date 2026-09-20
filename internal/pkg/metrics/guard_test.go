package metrics

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBearerGuard(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	t.Run("no token configured passes through", func(t *testing.T) {
		rec := httptest.NewRecorder()
		BearerGuard("")(next).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("requires bearer when configured", func(t *testing.T) {
		h := BearerGuard("secret")(next)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		assert.Equal(t, http.StatusUnauthorized, rec.Code)

		req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		req.Header.Set("Authorization", "Bearer secret")
		rec2 := httptest.NewRecorder()
		h.ServeHTTP(rec2, req)
		assert.Equal(t, http.StatusOK, rec2.Code)
	})
}
