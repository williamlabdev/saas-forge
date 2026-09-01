package mock

import (
	"encoding/json"
	"io"
	"net/http"
	"sync"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/pkg/mcp"
)

// Server is an in-memory MCP stub for local dev and tests.
type Server struct {
	mu      sync.Mutex
	upserts []mcp.UpsertRequest
}

func NewServer() *Server {
	return &Server{}
}

// Handler serves the MCP user-state contract (see arch-mcp.md).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("PUT /v1/users/state", s.handleUpsert)
	mux.HandleFunc("GET /debug/upserts", s.handleDebugUpserts)
	return mux
}

func (s *Server) handleUpsert(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var raw struct {
		UserID         string `json:"user_id"`
		Status         string `json:"status"`
		StatusVersion  int    `json:"status_version"`
		EventType      string `json:"event_type"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	uid, err := uuid.Parse(raw.UserID)
	if err != nil {
		http.Error(w, "invalid user_id", http.StatusBadRequest)
		return
	}
	req := mcp.UpsertRequest{
		UserID:         uid,
		Status:         raw.Status,
		StatusVersion:  raw.StatusVersion,
		EventType:      raw.EventType,
		IdempotencyKey: raw.IdempotencyKey,
	}
	if req.IdempotencyKey == "" {
		req.IdempotencyKey = r.Header.Get("Idempotency-Key")
	}
	s.mu.Lock()
	s.upserts = append(s.upserts, req)
	s.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleDebugUpserts(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	list := append([]mcp.UpsertRequest(nil), s.upserts...)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(list)
}

// Upserts returns a copy of received upserts.
func (s *Server) Upserts() []mcp.UpsertRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]mcp.UpsertRequest(nil), s.upserts...)
}

// Reset clears recorded upserts (tests).
func (s *Server) Reset() {
	s.mu.Lock()
	s.upserts = nil
	s.mu.Unlock()
}

// Count returns the number of upserts received.
func (s *Server) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.upserts)
}
