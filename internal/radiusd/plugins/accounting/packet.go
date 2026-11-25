package accounting

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/talkincode/toughradius/v9/internal/domain"
	"github.com/talkincode/toughradius/v9/pkg/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"layeh.com/radius"
	"layeh.com/radius/rfc2865"
	"layeh.com/radius/rfc2866"
	"layeh.com/radius/rfc2869"
)

// acctEventTime returns the NAS-reported event time (RFC 2869 §5.3,
// Event-Timestamp type 55) or the current time when the attribute is absent.
func EventTime(p *radius.Packet) time.Time {
	if ts, err := rfc2869.EventTimestamp_Lookup(p); err == nil && !ts.IsZero() {
		return ts.UTC()
	}
	return time.Now().UTC()
}

// acctStartTime derives the session start time from the NAS event time:
//
//	start = Event-Timestamp - Acct-Session-Time - Acct-Delay-Time
//
// This matches the FreeRADIUS v3 preacct convention
// (`%{expr: %l - Acct-Session-Time - Acct-Delay-Time}`) while preferring the
// authoritative NAS event time over the server clock. When Event-Timestamp is
// absent, the legacy server-clock derivation is retained.
func StartTime(p *radius.Packet) time.Time {
	sessionTime := int64(rfc2866.AcctSessionTime_Get(p))
	delay := int64(rfc2866.AcctDelayTime_Get(p))
	if ts, err := rfc2869.EventTimestamp_Lookup(p); err == nil && !ts.IsZero() {
		return ts.UTC().Add(-time.Duration(sessionTime+delay) * time.Second)
	}
	return time.Now().UTC().Add(-time.Duration(sessionTime) * time.Second)
}

type attrSnapshot struct {
	Type  int    `json:"type"`
	Value string `json:"value"`
}

// sanitizedAttributes serializes request attributes for the raw packet ledger.
// Passwords (types 2 and 3) are masked; EAP-Message payloads are truncated to
// the first 8 bytes because they can carry inner authentication credentials.
func SanitizedAttributes(p *radius.Packet) string {
	attrs := make([]attrSnapshot, 0, len(p.Attributes))
	for _, avp := range p.Attributes {
		if avp == nil {
			continue
		}
		value := hex.EncodeToString(avp.Attribute)
		switch int(avp.Type) {
		case 2, 3: // User-Password, CHAP-Password
			value = "***MASKED***"
		case 79: // EAP-Message
			if len(avp.Attribute) > 8 {
				value = hex.EncodeToString(avp.Attribute[:8]) + "..."
			}
		}
		attrs = append(attrs, attrSnapshot{Type: int(avp.Type), Value: value})
	}
	encoded, err := json.Marshal(attrs)
	if err != nil {
		return "[]"
	}
	return string(encoded)
}

// acctGigawordOctets merges RFC 2869 Acct-Input/Output-Gigawords (types
// 52/53) with the 32-bit Octets counters. Gigawords count 2^32 wraparounds.
func GigawordOctets(octets uint32, gigawords uint32) int64 {
	return int64(octets) + int64(gigawords)*int64(1<<32)
}

// acctDedupKey implements RFC 5080 §2.2.2 server-side duplicate detection:
// (source IP, source UDP port, Identifier) plus session/event identity.
func DedupKey(r *radius.Request, nasAddr string) string {
	source := ""
	if r.RemoteAddr != nil {
		source = r.RemoteAddr.String()
	}
	if host, port, err := net.SplitHostPort(source); err == nil {
		source = host + ":" + port
	}
	sessionID := rfc2866.AcctSessionID_GetString(r.Packet)
	var eventUnix int64
	if ts, err := rfc2869.EventTimestamp_Lookup(r.Packet); err == nil && !ts.IsZero() {
		eventUnix = ts.Unix()
	}
	input := fmt.Sprintf("%s|%s|%d|%d|%d|%s|%d",
		source, nasAddr, r.Identifier, int(r.Code),
		int(rfc2866.AcctStatusType_Get(r.Packet)), sessionID, eventUnix)
	sum := sha256.Sum256([]byte(input))
	return hex.EncodeToString(sum[:])
}

// recordAccountingPacket stores a sanitized raw accounting packet and reserves
// its RFC 5080 dedup key atomically. It returns false for a retransmitted
// packet (the caller should answer with the cached Accounting-Response and
// skip session mutation).
func RecordPacket(ctx context.Context, db *gorm.DB, r *radius.Request, nasAddr, username string) (bool, error) {
	if db == nil {
		return true, nil // no persistence configured: treat as fresh packet
	}
	key := DedupKey(r, nasAddr)
	record := BuildRawRecord(r, nasAddr, username, key)

	res := db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "dedup_key"}},
		DoNothing: true,
	}).Create(&record)
	if res.Error != nil {
		return true, res.Error
	}
	if res.RowsAffected == 0 {
		return false, nil
	}

	// Response cache reservation, pruned lazily to keep the write path cheap.
	if err := db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "dedup_key"}},
		DoNothing: true,
	}).Create(&domain.RadiusAcctDedup{DedupKey: key, CreatedAt: time.Now().UTC()}).Error; err != nil {
		return true, err
	}
	if time.Now().UTC().Unix()%16 == 0 {
		_ = db.WithContext(ctx).Where("created_at < ?", time.Now().UTC().Add(-30*time.Second)).
			Delete(&domain.RadiusAcctDedup{}).Error
	}
	return true, nil
}

// acctRemoteHost extracts the NAS source IP from a UDP remote address.
func acctRemoteHost(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	if idx := strings.LastIndex(addr, ":"); idx >= 0 {
		return addr[:idx]
	}
	return addr
}

// buildRawAcctRecord projects an accounting request into the append-only
// raw-record model shared by the recorder and its unit tests.
func BuildRawRecord(r *radius.Request, nasAddr, username, dedupKey string) domain.RadiusAcctRecord {
	p := r.Packet
	return domain.RadiusAcctRecord{
		ID:              common.UUIDint64(),
		Username:        username,
		NasAddr:         nasAddr,
		AcctSessionId:   rfc2866.AcctSessionID_GetString(p),
		StatusType:      int(rfc2866.AcctStatusType_Get(p)),
		Identifier:      int(r.Identifier),
		EventTimestamp:  EventTime(p),
		AcctDelayTime:   int(rfc2866.AcctDelayTime_Get(p)),
		AcctSessionTime: int(rfc2866.AcctSessionTime_Get(p)),
		InputOctets:     GigawordOctets(uint32(rfc2866.AcctInputOctets_Get(p)), uint32(rfc2869.AcctInputGigawords_Get(p))),
		OutputOctets:    GigawordOctets(uint32(rfc2866.AcctOutputOctets_Get(p)), uint32(rfc2869.AcctOutputGigawords_Get(p))),
		InputPackets:    int(rfc2866.AcctInputPackets_Get(p)),
		OutputPackets:   int(rfc2866.AcctOutputPackets_Get(p)),
		RawAttrs:        SanitizedAttributes(p),
		DedupKey:        dedupKey,
		CreatedAt:       time.Now().UTC(),
	}
}

// acctIdentityUsername keeps the compiler honest about the RFC 2865 import
// used by raw-record tests that build User-Name attributes.
var _ = rfc2865.UserName_Type
