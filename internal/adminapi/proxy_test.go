package adminapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"strconv"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/talkincode/toughradius/v9/internal/app"
	"github.com/talkincode/toughradius/v9/internal/domain"
	"gorm.io/gorm"
)

// decodeData unwraps the standard {code,data} response envelope into dst.
func decodeData(t *testing.T, rec *httptest.ResponseRecorder, dst interface{}) {
	t.Helper()
	var resp Response
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	raw, err := json.Marshal(resp.Data)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, dst))
}

// proxyAPICtx builds an echo context with a super-admin operator for a proxy
// management API call, mirroring newSessionActionCtx.
func proxyAPICtx(t *testing.T, db *gorm.DB, appCtx app.AppContext, method, path, body string, idParam ...string) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	e := setupTestEcho()
	var reqBody io.Reader
	if body != "" {
		reqBody = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reqBody)
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := CreateTestContext(e, db, req, rec, appCtx)
	if len(idParam) > 0 {
		c.SetParamNames("id")
		c.SetParamValues(idParam[0])
	}
	return c, rec
}

func TestUpstreamCRUD(t *testing.T) {
	db := setupTestDB(t)
	appCtx := setupTestApp(t, db)

	body := `{"name":"carrier-a","addr":"10.1.1.1","secret":"upstream-secret","role":"proxy","weight":2}`
	c, rec := proxyAPICtx(t, db, appCtx, http.MethodPost, "/api/v1/radius-upstreams", body)
	require.NoError(t, CreateUpstream(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var created domain.RadiusUpstream
	decodeData(t, rec, &created)
	assert.Equal(t, "carrier-a", created.Name)
	assert.Equal(t, 1812, created.AuthPort)  // default applied
	assert.Equal(t, 1813, created.AcctPort)  // default applied
	assert.Equal(t, 3799, created.CoaPort)   // default applied
	assert.Equal(t, 2, created.Weight)

	// Get
	c, rec = proxyAPICtx(t, db, appCtx, http.MethodGet, "/api/v1/radius-upstreams/"+idStr(created.ID), "", idStr(created.ID))
	require.NoError(t, GetUpstream(c))
	require.Equal(t, http.StatusOK, rec.Code)

	// Update
	c, rec = proxyAPICtx(t, db, appCtx, http.MethodPut, "/api/v1/radius-upstreams/"+idStr(created.ID),
		`{"status":"disabled","remark":"maint"}`, idStr(created.ID))
	require.NoError(t, UpdateUpstream(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var updated domain.RadiusUpstream
	decodeData(t, rec, &updated)
	assert.Equal(t, "disabled", updated.Status)
	assert.Equal(t, "maint", updated.Remark)
	assert.Equal(t, "carrier-a", updated.Name) // untouched field preserved

	// List
	c, rec = proxyAPICtx(t, db, appCtx, http.MethodGet, "/api/v1/radius-upstreams", "")
	require.NoError(t, ListUpstreams(c))
	require.Equal(t, http.StatusOK, rec.Code)

	// Delete
	c, rec = proxyAPICtx(t, db, appCtx, http.MethodDelete, "/api/v1/radius-upstreams/"+idStr(created.ID), "", idStr(created.ID))
	require.NoError(t, DeleteUpstream(c))
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestCreateUpstreamValidation(t *testing.T) {
	db := setupTestDB(t)
	appCtx := setupTestApp(t, db)

	// Missing required fields
	c, rec := proxyAPICtx(t, db, appCtx, http.MethodPost, "/api/v1/radius-upstreams", `{"name":""}`)
	require.NoError(t, CreateUpstream(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRealmRouteCRUDAndUnknownUpstream(t *testing.T) {
	db := setupTestDB(t)
	appCtx := setupTestApp(t, db)

	up := &domain.RadiusUpstream{Name: "isp-a", Addr: "10.0.0.9", Secret: "route-secret-x", Role: domain.UpstreamRoleProxy, Status: "enabled"}
	require.NoError(t, db.Create(up).Error)

	// Unknown upstream id is rejected
	c, rec := proxyAPICtx(t, db, appCtx, http.MethodPost, "/api/v1/radius-realm-routes",
		`{"realm":"campus.edu","upstream_ids":"999","strategy":"single"}`)
	require.NoError(t, CreateRealmRoute(c))
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)

	// Valid create
	c, rec = proxyAPICtx(t, db, appCtx, http.MethodPost, "/api/v1/radius-realm-routes",
		`{"realm":"campus.edu","upstream_ids":"`+idStr(up.ID)+`","strategy":"weight","user_prefix":"isp_","mapped_password":"carrier-pwd","acct_mode":"both"}`)
	require.NoError(t, CreateRealmRoute(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var route domain.RadiusRealmRoute
	decodeData(t, rec, &route)
	assert.Equal(t, "campus.edu", route.Realm)
	assert.Equal(t, domain.RealmStrategyWeight, route.Strategy)
	assert.Equal(t, "isp_", route.UserPrefix)
	assert.Equal(t, domain.AcctModeBoth, route.AcctMode)

	// Upstream deletion is refused while referenced
	c, rec = proxyAPICtx(t, db, appCtx, http.MethodDelete, "/api/v1/radius-upstreams/"+idStr(up.ID), "", idStr(up.ID))
	require.NoError(t, DeleteUpstream(c))
	assert.Equal(t, http.StatusConflict, rec.Code)

	// Delete route then upstream succeeds
	c, rec = proxyAPICtx(t, db, appCtx, http.MethodDelete, "/api/v1/radius-realm-routes/"+idStr(route.ID), "", idStr(route.ID))
	require.NoError(t, DeleteRealmRoute(c))
	require.Equal(t, http.StatusOK, rec.Code)

	c, rec = proxyAPICtx(t, db, appCtx, http.MethodDelete, "/api/v1/radius-upstreams/"+idStr(up.ID), "", idStr(up.ID))
	require.NoError(t, DeleteUpstream(c))
	require.Equal(t, http.StatusOK, rec.Code)
}

// idStr renders a model id as its decimal string form.
func idStr(id int64) string {
	return strconv.FormatInt(id, 10)
}
