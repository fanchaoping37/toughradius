package handlers

import (
	"fmt"

	"github.com/talkincode/toughradius/v9/internal/billing"
	"github.com/talkincode/toughradius/v9/internal/radiusd/plugins/accounting"
	"github.com/talkincode/toughradius/v9/internal/radiusd/repository"
	"go.uber.org/zap"
	"layeh.com/radius/rfc2866"
	"layeh.com/radius/rfc2869"
)

// NasStateHandler handles Accounting-On and Accounting-Off events by clearing
// online sessions for the NAS that restarted or went offline.
type NasStateHandler struct {
	sessionRepo repository.SessionRepository
}

// NewNasStateHandler constructs a NasStateHandler with the session repository
// used for NAS-scoped bulk cleanup.
func NewNasStateHandler(sessionRepo repository.SessionRepository) *NasStateHandler {
	return &NasStateHandler{
		sessionRepo: sessionRepo,
	}
}

// Name returns the stable plugin name used by the accounting dispatcher.
func (h *NasStateHandler) Name() string {
	return "NasStateHandler"
}

// CanHandle reports whether the context represents Accounting-On or
// Accounting-Off.
func (h *NasStateHandler) CanHandle(ctx *accounting.AccountingContext) bool {
	return ctx.StatusType == int(rfc2866.AcctStatusType_Value_AccountingOn) ||
		ctx.StatusType == int(rfc2866.AcctStatusType_Value_AccountingOff)
}

// Handle removes all online sessions bound to the NAS in the event context.
//
// Handle returns an error when repository access is unavailable or when NAS
// identity data is missing, because both are required to scope the cleanup.
func (h *NasStateHandler) Handle(ctx *accounting.AccountingContext) error {
	if h.sessionRepo == nil {
		return fmt.Errorf("session repository is not available")
	}
	if ctx.NAS == nil {
		return fmt.Errorf("nas information is missing")
	}

	// RFC 2869 Event-Timestamp on Accounting-On is the authoritative reboot
	// time. Reconcile sessions into CDRs before any bulk delete so a NAS
	// restart cannot silently discard usage. When the attribute is absent we
	// keep the legacy cleanup path and never invent a reboot time.
	if ctx.StatusType == int(rfc2866.AcctStatusType_Value_AccountingOn) &&
		ctx.DB != nil && ctx.Request != nil && ctx.Request.Packet != nil {
		if eventTime, err := rfc2869.EventTimestamp_Lookup(ctx.Request.Packet); err == nil && !eventTime.IsZero() {
			result, reconcileErr := billing.NewEngine(ctx.DB).HandleNASReboot(ctx.Context, ctx.NASIP, eventTime.UTC())
			if reconcileErr != nil {
				zap.L().Error("reconcile sessions on NAS reboot failed",
					zap.String("namespace", "radius"),
					zap.String("nas_ip", ctx.NASIP),
					zap.Error(reconcileErr),
				)
			} else {
				zap.L().Info("reconciled sessions on NAS reboot",
					zap.String("namespace", "radius"),
					zap.String("nas_ip", ctx.NASIP),
					zap.Int("scanned", result.Scanned),
					zap.Int("closed", result.ClosedMissingStop),
					zap.Strings("errors", result.Errors),
				)
				return nil
			}
		}
	}

	if err := h.sessionRepo.BatchDeleteByNas(ctx.Context, ctx.NASIP, ctx.NAS.Identifier); err != nil {
		zap.L().Error("failed to clear sessions on NAS state change",
			zap.String("namespace", "radius"),
			zap.String("nas_ip", ctx.NASIP),
			zap.String("nas_id", ctx.NAS.Identifier),
			zap.Error(err),
		)
		return err
	}

	zap.L().Info("cleared sessions due to NAS state change",
		zap.String("namespace", "radius"),
		zap.String("nas_ip", ctx.NASIP),
		zap.String("nas_id", ctx.NAS.Identifier),
		zap.Int("status_type", ctx.StatusType),
	)
	return nil
}
