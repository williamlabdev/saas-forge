package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
	"github.com/williamlabdev/saas-forge/internal/pkg/response"
	"github.com/williamlabdev/saas-forge/internal/user/domain"
	"github.com/williamlabdev/saas-forge/internal/user/service"
	"github.com/williamlabdev/saas-forge/internal/user/service/mocks"
)

func newTestRouter(svc service.UserService) http.Handler {
	return NewRouter(NewHandler(svc))
}

func decodeEnvelope(t *testing.T, rec *httptest.ResponseRecorder) response.Envelope {
	t.Helper()
	var env response.Envelope
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&env))
	return env
}

func TestHandler_CreateUser(t *testing.T) {
	userID := uuid.New()
	now := time.Now().UTC()
	svc := mocks.NewMockUserService(t)
	svc.EXPECT().
		Register(mock.Anything, service.RegisterInput{
			Username: "jane_doe",
			Email:    "jane@example.com",
			Password: "password12",
		}).
		Return(&service.UserDTO{
			ID:        userID,
			Username:  "jane_doe",
			Email:     "jane@example.com",
			Status:    domain.StatusActive,
			CreatedAt: now,
			UpdatedAt: now,
		}, false, nil)

	router := newTestRouter(svc)
	body := `{"username":"jane_doe","email":"jane@example.com","password":"password12"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/users", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusCreated, rec.Code)
	env := decodeEnvelope(t, rec)
	require.Nil(t, env.Error)
	data, ok := env.Data.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, userID.String(), data["id"])
}

func TestHandler_CreateUser_InvalidJSON(t *testing.T) {
	router := newTestRouter(mocks.NewMockUserService(t))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/users", bytes.NewBufferString(`{`))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	env := decodeEnvelope(t, rec)
	require.NotNil(t, env.Error)
	assert.Equal(t, "INVALID_JSON", env.Error.Code)
}

func TestHandler_UserByID(t *testing.T) {
	userID := uuid.New()
	svc := mocks.NewMockUserService(t)
	svc.EXPECT().
		ByID(mock.Anything, userID).
		Return(&service.UserDTO{ID: userID, Username: "jane_doe", Status: domain.StatusActive}, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/users/"+userID.String(), nil)
	rec := httptest.NewRecorder()
	newTestRouter(svc).ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestHandler_UserByID_NotFound(t *testing.T) {
	userID := uuid.New()
	svc := mocks.NewMockUserService(t)
	svc.EXPECT().
		ByID(mock.Anything, userID).
		Return(nil, apperrors.ErrNotFound)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/users/"+userID.String(), nil)
	rec := httptest.NewRecorder()
	newTestRouter(svc).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
	env := decodeEnvelope(t, rec)
	require.NotNil(t, env.Error)
	assert.Equal(t, "NOT_FOUND", env.Error.Code)
}

func TestHandler_UserByID_InvalidID(t *testing.T) {
	router := newTestRouter(mocks.NewMockUserService(t))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/users/not-a-uuid", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestHandler_DeleteUser(t *testing.T) {
	userID := uuid.New()
	svc := mocks.NewMockUserService(t)
	svc.EXPECT().Delete(mock.Anything, userID).Return(nil)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/users/"+userID.String(), nil)
	rec := httptest.NewRecorder()
	newTestRouter(svc).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestHandler_PatchPreferences(t *testing.T) {
	userID := uuid.New()
	svc := mocks.NewMockUserService(t)
	svc.EXPECT().
		UpdatePreferences(mock.Anything, userID, service.PreferencesInput{
			Merge:       true,
			Preferences: domain.Preferences{"theme": "light"},
		}).
		Return(&service.UserDTO{ID: userID, Preferences: domain.Preferences{"theme": "light"}}, nil)

	body := `{"merge":true,"preferences":{"theme":"light"}}`
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/users/"+userID.String()+"/preferences", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	newTestRouter(svc).ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}
