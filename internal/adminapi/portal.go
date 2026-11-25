package adminapi

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"
	"github.com/talkincode/toughradius/v9/internal/domain"
	"github.com/talkincode/toughradius/v9/internal/portal"
	"github.com/talkincode/toughradius/v9/internal/webserver"
)

// Portal subsystem management API and the HTTP bridge for external login
// pages. The login page itself lives outside ToughRADIUS; these endpoints
// drive the Portal V2 UDP exchange with registered NAS devices.

type portalNasPayload struct {
	NodeId int64  `json:"node_id,string" validate:"gte=0"`
	Name   string `json:"name" validate:"required,min=1,max=100"`
	Addr   string `json:"addr" validate:"required,min=1,max=100"`
	Port   *int   `json:"port" validate:"omitempty,port"`
	Secret string `json:"secret" validate:"required,min=6,max=100"`
	Status string `json:"status" validate:"omitempty,oneof=enabled disabled"`
	Remark string `json:"remark" validate:"omitempty,max=500"`
}

type portalNasUpdatePayload struct {
	NodeId int64  `json:"node_id,string" validate:"omitempty,gte=0"`
	Name   string `json:"name" validate:"omitempty,min=1,max=100"`
	Addr   string `json:"addr" validate:"omitempty,min=1,max=100"`
	Port   *int   `json:"port" validate:"omitempty,port"`
	Secret string `json:"secret" validate:"omitempty,min=6,max=100"`
	Status string `json:"status" validate:"omitempty,oneof=enabled disabled"`
	Remark string `json:"remark" validate:"omitempty,max=500"`
}

func portalServiceUnavailable(c echo.Context) error {
	return fail(c, http.StatusServiceUnavailable, "PORTAL_DISABLED",
		"Portal protocol server is not enabled", nil)
}

// ListPortalNASES handles GET /api/v1/portal-nases.
func ListPortalNASES(c echo.Context) error {
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
	var items []domain.PortalNas
	query := db.Model(&domain.PortalNas{})
	if status := strings.TrimSpace(c.QueryParam("status")); status != "" {
		query = query.Where("status = ?", status)
	}
	query.Count(&total)
	query.Order("id DESC").Limit(perPage).Offset((page - 1) * perPage).Find(&items)
	return c.JSON(http.StatusOK, map[string]interface{}{"data": items, "total": total})
}

// GetPortalNAS handles GET /api/v1/portal-nases/:id.
func GetPortalNAS(c echo.Context) error {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return fail(c, http.StatusBadRequest, "INVALID_ID", "Invalid Portal NAS ID", nil)
	}
	var item domain.PortalNas
	if err := GetDB(c).First(&item, id).Error; err != nil {
		return fail(c, http.StatusNotFound, "NOT_FOUND", "Portal NAS not found", nil)
	}
	return ok(c, item)
}

// CreatePortalNAS handles POST /api/v1/portal-nases.
func CreatePortalNAS(c echo.Context) error {
	var payload portalNasPayload
	if err := c.Bind(&payload); err != nil {
		return fail(c, http.StatusBadRequest, "INVALID_REQUEST", "Unable to parse request parameters", err.Error())
	}
	if err := c.Validate(&payload); err != nil {
		return handleValidationError(c, err)
	}
	status := payload.Status
	if status == "" {
		status = "enabled"
	}
	item := domain.PortalNas{
		NodeId: payload.NodeId, Name: payload.Name, Addr: payload.Addr,
		Port: intOrDefault(payload.Port, 2000), Secret: payload.Secret,
		Status: status, Remark: payload.Remark,
	}
	if err := GetDB(c).Create(&item).Error; err != nil {
		return fail(c, http.StatusInternalServerError, "CREATE_FAILED", "Failed to create Portal NAS", err.Error())
	}
	if item.Status == "enabled" {
		if svc := portal.GetActiveService(); svc != nil {
			_ = svc.Server.RegisterNAS(&portal.NAS{Addr: item.Addr, Secret: item.Secret})
		}
	}
	return ok(c, item)
}

// UpdatePortalNAS handles PUT /api/v1/portal-nases/:id.
func UpdatePortalNAS(c echo.Context) error {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return fail(c, http.StatusBadRequest, "INVALID_ID", "Invalid Portal NAS ID", nil)
	}
	var item domain.PortalNas
	if err := GetDB(c).First(&item, id).Error; err != nil {
		return fail(c, http.StatusNotFound, "NOT_FOUND", "Portal NAS not found", nil)
	}
	var payload portalNasUpdatePayload
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
	if payload.Port != nil {
		item.Port = *payload.Port
	}
	if payload.Secret != "" {
		item.Secret = payload.Secret
	}
	if payload.Status != "" {
		item.Status = payload.Status
	}
	if payload.Remark != "" {
		item.Remark = payload.Remark
	}
	if err := GetDB(c).Save(&item).Error; err != nil {
		return fail(c, http.StatusInternalServerError, "UPDATE_FAILED", "Failed to update Portal NAS", err.Error())
	}
	if svc := portal.GetActiveService(); svc != nil {
		if item.Status == "enabled" {
			_ = svc.Server.RegisterNAS(&portal.NAS{Addr: item.Addr, Secret: item.Secret})
		} else {
			svc.Server.UnregisterNAS(item.Addr)
		}
	}
	return ok(c, item)
}

