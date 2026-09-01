package domainapi

import (
	"context"
	"net/http"
	"testing"
)

func TestFanout_CountsClientCalls(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		dataResponse(w, http.StatusOK, map[string]any{"ok": true})
	})
	ctx, stats := WithFanout(context.Background())

	if _, err := c.GetMe(ctx, "tok"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetPlatformBillingSummary(ctx, "tok"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListPlatformStaff(ctx, "tok"); err != nil {
		t.Fatal(err)
	}

	if got := stats.Calls(); got != 3 {
		t.Fatalf("calls = %d, want 3", got)
	}
	if stats.DomainDuration() <= 0 {
		t.Fatal("domain duration should be positive")
	}
}

func TestFanout_ErrorsStillCounted(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"X","message":"boom"}}`))
	})
	ctx, stats := WithFanout(context.Background())
	if _, err := c.GetMe(ctx, "tok"); err == nil {
		t.Fatal("expected error")
	}
	if got := stats.Calls(); got != 1 {
		t.Fatalf("calls = %d, want 1", got)
	}
}

func TestFanout_NoCollectorIsNoop(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		dataResponse(w, http.StatusOK, map[string]any{"ok": true})
	})
	// Plain context: must not panic or allocate stats.
	if _, err := c.GetMe(context.Background(), "tok"); err != nil {
		t.Fatal(err)
	}
	if fanoutFromContext(context.Background()) != nil {
		t.Fatal("plain context should have no collector")
	}
}
