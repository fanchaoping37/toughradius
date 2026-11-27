package radiusd

import (
	"context"
	"strconv"
	"strings"

	"github.com/talkincode/toughradius/v9/internal/analytics"
	"github.com/talkincode/toughradius/v9/internal/app"
	"github.com/talkincode/toughradius/v9/internal/domain"
	radiuserrors "github.com/talkincode/toughradius/v9/internal/radiusd/errors"
	acctpkg "github.com/talkincode/toughradius/v9/internal/radiusd/plugins/accounting"
	vendorparserspkg "github.com/talkincode/toughradius/v9/internal/radiusd/plugins/vendorparsers"
	"go.uber.org/zap"
	"layeh.com/radius"
	"layeh.com/radius/rfc2865"
	"layeh.com/radius/rfc2866"
	"layeh.com/radius/rfc2869"
)

// AcctService handles RADIUS Accounting-Request packets.
type AcctService struct {
	*RadiusService
}

func NewAcctService(radiusService *RadiusService) *AcctService {
	return &AcctService{RadiusService: radiusService}
}

func (s *AcctService) ServeRADIUS(w radius.ResponseWriter, r *radius.Request) {
	// Recover from unexpected panics only (programming errors)
	defer func() {
		if ret := recover(); ret != nil {
			var err error
			switch v := ret.(type) {
			case error:
				err = v
			case string:
				err = radiuserrors.NewError(v)
			default:
				err = radiuserrors.NewError("unknown panic")
			}
			app.IncRadiusMetric(app.MetricsRadiusAcctDrop)
			zap.L().Error("radius accounting unexpected panic",
				zap.Error(err),
				zap.String("namespace", "radius"),
				zap.String("metrics", app.MetricsRadiusAcctDrop),
				zap.Stack("stacktrace"),
			)
		}
	}()

	if r == nil {
		return
	}

	if s.Config().Radiusd.Debug {
		zap.S().Debug(FmtRequest(r))
	}

	// NAS Access check
	raddrstr := r.RemoteAddr.String()
	nasrip := raddrstr[:strings.Index(raddrstr, ":")]
	var identifier = rfc2865.NASIdentifier_GetString(r.Packet)

	nas, err := s.GetNas(nasrip, identifier)
	if err != nil {
		s.logAcctError("nas_lookup", nasrip, "", err)
		return
	}

	// Reset packet secret
	r.Secret = []byte(nas.Secret)
	r.Secret = []byte(nas.Secret) //nolint:staticcheck

	statusType := rfc2866.AcctStatusType_Get(r.Packet)

	// UsernameCheck
	var username string
	if statusType != rfc2866.AcctStatusType_Value_AccountingOn &&
		statusType != rfc2866.AcctStatusType_Value_AccountingOff {
		username = rfc2865.UserName_GetString(r.Packet)
		if username == "" {
			s.logAcctError("validate_username", nasrip, "", radiuserrors.NewAcctUsernameEmptyError())
			return
		}
	}

	defer s.ReleaseAuthRateLimit(username)

	// Validate the Accounting-Request authenticator against the NAS shared secret.
	// Unlike Access-Request (which carries a random authenticator), accounting
	// packets are signed with a keyed Request Authenticator (RFC 2866). A mismatch
	// means the request is forged or the NAS is misconfigured, so it must be
	// dropped before any session state is mutated.
	if err := s.CheckRequestSecret(r.Packet, []byte(nas.Secret)); err != nil {
		s.logAcctError("verify_secret", nasrip, username, err)
		return
	}

	// RFC 5080 §2.2.2: cache responses by (source IP, source port,
	// Identifier). A retransmitted accounting packet is answered with the
	// same Accounting-Response but must not mutate session or billing state.
	fresh, dedupErr := acctpkg.RecordPacket(r.Context(), s.appCtx.DB(), r, nasrip, username)
	if dedupErr != nil {
		zap.L().Warn("accounting dedup store unavailable",
			zap.String("namespace", "radius"),
			zap.String("username", username),
			zap.Error(dedupErr),
		)
	}
	if !fresh {
		app.IncRadiusMetric(app.MetricsRadiusAcctDrop)
		zap.L().Debug("duplicate accounting packet ignored",
			zap.String("namespace", "radius"),
			zap.String("username", username),
			zap.String("session_id", rfc2866.AcctSessionID_GetString(r.Packet)),
		)
	}

	vendorReq := s.ParseVendor(r, nas.VendorCode)

	responseSent := s.SendResponse(w, r)

	if responseSent {
		app.IncRadiusMetric(app.MetricsRadiusAccounting)
		zap.S().Info("radius accounting",
			zap.String("namespace", "radius"),
			zap.String("metrics", app.MetricsRadiusAccounting),
		)
	}

	if !fresh {
		return
	}

	// Proxy forwarding + replication side-channels (async upstream I/O).
	s.dispatchAcctProxy(r, username)

	// async process accounting with back-pressure aware submit
	task := func() {
		vendorReqForPlugin := &vendorparserspkg.VendorRequest{
			MacAddr: vendorReq.MacAddr,
			Vlanid1: vendorReq.Vlanid1,
			Vlanid2: vendorReq.Vlanid2,
		}

		ctx := context.Background()
		err := s.HandleAccountingWithPlugins(ctx, r, vendorReqForPlugin, username, nas, nasrip)
		if err != nil {
			zap.L().Error("accounting plugin processing error",
				zap.String("namespace", "radius"),
				zap.String("username", username),
				zap.Int("status_type", int(statusType)),
				zap.Error(err),
			)
			return
		}
		// The raw packet is reserved with consumed=false at ingress. Only a
		// successfully handled packet may be marked consumed; anything left
		// behind is a genuine dead letter for the reconciliation worker.
		key := acctpkg.DedupKey(r, nasrip)
		if err := s.appCtx.DB().WithContext(ctx).Model(&domain.RadiusAcctRecord{}).
			Where("dedup_key = ?", key).Update("consumed", true).Error; err != nil {
			zap.L().Warn("mark accounting raw record consumed failed",
				zap.String("namespace", "radius"),
				zap.String("username", username),
				zap.String("dedup_key", key),
				zap.Error(err),
			)
		}
		if s.Archive != nil && s.Archive.Enabled() {
			nasPort := rfc2869.NASPortID_GetString(r.Packet)
			if nasPort == "" {
				nasPort = strconv.Itoa(int(rfc2865.NASPort_Get(r.Packet)))
			}
			archived := analytics.Record{
				EventTime:       acctpkg.EventTime(r.Packet),
				Username:        username,
				NASIP:           nasrip,
				NASPort:         nasPort,
				SessionID:       rfc2866.AcctSessionID_GetString(r.Packet),
				StatusType:      uint8(statusType),
				SessionTime:     uint32(rfc2866.AcctSessionTime_Get(r.Packet)),
				InputOctets:     uint64(acctpkg.GigawordOctets(uint32(rfc2866.AcctInputOctets_Get(r.Packet)), uint32(rfc2869.AcctInputGigawords_Get(r.Packet)))),
				OutputOctets:    uint64(acctpkg.GigawordOctets(uint32(rfc2866.AcctOutputOctets_Get(r.Packet)), uint32(rfc2869.AcctOutputGigawords_Get(r.Packet)))),
				InputGigawords:  uint32(rfc2869.AcctInputGigawords_Get(r.Packet)),
				OutputGigawords: uint32(rfc2869.AcctOutputGigawords_Get(r.Packet)),
				DedupKey:        key,
				FramedIP:        rfc2865.FramedIPAddress_Get(r.Packet).String(),
			}
			if err := s.Archive.Write(ctx, archived); err != nil {
				zap.L().Warn("archive accounting record failed",
					zap.String("namespace", "analytics"),
					zap.String("dedup_key", key),
					zap.Error(err),
				)
			}
		}
	}

	s.submitAcctTask(task, username)
}

