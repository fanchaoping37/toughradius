package adminapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/talkincode/toughradius/v9/internal/app"
	"github.com/talkincode/toughradius/v9/internal/domain"
	"gorm.io/gorm"
)

func portalCtx(t *testing.T, db *gorm.DB, appCtx app.AppContext, method, path, body string, id ...string) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	e := setupTestEcho()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := CreateTestContext(e, db, req, rec, appCtx)
	if len(id) > 0 {
		c.SetParamNames("id")
		c.SetParamValues(id[0])
	}
	return c, rec
}

func decodePortalData(t *testing.T, rec *httptest.ResponseRecorder, dst interface{}) {
	t.Helper()
	var resp Response
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	raw, err := json.Marshal(resp.Data)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, dst))
}

func TestPortalNASCRUD(t *testing.T) {
	db := setupTestDB(t)
	appCtx := setupTestApp(t, db)

	c, rec := portalCtx(t, db, appCtx, http.MethodPost, "/api/v1/portal-nases",
		`{"name":"AC-1","addr":"10.0.0.1","port":2000,"secret":"portal-secret"}`)
	require.NoError(t, CreatePortalNAS(c))
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var created domain.PortalNas
	decodePortalData(t, rec, &created)
	assert.Equal(t, "enabled", created.Status)
	assert.Equal(t, 2000, created.Port)

	id := strconv.FormatInt(created.ID, 10)
	c, rec = portalCtx(t, db, appCtx, http.MethodGet, "/api/v1/portal-nases/"+id, "", id)
	require.NoError(t, GetPortalNAS(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	var fetched domain.PortalNas
	decodePortalData(t, rec, &fetched)
	assert.Equal(t, "AC-1", fetched.Name)

	c, rec = portalCtx(t, db, appCtx, http.MethodPut, "/api/v1/portal-nases/"+id,
		`{"status":"disabled"}`, id)
	require.NoError(t, UpdatePortalNAS(c))
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	decodePortalData(t, rec, &fetched)
	assert.Equal(t, "disabled", fetched.Status)

	c, rec = portalCtx(t, db, appCtx, http.MethodDelete, "/api/v1/portal-nases/"+id, "", id)
	require.NoError(t, DeletePortalNAS(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	c, rec = portalCtx(t, db, appCtx, http.MethodGet, "/api/v1/portal-nases/"+id, "", id)
	require.NoError(t, GetPortalNAS(c))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestPortalHTTPBridgeDisabled(t *testing.T) {
	db := setupTestDB(t)
	appCtx := setupTestApp(t, db)

	for _, handler := range []echo.HandlerFunc{PortalChallenge, PortalAuthenticate, PortalLogout} {
		c, rec := portalCtx(t, db, appCtx, http.MethodPost, "/api/v1/portal/auth", `{}`)
		require.NoError(t, handler(c))
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	}
	c, rec := portalCtx(t, db, appCtx, http.MethodGet, "/api/v1/portal/session?user_ip=1.2.3.4", "")
	require.NoError(t, PortalSession(c))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}
