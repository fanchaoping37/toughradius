package adminapi

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"
	"github.com/talkincode/toughradius/v9/internal/domain"
	"github.com/talkincode/toughradius/v9/internal/webserver"
)

// RADIUS proxy subsystem management API: upstream servers and realm routes.

// upstreamPayload is the create/update request body for proxy upstreams.
type upstreamPayload struct {
	NodeId   int64  `json:"node_id,string" validate:"gte=0"`
	Name     string `json:"name" validate:"required,min=1,max=100"`
	Addr     string `json:"addr" validate:"required,min=1,max=100"`
	AuthPort *int   `json:"auth_port" validate:"omitempty,port"`
	AcctPort *int   `json:"acct_port" validate:"omitempty,port"`
	CoaPort  *int   `json:"coa_port" validate:"omitempty,port"`
	Secret   string `json:"secret" validate:"required,min=6,max=100"`
	Role     string `json:"role" validate:"omitempty,oneof=proxy replicate"`
	Weight   *int   `json:"weight" validate:"omitempty,gte=0"`
	Status   string `json:"status" validate:"omitempty,oneof=enabled disabled"`
	Remark   string `json:"remark" validate:"omitempty,max=500"`
}

// upstreamUpdatePayload relaxes validation rules for partial updates.
type upstreamUpdatePayload struct {
	NodeId   int64  `json:"node_id,string" validate:"omitempty,gte=0"`
	Name     string `json:"name" validate:"omitempty,min=1,max=100"`
	Addr     string `json:"addr" validate:"omitempty,min=1,max=100"`
	AuthPort *int   `json:"auth_port" validate:"omitempty,port"`
	AcctPort *int   `json:"acct_port" validate:"omitempty,port"`
	CoaPort  *int   `json:"coa_port" validate:"omitempty,port"`
	Secret   string `json:"secret" validate:"omitempty,min=6,max=100"`
	Role     string `json:"role" validate:"omitempty,oneof=proxy replicate"`
	Weight   *int   `json:"weight" validate:"omitempty,gte=0"`
	Status   string `json:"status" validate:"omitempty,oneof=enabled disabled"`
	Remark   string `json:"remark" validate:"omitempty,max=500"`
}

// realmRouteUpdatePayload relaxes validation rules for partial updates.
type realmRouteUpdatePayload struct {
	NodeId         int64  `json:"node_id,string" validate:"omitempty,gte=0"`
	Realm          string `json:"realm" validate:"omitempty,min=1,max=100"`
	Strategy       string `json:"strategy" validate:"omitempty,oneof=single round_robin weight"`
	UpstreamIds    string `json:"upstream_ids" validate:"omitempty,min=1,max=500"`
	UserPrefix     string `json:"user_prefix" validate:"omitempty,max=100"`
	UserSuffix     string `json:"user_suffix" validate:"omitempty,max=100"`
	MappedPassword string `json:"mapped_password" validate:"omitempty,max=100"`
	AcctMode       string `json:"acct_mode" validate:"omitempty,oneof=both forward local"`
	Status         string `json:"status" validate:"omitempty,oneof=enabled disabled"`
	Remark         string `json:"remark" validate:"omitempty,max=500"`
}

// realmRoutePayload is the create/update request body for realm routes.
type realmRoutePayload struct {
	NodeId         int64  `json:"node_id,string" validate:"gte=0"`
	Realm          string `json:"realm" validate:"required,min=1,max=100"`
	Strategy       string `json:"strategy" validate:"omitempty,oneof=single round_robin weight"`
	UpstreamIds    string `json:"upstream_ids" validate:"required,min=1,max=500"`
	UserPrefix     string `json:"user_prefix" validate:"omitempty,max=100"`
	UserSuffix     string `json:"user_suffix" validate:"omitempty,max=100"`
	MappedPassword string `json:"mapped_password" validate:"omitempty,max=100"`
	AcctMode       string `json:"acct_mode" validate:"omitempty,oneof=both forward local"`
	Status         string `json:"status" validate:"omitempty,oneof=enabled disabled"`
	Remark         string `json:"remark" validate:"omitempty,max=500"`
}