// submitAcctTask schedules an accounting task on the bounded worker pool.
//
// When the pool is saturated the task is dropped (and recorded as an accounting
// drop) rather than spawning a goroutine per request. Spawning an unbounded
// goroutine on overload would let a flood of accounting traffic exhaust memory;
// dropping preserves back-pressure. Returns true if the task was accepted.
func (s *AcctService) submitAcctTask(task func(), username string) bool {
	if err := s.TaskPool.Submit(task); err != nil {
		app.IncRadiusMetric(app.MetricsRadiusAcctDrop)
		zap.L().Warn("accounting task pool saturated, dropping accounting update",
			zap.String("namespace", "radius"),
			zap.String("username", username),
			zap.String("metrics", app.MetricsRadiusAcctDrop),
			zap.Error(err),
		)
		return false
	}
	return true
}

// acctMetricsKeyForStage maps an accounting ingress stage to its failure metric.
//
// Accounting-Requests dropped before processing are classified by the stage that
// rejected them so operators can distinguish an unknown/unauthorized NAS from a
// missing username or a failed authenticator check, instead of seeing a single
// opaque radus_acct_drop counter. Unknown stages fall back to that catch-all.
func acctMetricsKeyForStage(stage string) string {
	switch stage {
	case "nas_lookup":
		return app.MetricsRadiusAcctDropNas
	case "validate_username":
		return app.MetricsRadiusAcctDropUsername
	case "verify_secret":
		return app.MetricsRadiusAcctDropSecret
	default:
		return app.MetricsRadiusAcctDrop
	}
}

// logAcctError records a classified accounting-failure metric and logs the error.
//
// The metric is chosen from the ingress stage rather than the error's own key:
// the NAS lookup reuses the shared GetNas helper, whose not-found error is an
// AuthError carrying the auth-side radus_reject_unauthorized key, so keying off
// the error would mis-attribute dropped accounting packets to the auth reject
// counter. Stage-based classification keeps accounting and auth metrics disjoint.
func (s *AcctService) logAcctError(stage, nasip, username string, err error) {
	metricsKey := acctMetricsKeyForStage(stage)
	app.IncRadiusMetric(metricsKey)

	fields := []zap.Field{
		zap.Error(err),
		zap.String("namespace", "radius"),
		zap.String("metrics", metricsKey),
		zap.String("stage", stage),
	}
	if nasip != "" {
		fields = append(fields, zap.String("nasip", nasip))
	}
	if username != "" {
		fields = append(fields, zap.String("username", username))
	}

	zap.L().Error("radius accounting error", fields...)
}

func (s *AcctService) SendResponse(w radius.ResponseWriter, r *radius.Request) bool {
	resp := r.Response(radius.CodeAccountingResponse)
	if err := w.Write(resp); err != nil {
		app.IncRadiusMetric(app.MetricsRadiusAcctDrop)
		zap.L().Error("radius accounting response error",
			zap.Error(err),
			zap.String("namespace", "radius"),
			zap.String("metrics", app.MetricsRadiusAcctDrop),
		)
		return false
	}

	if s.Config().Radiusd.Debug {
		zap.S().Debug(FmtResponse(resp, r.RemoteAddr))
	}
	return true
}
