package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// HTTPClient calls MCP REST endpoints.
type HTTPClient struct {
	baseURL    string
	httpClient *http.Client
}

func NewHTTPClient(baseURL string) *HTTPClient {
	return &HTTPClient{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

func (c *HTTPClient) UpsertUserState(ctx context.Context, req UpsertRequest) error {
	body, err := json.Marshal(map[string]any{
		"user_id":         req.UserID.String(),
		"status":          req.Status,
		"status_version":  req.StatusVersion,
		"event_type":      req.EventType,
		"idempotency_key": req.IdempotencyKey,
	})
	if err != nil {
		return err
	}

	url := c.baseURL + "/v1/users/state"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Idempotency-Key", req.IdempotencyKey)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return fmt.Errorf("mcp: upsert status %d", resp.StatusCode)
}

// NoopClient logs and succeeds when MCP is not configured (local dev).
type NoopClient struct{}

func (NoopClient) UpsertUserState(context.Context, UpsertRequest) error {
	return nil
}