// ListUpstreams handles GET /api/v1/radius-upstreams.
//
// @Summary list proxy upstream servers
// @Tags RadiusProxy
// @Success 200 {object} ListResponse
// @Router /api/v1/radius-upstreams [get]
func ListUpstreams(c echo.Context) error {
	db := GetDB(c)
	page, _ := strconv.Atoi(c.QueryParam("page"))
	perPage, _ := strconv.Atoi(c.QueryParam("perPage"))
	if page < 1 {
		page = 1
	}
	if perPage < 1 || perPage > 100 {
		perPage = 20
	}

	var total int64
	var items []domain.RadiusUpstream
	query := db.Model(&domain.RadiusUpstream{})
	if role := strings.TrimSpace(c.QueryParam("role")); role != "" {
		query = query.Where("role = ?", role)
	}
	if status := strings.TrimSpace(c.QueryParam("status")); status != "" {
		query = query.Where("status = ?", status)
	}
	query.Count(&total)
	query.Order("id DESC").Limit(perPage).Offset((page - 1) * perPage).Find(&items)

	return c.JSON(http.StatusOK, map[string]interface{}{"data": items, "total": total})
}

// GetUpstream handles GET /api/v1/radius-upstreams/:id.
//
// @Summary get proxy upstream detail
// @Tags RadiusProxy
// @Param id path int true "Upstream ID"
// @Success 200 {object} domain.RadiusUpstream
// @Router /api/v1/radius-upstreams/{id} [get]
func GetUpstream(c echo.Context) error {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return fail(c, http.StatusBadRequest, "INVALID_ID", "Invalid upstream ID", nil)
	}
	var item domain.RadiusUpstream
	if err := GetDB(c).First(&item, id).Error; err != nil {
		return fail(c, http.StatusNotFound, "NOT_FOUND", "Upstream not found", nil)
	}
	return ok(c, item)
}

// CreateUpstream handles POST /api/v1/radius-upstreams.
//
// @Summary create a proxy upstream server
// @Tags RadiusProxy
// @Param body body upstreamPayload true "Upstream information"
// @Success 201 {object} domain.RadiusUpstream
// @Router /api/v1/radius-upstreams [post]
func CreateUpstream(c echo.Context) error {
	var payload upstreamPayload
	if err := c.Bind(&payload); err != nil {
		return fail(c, http.StatusBadRequest, "INVALID_REQUEST", "Unable to parse request parameters", err.Error())
	}
	if err := c.Validate(&payload); err != nil {
		return handleValidationError(c, err)
	}

	item := domain.RadiusUpstream{
		NodeId:   payload.NodeId,
		Name:     payload.Name,
		Addr:     payload.Addr,
		AuthPort: intOrDefault(payload.AuthPort, 1812),
		AcctPort: intOrDefault(payload.AcctPort, 1813),
		CoaPort:  intOrDefault(payload.CoaPort, 3799),
		Secret:   payload.Secret,
		Role:     domain.UpstreamRoleProxy,
		Weight:   intOrDefault(payload.Weight, 1),
		Status:   "enabled",
		Remark:   payload.Remark,
	}
	if payload.Role != "" {
		item.Role = payload.Role
	}
	if payload.Status != "" {
		item.Status = payload.Status
	}

	if err := GetDB(c).Create(&item).Error; err != nil {
		return fail(c, http.StatusInternalServerError, "CREATE_FAILED", "Failed to create upstream", err.Error())
	}
	return ok(c, item)
}

