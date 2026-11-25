package radiusd

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	sqlite "github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/talkincode/toughradius/v9/internal/domain"
	"gorm.io/gorm"
	"layeh.com/radius"
	"layeh.com/radius/rfc2865"
	"layeh.com/radius/rfc3576"
)

func coaTaskTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "coa.db")), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, db.AutoMigrate(&domain.RadiusCoATask{}))
	return db
}

func coaTestTask() *domain.RadiusCoATask {
	id := SessionIdentity{Username: "alice", AcctSessionID: "sess-1", NasIP: "192.0.2.1", NasIdentifier: "nas-1"}
	raw, _ := json.Marshal(id)
	return &domain.RadiusCoATask{
		AcctSessionID: "sess-1", Username: "alice", NasAddr: "192.0.2.1",
		Action: "disconnect", IdentityJSON: string(raw), Status: "pending",
	}
}

func TestCoATaskLifecycleWritesIdentifierAndACK(t *testing.T) {
	db := coaTaskTestDB(t)
	mgr := NewCoATaskManager(db)
	task := coaTestTask()
	require.NoError(t, mgr.Create(context.Background(), task))
	assert.NotZero(t, task.ID)
	assert.Equal(t, "pending", task.Status)

	client := NewCoAService(nil)
	client.exchange = func(ctx context.Context, packet *radius.Packet, addr string) (*radius.Packet, error) {
		assert.Equal(t, byte(23), packet.Identifier, "task retry must pin the RFC 5176 Identifier")
		return radius.New(radius.CodeDisconnectACK, []byte("secret")), nil
	}
	task.Identifier = 23
	result, err := mgr.Execute(context.Background(), task, client)
	require.NoError(t, err)
	assert.True(t, result.Success)

	var stored domain.RadiusCoATask
	require.NoError(t, db.First(&stored, task.ID).Error)
	assert.Equal(t, "ack", stored.Status)
	assert.Equal(t, 23, stored.Identifier)
	assert.Equal(t, result.Attempts, stored.Attempts)
	assert.False(t, stored.FinishedAt.IsZero())
}

func TestCoATaskNAKIsTerminalState(t *testing.T) {
	db := coaTaskTestDB(t)
	mgr := NewCoATaskManager(db)
	task := coaTestTask()
	task.Action = "coa"
	task.FilterID = "limited"
	task.SessionTimeoutSeconds = 600
	require.NoError(t, mgr.Create(context.Background(), task))

	client := NewCoAService(nil)
	client.exchange = func(ctx context.Context, packet *radius.Packet, addr string) (*radius.Packet, error) {
		assert.Equal(t, radius.CodeCoARequest, packet.Code)
		require.NoError(t, rfc2865.FilterID_SetString(packet, "limited"))
		resp := radius.New(radius.CodeCoANAK, []byte("secret"))
		require.NoError(t, rfc3576.ErrorCause_Set(resp, rfc3576.ErrorCause_Value_SessionContextNotFound))
		return resp, nil
	}
	_, err := mgr.Execute(context.Background(), task, client)
	assert.Error(t, err)

	var stored domain.RadiusCoATask
	require.NoError(t, db.First(&stored, task.ID).Error)
	assert.Equal(t, "nak", stored.Status)
	assert.Equal(t, 503, stored.ErrorCause)
}

func TestRetryPendingUsesStoredIdentifier(t *testing.T) {
	db := coaTaskTestDB(t)
	mgr := NewCoATaskManager(db)
	task := coaTestTask()
	require.NoError(t, mgr.Create(context.Background(), task))
	require.NoError(t, db.Model(&domain.RadiusCoATask{}).Where("id = ?", task.ID).
		Updates(map[string]interface{}{"status": "sending", "identifier": 42, "attempts": 1, "sent_at": time.Now()}).Error)

	client := NewCoAService(nil)
	client.exchange = func(ctx context.Context, packet *radius.Packet, addr string) (*radius.Packet, error) {
		assert.Equal(t, byte(42), packet.Identifier)
		return radius.New(radius.CodeDisconnectACK, []byte("secret")), nil
	}
	count, err := mgr.RetryPending(context.Background(), client, 10)
	require.NoError(t, err)
	assert.Equal(t, 1, count)

	var stored domain.RadiusCoATask
	require.NoError(t, db.First(&stored, task.ID).Error)
	assert.Equal(t, "ack", stored.Status)
}
