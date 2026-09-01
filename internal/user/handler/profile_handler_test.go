package handler

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
	"github.com/williamlabdev/saas-forge/internal/user/domain"
	"github.com/williamlabdev/saas-forge/internal/user/service"
	"github.com/williamlabdev/saas-forge/internal/user/service/mocks"
)

// GET /users/me and PATCH /users/{id} were the two routes with no test at any
// layer — not the handler, not the service. Both are on the path every signed-in
// session takes.

func TestHandler_CurrentUser(t *testing.T) {
	userID := uuid.New()
	svc := mocks.NewMockUserService(t)
	svc.EXPECT().
		CurrentUser(mock.Anything).
		Return(&service.UserDTO{ID: userID, Username: "jane_doe", Status: domain.StatusActive}, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/users/me", nil)
	rec := httptest.NewRecorder()
	newTestRouter(svc).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	env := decodeEnvelope(t, rec)
	require.Nil(t, env.Error)
}

// "me" must reach CurrentUser rather than be read as an id. chi prefers the
// static segment over {id} regardless of registration order, so the risk is not
// a mis-ordered route table — it is the route going missing or being renamed,
// after which /users/me falls through to userByID and answers INVALID_USER_ID.
// Verified by deleting the registration: this test fails with exactly that code.
func TestHandler_CurrentUser_IsNotParsedAsAnID(t *testing.T) {
	svc := mocks.NewMockUserService(t)
	svc.EXPECT().CurrentUser(mock.Anything).Return(&service.UserDTO{ID: uuid.New()}, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/users/me", nil)
	rec := httptest.NewRecorder()
	newTestRouter(svc).ServeHTTP(rec, req)

	// A 400 here would mean the request fell through to userByID.
	assert.NotEqual(t, http.StatusBadRequest, rec.Code, "/users/me was routed to the id handler")
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestHandler_CurrentUser_Unauthenticated(t *testing.T) {
	svc := mocks.NewMockUserService(t)
	svc.EXPECT().CurrentUser(mock.Anything).Return(nil, apperrors.ErrUnauthorized)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/users/me", nil)
	rec := httptest.NewRecorder()
	newTestRouter(svc).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	env := decodeEnvelope(t, rec)
	require.NotNil(t, env.Error)
	assert.Equal(t, "UNAUTHORIZED", env.Error.Code)
}

func strPtr(s string) *string { return &s }

// The pointers carry three distinct intentions and the handler must not
// collapse them: an absent key leaves the field alone, an explicit null is
// still "leave it alone" at this layer, and "" is an edit that clears it.
func TestHandler_PatchUser_ForwardsTheThreeStates(t *testing.T) {
	cases := []struct {
		name string
		body string
		want service.UpdateProfileInput
	}{
		{"absent keys change nothing", `{}`, service.UpdateProfileInput{}},
		{"explicit null is not an edit", `{"display_name":null,"phone":null}`, service.UpdateProfileInput{}},
		{"empty string clears the field", `{"display_name":"","phone":""}`,
			service.UpdateProfileInput{DisplayName: strPtr(""), Phone: strPtr("")}},
		{"values are passed through", `{"display_name":"Jane","phone":"+886900000000"}`,
			service.UpdateProfileInput{DisplayName: strPtr("Jane"), Phone: strPtr("+886900000000")}},
		{"one field at a time", `{"phone":"+886911111111"}`,
			service.UpdateProfileInput{Phone: strPtr("+886911111111")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			userID := uuid.New()
			svc := mocks.NewMockUserService(t)
			svc.EXPECT().
				UpdateProfile(mock.Anything, userID, tc.want).
				Return(&service.UserDTO{ID: userID}, nil)

			req := httptest.NewRequest(http.MethodPatch, "/api/v1/users/"+userID.String(),
				bytes.NewBufferString(tc.body))
			rec := httptest.NewRecorder()
			newTestRouter(svc).ServeHTTP(rec, req)

			assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		})
	}
}

func TestHandler_PatchUser_InvalidID(t *testing.T) {
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/users/not-a-uuid",
		bytes.NewBufferString(`{"display_name":"Jane"}`))
	rec := httptest.NewRecorder()
	// The mock is created with no expectations: reaching the service at all fails the test.
	newTestRouter(mocks.NewMockUserService(t)).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestHandler_PatchUser_InvalidJSON(t *testing.T) {
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/users/"+uuid.New().String(),
		bytes.NewBufferString(`{bad`))
	rec := httptest.NewRecorder()
	newTestRouter(mocks.NewMockUserService(t)).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// max=128 on display_name is enforced here, before the service sees it.
func TestHandler_PatchUser_OverlongDisplayNameIsRefused(t *testing.T) {
	body := `{"display_name":"` + strings.Repeat("a", 129) + `"}`
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/users/"+uuid.New().String(),
		bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	newTestRouter(mocks.NewMockUserService(t)).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}

func TestHandler_PatchUser_ServiceErrorKeepsItsStatus(t *testing.T) {
	userID := uuid.New()
	svc := mocks.NewMockUserService(t)
	svc.EXPECT().
		UpdateProfile(mock.Anything, userID, mock.Anything).
		Return(nil, apperrors.ErrNotFound)

	req := httptest.NewRequest(http.MethodPatch, "/api/v1/users/"+userID.String(),
		bytes.NewBufferString(`{"display_name":"Jane"}`))
	rec := httptest.NewRecorder()
	newTestRouter(svc).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
	env := decodeEnvelope(t, rec)
	require.NotNil(t, env.Error)
	assert.Equal(t, "NOT_FOUND", env.Error.Code)
}
