package domainapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newServer starts a test server whose handler is provided by the caller and
// returns a Client pointed at it.
func newServer(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return NewClient(srv.URL + "/") // trailing slash exercises TrimRight
}

// dataResponse writes a success envelope with the given data object.
func dataResponse(w http.ResponseWriter, status int, data any) {
	raw, _ := json.Marshal(data)
	env := map[string]any{"data": json.RawMessage(raw)}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(env)
}

func TestClient_Register(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/users" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("missing content-type")
		}
		dataResponse(w, http.StatusCreated, map[string]any{"id": "u1"})
	})
	got, err := c.Register(context.Background(), map[string]any{"email": "a@b.com"})
	if err != nil || got["id"] != "u1" {
		t.Fatalf("got %v err %v", got, err)
	}
}

func TestClient_Login(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		dataResponse(w, http.StatusOK, map[string]any{"access_token": "tok"})
	})
	got, err := c.Login(context.Background(), map[string]any{"email": "a@b.com"})
	if err != nil || got["access_token"] != "tok" {
		t.Fatalf("got %v err %v", got, err)
	}
}

func TestClient_GetUser_SendsBearer(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer abc" {
			t.Errorf("bearer not forwarded: %q", r.Header.Get("Authorization"))
		}
		if r.URL.Path != "/api/v1/users/u1" {
			t.Errorf("path=%s", r.URL.Path)
		}
		dataResponse(w, http.StatusOK, map[string]any{"id": "u1"})
	})
	got, err := c.GetUser(context.Background(), "u1", "abc")
	if err != nil || got["id"] != "u1" {
		t.Fatalf("got %v err %v", got, err)
	}
}

func TestClient_GetMe(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		dataResponse(w, http.StatusOK, map[string]any{"id": "me"})
	})
	got, err := c.GetMe(context.Background(), "abc")
	if err != nil || got["id"] != "me" {
		t.Fatalf("got %v err %v", got, err)
	}
}

func TestClient_ListNotifications(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("limit") != "7" {
			t.Errorf("limit=%s", r.URL.Query().Get("limit"))
		}
		dataResponse(w, http.StatusOK, map[string]any{"items": []any{
			map[string]any{"id": "n1"}, map[string]any{"id": "n2"},
		}})
	})
	got, err := c.ListNotifications(context.Background(), "abc", 7, false)
	if err != nil || len(got) != 2 || got[0]["id"] != "n1" {
		t.Fatalf("got %v err %v", got, err)
	}
}

func TestClient_ListNotifications_BadShape(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		dataResponse(w, http.StatusOK, map[string]any{"items": "not-an-array"})
	})
	if _, err := c.ListNotifications(context.Background(), "abc", 5, false); err == nil {
		t.Fatal("expected shape error")
	}
}

func TestClient_ListNotifications_BadItem(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		dataResponse(w, http.StatusOK, map[string]any{"items": []any{"not-an-object"}})
	})
	if _, err := c.ListNotifications(context.Background(), "abc", 5, false); err == nil {
		t.Fatal("expected item error")
	}
}

func TestClient_CreateNotification(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		dataResponse(w, http.StatusCreated, map[string]any{"id": "n1"})
	})
	got, err := c.CreateNotification(context.Background(), "abc", map[string]any{"title": "hi"})
	if err != nil || got["id"] != "n1" {
		t.Fatalf("got %v err %v", got, err)
	}
}

func TestClient_ListNotifications_UnreadTrue(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("unread") != "true" {
			t.Errorf("unread=%s, want true", r.URL.Query().Get("unread"))
		}
		dataResponse(w, http.StatusOK, map[string]any{"items": []any{}})
	})
	if _, err := c.ListNotifications(context.Background(), "abc", 5, true); err != nil {
		t.Fatalf("err %v", err)
	}
}

func TestClient_UnreadNotificationCount(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/notifications/unread-count" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		dataResponse(w, http.StatusOK, map[string]any{"count": float64(3)})
	})
	got, err := c.UnreadNotificationCount(context.Background(), "abc")
	if err != nil || got != 3 {
		t.Fatalf("got %d err %v", got, err)
	}
}

