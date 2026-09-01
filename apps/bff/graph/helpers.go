package graph

import (
	"context"
	"fmt"
	"strings"

	"github.com/vektah/gqlparser/v2/gqlerror"

	"github.com/williamlabdev/saas-forge/apps/bff/internal/domainapi"
)

func bearerFromContext(ctx context.Context) (string, error) {
	v := ctx.Value(AuthHeaderKey{})
	s, _ := v.(string)
	s = strings.TrimPrefix(strings.TrimSpace(s), "Bearer ")
	if s == "" {
		return "", fmt.Errorf("unauthorized")
	}
	return s, nil
}

// AuthHeaderKey carries Authorization into gqlgen context.
type AuthHeaderKey struct{}

func mapUser(m map[string]any) *User {
	u := &User{
		ID:       str(m["id"]),
		Username: str(m["username"]),
		Status:   str(m["status"]),
	}
	if e := str(m["email"]); e != "" {
		u.Email = &e
	}
	if d := str(m["display_name"]); d != "" {
		u.DisplayName = &d
	}
	return u
}

func mapPlatformApp(m map[string]any) *PlatformApp {
	return &PlatformApp{
		ID:        str(m["id"]),
		Name:      str(m["name"]),
		TenantID:  str(m["tenant_id"]),
		Owner:     str(m["owner"]),
		Status:    str(m["status"]),
		CreatedAt: str(m["created_at"]),
		UpdatedAt: str(m["updated_at"]),
	}
}

func mapPlatformAppConnection(m map[string]any) *PlatformAppConnection {
	itemsRaw, ok := m["items"].([]any)
	if !ok {
		return &PlatformAppConnection{}
	}
	items := make([]*PlatformApp, 0, len(itemsRaw))
	for _, it := range itemsRaw {
		row, ok := it.(map[string]any)
		if !ok {
			continue
		}
		items = append(items, mapPlatformApp(row))
	}
	return &PlatformAppConnection{
		Items:  items,
		Total:  intFromAny(m["total"]),
		Limit:  intFromAny(m["limit"]),
		Offset: intFromAny(m["offset"]),
	}
}

func mapNotification(m map[string]any) *Notification {
	n := &Notification{
		ID:        str(m["id"]),
		Title:     str(m["title"]),
		Body:      str(m["body"]),
		Read:      m["read"] == true,
		CreatedAt: str(m["created_at"]),
	}
	return n
}

// mapAPIError carries the domain's answer across the GraphQL boundary.
//
// The message keeps its "CODE: message" shape — anything already reading it
// would break otherwise — and the code and status are ADDED as extensions,
// because parsing them back out of a sentence is how a client ends up matching
// on prose. A caller that needs to tell 403 apart from 404 (the console's
// agent-credential page does) reads `extensions.code`.
func mapAPIError(err error) error {
	if ae, ok := err.(*domainapi.APIError); ok {
		return &gqlerror.Error{
			Message: fmt.Sprintf("%s: %s", ae.Code, ae.Message),
			Extensions: map[string]any{
				"code":   ae.Code,
				"status": ae.HTTPStatus,
			},
		}
	}
	return err
}

func intFromAny(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case int64:
		return int(t)
	default:
		return 0
	}
}

func str(v any) string {
	if v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	default:
		return fmt.Sprint(t)
	}
}

func authPayloadFromLogin(ctx context.Context, client *domainapi.Client, tokens map[string]any) (*AuthPayload, error) {
	bearer := str(tokens["access_token"])
	userID := str(tokens["user_id"])
	user, err := client.GetUser(ctx, userID, bearer)
	if err != nil {
		return nil, mapAPIError(err)
	}
	return &AuthPayload{
		User:         mapUser(user),
		AccessToken:  bearer,
		RefreshToken: str(tokens["refresh_token"]),
		ExpiresIn:    intFromAny(tokens["expires_in"]),
		TokenType:    str(tokens["token_type"]),
	}, nil
}