// UpdateUpstream handles PUT /api/v1/radius-upstreams/:id.
//
// @Summary update a proxy upstream server
// @Tags RadiusProxy
// @Param id path int true "Upstream ID"
// @Param body body upstreamPayload true "Upstream information"
// @Success 200 {object} domain.RadiusUpstream
// @Router /api/v1/radius-upstreams/{id} [put]
func UpdateUpstream(c echo.Context) error {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return fail(c, http.StatusBadRequest, "INVALID_ID", "Invalid upstream ID", nil)
	}
	var item domain.RadiusUpstream
	if err := GetDB(c).First(&item, id).Error; err != nil {
		return fail(c, http.StatusNotFound, "NOT_FOUND", "Upstream not found", nil)
	}

	var payload upstreamUpdatePayload
	if err := c.Bind(&payload); err != nil {
		return fail(c, http.StatusBadRequest, "INVALID_REQUEST", "Unable to parse request parameters", err.Error())
	}
	if err := c.Validate(&payload); err != nil {
		return handleValidationError(c, err)
	}

	if payload.Name != "" {
		item.Name = payload.Name
	}
	if payload.Addr != "" {
		item.Addr = payload.Addr
	}
	if payload.AuthPort != nil {
		item.AuthPort = *payload.AuthPort
	}
	if payload.AcctPort != nil {
		item.AcctPort = *payload.AcctPort
	}
	if payload.CoaPort != nil {
		item.CoaPort = *payload.CoaPort
	}
	if payload.Secret != "" {
		item.Secret = payload.Secret
	}
	if payload.Role != "" {
		item.Role = payload.Role
	}
	if payload.Weight != nil {
		item.Weight = *payload.Weight
	}
	if payload.Status != "" {
		item.Status = payload.Status
	}
	if payload.Remark != "" {
		item.Remark = payload.Remark
	}
	if payload.NodeId > 0 {
		item.NodeId = payload.NodeId
	}

	if err := GetDB(c).Save(&item).Error; err != nil {
		return fail(c, http.StatusInternalServerError, "UPDATE_FAILED", "Failed to update upstream", err.Error())
	}
	return ok(c, item)
}

// DeleteUpstream handles DELETE /api/v1/radius-upstreams/:id. It refuses
// deletion while the upstream is referenced by any realm route.
//
// @Summary delete a proxy upstream server
// @Tags RadiusProxy
// @Param id path int true "Upstream ID"
// @Success 200 {object} SuccessResponse
// @Router /api/v1/radius-upstreams/{id} [delete]
func DeleteUpstream(c echo.Context) error {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return fail(c, http.StatusBadRequest, "INVALID_ID", "Invalid upstream ID", nil)
	}

	var routes int64
	GetDB(c).Model(&domain.RadiusRealmRoute{}).Where(
		"',' || upstream_ids || ',' LIKE ?", "%,"+strconv.FormatInt(id, 10)+",%").Count(&routes)
	if routes > 0 {
		return fail(c, http.StatusConflict, "IN_USE", "Upstream is referenced by realm routes", map[string]interface{}{
			"route_count": routes,
		})
	}

	if err := GetDB(c).Delete(&domain.RadiusUpstream{}, id).Error; err != nil {
		return fail(c, http.StatusInternalServerError, "DELETE_FAILED", "Failed to delete upstream", err.Error())
	}
	return ok(c, map[string]interface{}{"message": "Deletion successful"})
}

// ListRealmRoutes handles GET /api/v1/radius-realm-routes.
//
// @Summary list realm routes
// @Tags RadiusProxy
// @Success 200 {object} ListResponse
// @Router /api/v1/radius-realm-routes [get]
func ListRealmRoutes(c echo.Context) error {
	db := GetDB(c)
	page, _ := strconv.Atoi(c.QueryParam("page"))
	perPage, _ := strconv.Atoi(c.QueryParam("perPage"))
	if page < 1 {
		page = 1
	}
	if perPage < 1 || perPage > 100 {
		perPage = 20
	}

	var total int64
	var items []domain.RadiusRealmRoute
	query := db.Model(&domain.RadiusRealmRoute{})
	if status := strings.TrimSpace(c.QueryParam("status")); status != "" {
		query = query.Where("status = ?", status)
	}
	query.Count(&total)
	query.Order("id ASC").Limit(perPage).Offset((page - 1) * perPage).Find(&items)

	return c.JSON(http.StatusOK, map[string]interface{}{"data": items, "total": total})
}