func TestClient_MarkNotificationRead(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/api/v1/notifications/n1/read" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		dataResponse(w, http.StatusOK, map[string]any{"id": "n1", "read": true})
	})
	got, err := c.MarkNotificationRead(context.Background(), "abc", "n1")
	if err != nil || got["id"] != "n1" || got["read"] != true {
		t.Fatalf("got %v err %v", got, err)
	}
}

func TestClient_MarkAllNotificationsRead(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/notifications/read-all" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		dataResponse(w, http.StatusOK, map[string]any{"count": float64(5)})
	})
	got, err := c.MarkAllNotificationsRead(context.Background(), "abc")
	if err != nil || got != 5 {
		t.Fatalf("got %d err %v", got, err)
	}
}

func TestClient_ListPlatformApps_BuildsQuery(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("limit") != "10" || q.Get("offset") != "20" || q.Get("q") != "srch" || q.Get("status") != "active" {
			t.Errorf("query=%v", q)
		}
		dataResponse(w, http.StatusOK, map[string]any{"total": float64(0)})
	})
	if _, err := c.ListPlatformApps(context.Background(), "abc", "srch", "active", 10, 20); err != nil {
		t.Fatal(err)
	}
}

func TestClient_ListPlatformApps_OmitsEmptyFilters(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Has("q") || q.Has("status") {
			t.Errorf("empty filters should be omitted: %v", q)
		}
		dataResponse(w, http.StatusOK, map[string]any{})
	})
	if _, err := c.ListPlatformApps(context.Background(), "abc", "", "", 5, 0); err != nil {
		t.Fatal(err)
	}
}

func TestClient_CreatePlatformApp(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		dataResponse(w, http.StatusCreated, map[string]any{"id": "app1"})
	})
	got, err := c.CreatePlatformApp(context.Background(), "abc", map[string]any{"name": "A"})
	if err != nil || got["id"] != "app1" {
		t.Fatalf("got %v err %v", got, err)
	}
}

func TestClient_UpdatePlatformAppStatus(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/api/v1/platform/apps/app1/status" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["status"] != "paused" {
			t.Errorf("status body=%v", body)
		}
		dataResponse(w, http.StatusOK, map[string]any{"id": "app1", "status": "paused"})
	})
	got, err := c.UpdatePlatformAppStatus(context.Background(), "abc", "app1", "paused")
	if err != nil || got["status"] != "paused" {
		t.Fatalf("got %v err %v", got, err)
	}
}

func TestClient_PlatformReadEndpoints(t *testing.T) {
	// Exercise the remaining GET wrappers against a permissive server.
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		dataResponse(w, http.StatusOK, map[string]any{"ok": true})
	})
	ctx := context.Background()
	if _, err := c.GetPlatformBillingSummary(ctx, "abc"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListPlatformInvoices(ctx, "abc", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListPlatformStaff(ctx, "abc"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListPlatformAlerts(ctx, "abc", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetPlatformReportsSummary(ctx, "abc"); err != nil {
		t.Fatal(err)
	}
}

func TestClient_APIError(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"code": "CONFLICT", "message": "exists"},
		})
	})
	_, err := c.Register(context.Background(), map[string]any{})
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("want *APIError, got %T", err)
	}
	if apiErr.Code != "CONFLICT" || apiErr.HTTPStatus != http.StatusConflict {
		t.Fatalf("unexpected: %+v", apiErr)
	}
	if apiErr.Error() != "CONFLICT: exists" {
		t.Fatalf("Error()=%s", apiErr.Error())
	}
}

func TestClient_UnexpectedStatus(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		// success envelope but a status not in the accepted list
		dataResponse(w, http.StatusAccepted, map[string]any{"id": "x"})
	})
	if _, err := c.Login(context.Background(), map[string]any{}); err == nil {
		t.Fatal("expected unexpected-status error")
	}
}

func TestClient_DecodeError(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not json"))
	})
	if _, err := c.Login(context.Background(), map[string]any{}); err == nil {
		t.Fatal("expected decode error")
	}
}

func TestClient_NullData(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":null}`))
	})
	got, err := c.Login(context.Background(), map[string]any{})
	if err != nil || len(got) != 0 {
		t.Fatalf("want empty map, got %v err %v", got, err)
	}
}

