package accounting

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sqlite "github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/talkincode/toughradius/v9/internal/domain"
	"gorm.io/gorm"
	"layeh.com/radius"
	"layeh.com/radius/rfc2865"
	"layeh.com/radius/rfc2866"
	"layeh.com/radius/rfc2869"
)

func acctTestPacket(t *testing.T, identifier byte) *radius.Request {
	t.Helper()
	p := radius.New(radius.CodeAccountingRequest, []byte("secret"))
	require.NoError(t, rfc2865.UserName_SetString(p, "alice"))
	require.NoError(t, rfc2866.AcctSessionID_SetString(p, "sess-1"))
	require.NoError(t, rfc2866.AcctStatusType_Set(p, rfc2866.AcctStatusType_Value_Start))
	require.NoError(t, rfc2866.AcctSessionTime_Set(p, 120))
	require.NoError(t, rfc2866.AcctDelayTime_Set(p, 5))
	require.NoError(t, rfc2869.EventTimestamp_Set(p, time.Unix(1_700_000_000, 0).UTC()))
	require.NoError(t, rfc2866.AcctInputOctets_Set(p, 7))
	require.NoError(t, rfc2869.AcctInputGigawords_Set(p, 2))
	require.NoError(t, rfc2865.UserPassword_SetString(p, "topsecret"))
	req := &radius.Request{Packet: p, RemoteAddr: testAddr("192.0.2.10:4000")}
	req.Identifier = identifier
	return req
}

type testAddr string

func (a testAddr) Network() string { return "udp" }
func (a testAddr) String() string  { return string(a) }

func TestEventTimeAndStartTimeUseRFCAttributes(t *testing.T) {
	req := acctTestPacket(t, 7)
	event := time.Unix(1_700_000_000, 0).UTC()
	assert.True(t, EventTime(req.Packet).Equal(event))
	assert.True(t, StartTime(req.Packet).Equal(event.Add(-125*time.Second)),
		"start = Event-Timestamp - Session-Time - Delay-Time (RFC 2866 + FreeRADIUS preacct)")
}

func TestGigawordMergeHandlesWrap(t *testing.T) {
	assert.Equal(t, int64(1<<32+7), GigawordOctets(7, 1))
	assert.Equal(t, int64(2<<32+7), GigawordOctets(7, 2))
}

func TestSanitizedAttributesMasksSecrets(t *testing.T) {
	req := acctTestPacket(t, 1)
	raw := SanitizedAttributes(req.Packet)
	assert.Contains(t, raw, `"type":2`)
	assert.Contains(t, raw, "***MASKED***")
	assert.NotContains(t, raw, "746f70736563726574") // hex("topsecret")
}

func TestDedupKeyChangesWithIdentifier(t *testing.T) {
	a := DedupKey(acctTestPacket(t, 1), "192.0.2.1")
	b := DedupKey(acctTestPacket(t, 1), "192.0.2.1")
	c := DedupKey(acctTestPacket(t, 2), "192.0.2.1")
	assert.Equal(t, a, b)
	assert.NotEqual(t, a, c)
}

func TestRecordPacketIdempotent(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "acct.db")), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	defer sqlDB.Close()
	require.NoError(t, db.AutoMigrate(&domain.RadiusAcctRecord{}, &domain.RadiusAcctDedup{}))

	req := acctTestPacket(t, 11)
	first, err := RecordPacket(context.Background(), db, req, "192.0.2.1", "alice")
	require.NoError(t, err)
	assert.True(t, first)

	second, err := RecordPacket(context.Background(), db, acctTestPacket(t, 11), "192.0.2.1", "alice")
	require.NoError(t, err)
	assert.False(t, second, "retransmitted packet must be deduplicated per RFC 5080")

	var count int64
	require.NoError(t, db.Model(&domain.RadiusAcctRecord{}).Count(&count).Error)
	assert.Equal(t, int64(1), count)

	var record domain.RadiusAcctRecord
	require.NoError(t, db.First(&record).Error)
	assert.True(t, strings.Contains(record.RawAttrs, "***MASKED***"))
	assert.Equal(t, int64(2<<32+7), record.InputOctets)
	assert.Equal(t, int(rfc2866.AcctStatusType_Value_Start), record.StatusType)
}