// GetRealmRoute handles GET /api/v1/radius-realm-routes/:id.
//
// @Summary get realm route detail
// @Tags RadiusProxy
// @Param id path int true "Route ID"
// @Success 200 {object} domain.RadiusRealmRoute
// @Router /api/v1/radius-realm-routes/{id} [get]
func GetRealmRoute(c echo.Context) error {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return fail(c, http.StatusBadRequest, "INVALID_ID", "Invalid route ID", nil)
	}
	var item domain.RadiusRealmRoute
	if err := GetDB(c).First(&item, id).Error; err != nil {
		return fail(c, http.StatusNotFound, "NOT_FOUND", "Realm route not found", nil)
	}
	return ok(c, item)
}

// CreateRealmRoute handles POST /api/v1/radius-realm-routes.
//
// @Summary create a realm route
// @Tags RadiusProxy
// @Param body body realmRoutePayload true "Realm route information"
// @Success 201 {object} domain.RadiusRealmRoute
// @Router /api/v1/radius-realm-routes [post]
func CreateRealmRoute(c echo.Context) error {
	var payload realmRoutePayload
	if err := c.Bind(&payload); err != nil {
		return fail(c, http.StatusBadRequest, "INVALID_REQUEST", "Unable to parse request parameters", err.Error())
	}
	if err := c.Validate(&payload); err != nil {
		return handleValidationError(c, err)
	}

	// Reject unknown upstream ids so a typo does not silently break routing.
	ids := parseUpstreamIDCSV(payload.UpstreamIds)
	var found int64
	GetDB(c).Model(&domain.RadiusUpstream{}).Where("id IN ?", ids).Count(&found)
	if int(found) != len(ids) {
		return fail(c, http.StatusUnprocessableEntity, "UNKNOWN_UPSTREAM", "One or more upstream IDs do not exist", map[string]interface{}{
			"requested": ids, "found": found,
		})
	}

	item := domain.RadiusRealmRoute{
		NodeId:         payload.NodeId,
		Realm:          payload.Realm,
		Strategy:       domain.RealmStrategySingle,
		UpstreamIds:    payload.UpstreamIds,
		UserPrefix:     payload.UserPrefix,
		UserSuffix:     payload.UserSuffix,
		MappedPassword: payload.MappedPassword,
		AcctMode:       domain.AcctModeBoth,
		Status:         "enabled",
		Remark:         payload.Remark,
	}
	if payload.Strategy != "" {
		item.Strategy = payload.Strategy
	}
	if payload.AcctMode != "" {
		item.AcctMode = payload.AcctMode
	}
	if payload.Status != "" {
		item.Status = payload.Status
	}

	if err := GetDB(c).Create(&item).Error; err != nil {
		return fail(c, http.StatusInternalServerError, "CREATE_FAILED", "Failed to create realm route", err.Error())
	}
	return ok(c, item)
}