// DeletePortalNAS handles DELETE /api/v1/portal-nases/:id.
func DeletePortalNAS(c echo.Context) error {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return fail(c, http.StatusBadRequest, "INVALID_ID", "Invalid Portal NAS ID", nil)
	}
	var item domain.PortalNas
	if err := GetDB(c).First(&item, id).Error; err != nil {
		return fail(c, http.StatusNotFound, "NOT_FOUND", "Portal NAS not found", nil)
	}
	if err := GetDB(c).Delete(&item).Error; err != nil {
		return fail(c, http.StatusInternalServerError, "DELETE_FAILED", "Failed to delete Portal NAS", err.Error())
	}
	if svc := portal.GetActiveService(); svc != nil {
		svc.Server.UnregisterNAS(item.Addr)
	}
	return ok(c, map[string]interface{}{"message": "Deletion successful"})
}

// PortalChallenge handles POST /api/v1/portal/challenge.
func PortalChallenge(c echo.Context) error {
	svc := portal.GetActiveService()
	if svc == nil {
		return portalServiceUnavailable(c)
	}
	var req portal.ChallengeRequest
	if err := c.Bind(&req); err != nil {
		return fail(c, http.StatusBadRequest, "INVALID_REQUEST", "Unable to parse request parameters", err.Error())
	}
	result, err := svc.Challenge(req)
	if err != nil {
		return fail(c, http.StatusBadRequest, "PORTAL_CHALLENGE_FAILED", "Portal challenge failed", err.Error())
	}
	return ok(c, result)
}

// PortalAuthenticate handles POST /api/v1/portal/auth.
func PortalAuthenticate(c echo.Context) error {
	svc := portal.GetActiveService()
	if svc == nil {
		return portalServiceUnavailable(c)
	}
	var req portal.AuthRequest
	if err := c.Bind(&req); err != nil {
		return fail(c, http.StatusBadRequest, "INVALID_REQUEST", "Unable to parse request parameters", err.Error())
	}
	result, err := svc.Authenticate(req)
	if err != nil {
		return fail(c, http.StatusBadRequest, "PORTAL_AUTH_FAILED", "Portal authentication failed", err.Error())
	}
	return ok(c, result)
}

// PortalLogout handles POST /api/v1/portal/logout.
func PortalLogout(c echo.Context) error {
	svc := portal.GetActiveService()
	if svc == nil {
		return portalServiceUnavailable(c)
	}
	var req portal.LogoutRequest
	if err := c.Bind(&req); err != nil {
		return fail(c, http.StatusBadRequest, "INVALID_REQUEST", "Unable to parse request parameters", err.Error())
	}
	if err := svc.Logout(req); err != nil {
		return fail(c, http.StatusBadRequest, "PORTAL_LOGOUT_FAILED", "Portal logout failed", err.Error())
	}
	return ok(c, map[string]interface{}{"message": "Logout successful"})
}

// PortalSession handles GET /api/v1/portal/session?user_ip=...
func PortalSession(c echo.Context) error {
	svc := portal.GetActiveService()
	if svc == nil {
		return portalServiceUnavailable(c)
	}
	session, found := svc.SessionStatus(c.QueryParam("user_ip"))
	if !found {
		return fail(c, http.StatusNotFound, "NOT_FOUND", "No active Portal session", nil)
	}
	return ok(c, session)
}

// registerPortalRoutes registers the Portal management and HTTP bridge routes.
func registerPortalRoutes() {
	webserver.ApiGET("/portal-nases", ListPortalNASES)
	webserver.ApiGET("/portal-nases/:id", GetPortalNAS)
	webserver.ApiPOST("/portal-nases", CreatePortalNAS, requireAdmin())
	webserver.ApiPUT("/portal-nases/:id", UpdatePortalNAS, requireAdmin())
	webserver.ApiDELETE("/portal-nases/:id", DeletePortalNAS, requireAdmin())

	webserver.ApiPOST("/portal/challenge", PortalChallenge)
	webserver.ApiPOST("/portal/auth", PortalAuthenticate)
	webserver.ApiPOST("/portal/logout", PortalLogout)
	webserver.ApiGET("/portal/session", PortalSession)
}