func TestClient_RequestError(t *testing.T) {
	// Point the client at an unroutable base so client.Do fails.
	c := NewClient("http://127.0.0.1:0")
	if _, err := c.GetMe(context.Background(), "abc"); err == nil {
		t.Fatal("expected transport error")
	}
}

func TestClient_ForwardsGatewaySecretWhenSet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Gateway-Secret") != "gw-sec" {
			t.Errorf("gateway secret not forwarded: %q", r.Header.Get("X-Gateway-Secret"))
		}
		dataResponse(w, http.StatusOK, map[string]any{"ok": true})
	}))
	t.Cleanup(srv.Close)
	c := NewClientWithGateway(srv.URL, "gw-sec")
	if _, err := c.Login(context.Background(), map[string]any{"email": "a@b.com"}); err != nil {
		t.Fatalf("err %v", err)
	}
}

func TestClient_NoGatewayHeaderWhenUnset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Header["X-Gateway-Secret"]; ok {
			t.Errorf("unexpected gateway header when secret unset")
		}
		dataResponse(w, http.StatusOK, map[string]any{"ok": true})
	}))
	t.Cleanup(srv.Close)
	c := NewClient(srv.URL)
	if _, err := c.Login(context.Background(), map[string]any{"email": "a@b.com"}); err != nil {
		t.Fatalf("err %v", err)
	}
}

// Agent credentials (ADR-015 乙案). Three shapes the other endpoints do not
// have between them: a bare-array payload, a 204 with no body at all, and a
// bearer that is the request's INPUT rather than only its authentication.

func TestClient_ListAgentCredentials_DecodesBareArray(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/auth/agent-tokens" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer abc" {
			t.Errorf("bearer not forwarded: %q", r.Header.Get("Authorization"))
		}
		dataResponse(w, http.StatusOK, []any{
			map[string]any{"id": "c1", "agent_id": "import-bot"},
			map[string]any{"id": "c2", "agent_id": "sync-bot"},
		})
	})
	got, err := c.ListAgentCredentials(context.Background(), "abc")
	if err != nil {
		t.Fatalf("err %v", err)
	}
	// Two rows, in order: the registry is a list and the console renders it as
	// one, so "decoded something" is not the assertion — "decoded both" is.
	if len(got) != 2 || got[0]["id"] != "c1" || got[1]["agent_id"] != "sync-bot" {
		t.Fatalf("got %v", got)
	}
}

// An empty registry is an answer, not a failure — and it is the answer a tenant
// sees on day one, so it must not arrive as a decode error.
func TestClient_ListAgentCredentials_EmptyRegistryIsNotAnError(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		dataResponse(w, http.StatusOK, []any{})
	})
	got, err := c.ListAgentCredentials(context.Background(), "abc")
	if err != nil {
		t.Fatalf("err %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("got %v, want empty non-nil", got)
	}
}

func TestClient_IssueAgentCredential_ForwardsBearerAndBody(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/auth/agent-tokens" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		// The bearer is what gets downgraded (ADR-013 補裁 O-3). If the BFF
		// dropped it, the domain would answer 400 "nothing to downgrade" and
		// the console would report a validation problem for a working login.
		if r.Header.Get("Authorization") != "Bearer minter-token" {
			t.Errorf("bearer not forwarded: %q", r.Header.Get("Authorization"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["agent_id"] != "import-bot" {
			t.Errorf("agent_id=%v", body["agent_id"])
		}
		types, _ := body["allowed_types"].([]any)
		if len(types) != 1 || types[0] != "article" {
			t.Errorf("allowed_types=%v", body["allowed_types"])
		}
		dataResponse(w, http.StatusCreated, map[string]any{"id": "c1", "token": "agent-jwt"})
	})
	got, err := c.IssueAgentCredential(context.Background(), "minter-token", map[string]any{
		"agent_id":      "import-bot",
		"allowed_types": []string{"article"},
	})
	if err != nil || got["token"] != "agent-jwt" {
		t.Fatalf("got %v err %v", got, err)
	}
}

// 204 carries no body. Decoding an envelope out of nothing is a decode error,
// so a revoke that WORKED would surface as a failure — with the row already
// gone. This is the one the shared path has to special-case.
func TestClient_RevokeAgentCredential_204HasNoEnvelope(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/api/v1/auth/agent-tokens/c1" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer abc" {
			t.Errorf("bearer not forwarded: %q", r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusNoContent)
	})
	if err := c.RevokeAgentCredential(context.Background(), "abc", "c1"); err != nil {
		t.Fatalf("err %v", err)
	}
}