// UpdateRealmRoute handles PUT /api/v1/radius-realm-routes/:id.
//
// @Summary update a realm route
// @Tags RadiusProxy
// @Param id path int true "Route ID"
// @Param body body realmRoutePayload true "Realm route information"
// @Success 200 {object} domain.RadiusRealmRoute
// @Router /api/v1/radius-realm-routes/{id} [put]
func UpdateRealmRoute(c echo.Context) error {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return fail(c, http.StatusBadRequest, "INVALID_ID", "Invalid route ID", nil)
	}
	var item domain.RadiusRealmRoute
	if err := GetDB(c).First(&item, id).Error; err != nil {
		return fail(c, http.StatusNotFound, "NOT_FOUND", "Realm route not found", nil)
	}

	var payload realmRouteUpdatePayload
	if err := c.Bind(&payload); err != nil {
		return fail(c, http.StatusBadRequest, "INVALID_REQUEST", "Unable to parse request parameters", err.Error())
	}
	if err := c.Validate(&payload); err != nil {
		return handleValidationError(c, err)
	}

	if payload.Realm != "" {
		item.Realm = payload.Realm
	}
	if payload.Strategy != "" {
		item.Strategy = payload.Strategy
	}
	if payload.UpstreamIds != "" {
		ids := parseUpstreamIDCSV(payload.UpstreamIds)
		var found int64
		GetDB(c).Model(&domain.RadiusUpstream{}).Where("id IN ?", ids).Count(&found)
		if int(found) != len(ids) {
			return fail(c, http.StatusUnprocessableEntity, "UNKNOWN_UPSTREAM", "One or more upstream IDs do not exist", nil)
		}
		item.UpstreamIds = payload.UpstreamIds
	}
	if payload.UserPrefix != "" {
		item.UserPrefix = payload.UserPrefix
	}
	if payload.UserSuffix != "" {
		item.UserSuffix = payload.UserSuffix
	}
	if payload.MappedPassword != "" {
		item.MappedPassword = payload.MappedPassword
	}
	if payload.AcctMode != "" {
		item.AcctMode = payload.AcctMode
	}
	if payload.Status != "" {
		item.Status = payload.Status
	}
	if payload.Remark != "" {
		item.Remark = payload.Remark
	}
	if payload.NodeId > 0 {
		item.NodeId = payload.NodeId
	}

	if err := GetDB(c).Save(&item).Error; err != nil {
		return fail(c, http.StatusInternalServerError, "UPDATE_FAILED", "Failed to update realm route", err.Error())
	}
	return ok(c, item)
}

// DeleteRealmRoute handles DELETE /api/v1/radius-realm-routes/:id.
//
// @Summary delete a realm route
// @Tags RadiusProxy
// @Param id path int true "Route ID"
// @Success 200 {object} SuccessResponse
// @Router /api/v1/radius-realm-routes/{id} [delete]
func DeleteRealmRoute(c echo.Context) error {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return fail(c, http.StatusBadRequest, "INVALID_ID", "Invalid route ID", nil)
	}
	if err := GetDB(c).Delete(&domain.RadiusRealmRoute{}, id).Error; err != nil {
		return fail(c, http.StatusInternalServerError, "DELETE_FAILED", "Failed to delete realm route", err.Error())
	}
	return ok(c, map[string]interface{}{"message": "Deletion successful"})
}

// registerProxyRoutes registers the proxy subsystem management routes.
func registerProxyRoutes() {
	webserver.ApiGET("/radius-upstreams", ListUpstreams)
	webserver.ApiGET("/radius-upstreams/:id", GetUpstream)
	webserver.ApiPOST("/radius-upstreams", CreateUpstream, requireAdmin())
	webserver.ApiPUT("/radius-upstreams/:id", UpdateUpstream, requireAdmin())
	webserver.ApiDELETE("/radius-upstreams/:id", DeleteUpstream, requireAdmin())

	webserver.ApiGET("/radius-realm-routes", ListRealmRoutes)
	webserver.ApiGET("/radius-realm-routes/:id", GetRealmRoute)
	webserver.ApiPOST("/radius-realm-routes", CreateRealmRoute, requireAdmin())
	webserver.ApiPUT("/radius-realm-routes/:id", UpdateRealmRoute, requireAdmin())
	webserver.ApiDELETE("/radius-realm-routes/:id", DeleteRealmRoute, requireAdmin())
}

func intOrDefault(v *int, def int) int {
	if v == nil {
		return def
	}
	return *v
}

// parseUpstreamIDCSV parses a comma-separated upstream id list, tolerating
// spaces and dropping empty entries.
func parseUpstreamIDCSV(csv string) []int64 {
	parts := strings.Split(csv, ",")
	ids := make([]int64, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if id, err := strconv.ParseInt(p, 10, 64); err == nil && id > 0 {
			ids = append(ids, id)
		}
	}
	return ids
}