// The failure path still speaks the envelope: skipping the decode for 204 must
// not skip it for the 404 that says somebody else revoked it first.
func TestClient_RevokeAgentCredential_SurfacesDomainError(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"NOT_FOUND","message":"credential not found"}}`))
	})
	err := c.RevokeAgentCredential(context.Background(), "abc", "c1")
	ae, ok := err.(*APIError)
	if !ok {
		t.Fatalf("want *APIError, got %T: %v", err, err)
	}
	if ae.Code != "NOT_FOUND" || ae.HTTPStatus != http.StatusNotFound {
		t.Fatalf("got %+v", ae)
	}
}

// Refresh is unauthenticated (like Login): the refresh token IS the
// credential, so no Authorization header should be sent.
func TestClient_Refresh(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/auth/refresh" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "" {
			t.Errorf("refresh must not send a bearer, got %q", r.Header.Get("Authorization"))
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["refresh_token"] != "old-rt" {
			t.Errorf("refresh_token=%v", body["refresh_token"])
		}
		dataResponse(w, http.StatusOK, map[string]any{
			"access_token": "new-at", "refresh_token": "new-rt", "user_id": "u1",
		})
	})
	got, err := c.Refresh(context.Background(), "old-rt")
	if err != nil || got["access_token"] != "new-at" || got["refresh_token"] != "new-rt" {
		t.Fatalf("got %v err %v", got, err)
	}
}

// A revoked, expired, or already-redeemed refresh token comes back as core's
// AUTH_REFRESH_REVOKED (401) — the code the console reads to decide "sign the
// user out" (see mapAPIError). This pins that the client surfaces it intact.
func TestClient_Refresh_Revoked(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"code": "AUTH_REFRESH_REVOKED", "message": "refresh token revoked or expired"},
		})
	})
	_, err := c.Refresh(context.Background(), "spent-rt")
	ae, ok := err.(*APIError)
	if !ok {
		t.Fatalf("want *APIError, got %T: %v", err, err)
	}
	if ae.Code != "AUTH_REFRESH_REVOKED" || ae.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("got %+v", ae)
	}
}

// Logout is also unauthenticated — the token being revoked is itself the
// credential.
func TestClient_Logout(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/auth/logout" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "" {
			t.Errorf("logout must not send a bearer, got %q", r.Header.Get("Authorization"))
		}
		dataResponse(w, http.StatusOK, map[string]any{"ok": true})
	})
	if err := c.Logout(context.Background(), "rt-1"); err != nil {
		t.Fatalf("err %v", err)
	}
}

// The scenario the console actually depends on: logout revokes the refresh
// token, so a refresh attempted with it afterwards must be rejected with the
// same AUTH_REFRESH_REVOKED code as any other spent token — not a generic
// error. The fake server below plays core's role (one refresh token, revoked
// by logout) rather than asserting each client call in isolation.
func TestClient_Logout_ThenRefreshRejected(t *testing.T) {
	revoked := false
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/logout":
			revoked = true
			dataResponse(w, http.StatusOK, map[string]any{"ok": true})
		case "/api/v1/auth/refresh":
			if revoked {
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]any{"code": "AUTH_REFRESH_REVOKED", "message": "refresh token revoked or expired"},
				})
				return
			}
			dataResponse(w, http.StatusOK, map[string]any{"access_token": "at", "refresh_token": "rt", "user_id": "u1"})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})

	if err := c.Logout(context.Background(), "rt-1"); err != nil {
		t.Fatalf("logout err %v", err)
	}

	_, err := c.Refresh(context.Background(), "rt-1")
	ae, ok := err.(*APIError)
	if !ok {
		t.Fatalf("want *APIError after logout, got %T: %v", err, err)
	}
	if ae.Code != "AUTH_REFRESH_REVOKED" {
		t.Fatalf("code=%q, want AUTH_REFRESH_REVOKED", ae.Code)
	}
}
