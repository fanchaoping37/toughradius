package radiusd

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/talkincode/toughradius/v9/internal/domain"
	"layeh.com/radius"
	"layeh.com/radius/rfc2865"
	"layeh.com/radius/rfc2866"
)

func TestDialRejectCacheTTLAndCleanup(t *testing.T) {
	cache := newDialRejectCacheWithTTL(40*time.Millisecond, 10*time.Millisecond)
	defer cache.Close()

	cache.Set("alice", "operator: password expired")
	got, ok := cache.Get("alice")
	require.True(t, ok)
	assert.Equal(t, "operator: password expired", got)

	// A miss must not fabricate a reason, and expired entries disappear both
	// on Get (lazy) and via the background cleaner.
	time.Sleep(90 * time.Millisecond)
	got, ok = cache.Get("alice")
	assert.False(t, ok)
	assert.Empty(t, got)
	cache.mu.Lock()
	assert.Empty(t, cache.entries)
	cache.mu.Unlock()
}

func TestDialRejectCacheDeleteAndConcurrentAccess(t *testing.T) {
	cache := newDialRejectCache()
	defer cache.Close()

	cache.Set("alice", "first")
	cache.Delete("alice")
	_, ok := cache.Get("alice")
	assert.False(t, ok)

	// Set/Get/Delete from many goroutines must never race or lose the map.
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			username := "user" + string(rune('a'+i))
			cache.Set(username, "reason")
			cache.Get(username)
			cache.Delete(username)
		}(i)
	}
	wg.Wait()
}

// swapDialRejectCache isolates the process-wide cache used by the auth stage.
func swapDialRejectCache(t *testing.T) {
	t.Helper()
	previous := dialRejectReasons
	dialRejectReasons = newDialRejectCacheWithTTL(time.Hour, time.Hour)
	t.Cleanup(func() {
		dialRejectReasons.Close()
		dialRejectReasons = previous
	})
}

func dialProxyAuthContext(username string) (*AuthPipelineContext, *captureResponseWriter) {
	writer := &captureResponseWriter{}
	return &AuthPipelineContext{
		Context:  context.Background(),
		Writer:   writer,
		Request:  &radius.Request{Packet: radius.New(radius.CodeAccessRequest, []byte("nassecret"))},
		Username: username,
		NAS:      &domain.NetNas{Secret: "nassecret"},
	}, writer
}

func dialProxyAuthService() *AuthService {
	return &AuthService{RadiusService: &RadiusService{appCtx: &metricsTestAppContext{}}}
}

func TestDialRejectCacheStageCachesAndEchoesReason(t *testing.T) {
	swapDialRejectCache(t)
	svc := dialProxyAuthService()

	// First upstream reject carries the operator's reason.
	ctx, writer := dialProxyAuthContext("alice")
	upstream := radius.New(radius.CodeAccessReject, []byte("upsecret"))
	require.NoError(t, rfc2865.ReplyMessage_SetString(upstream, "password expired"))
	svc.sendProxiedReject(ctx, upstream)
	require.NotNil(t, writer.last)
	assert.Equal(t, radius.CodeAccessReject, writer.last.Code)
	assert.Equal(t, "password expired", rfc2865.ReplyMessage_GetString(writer.last))

	// A later synthesized reject with no Reply-Message still tells the NAS why,
	// but stays a reject: cached text never changes the authentication result.
	ctx2, writer2 := dialProxyAuthContext("alice")
	svc.sendProxiedReject(ctx2, radius.New(radius.CodeAccessReject, []byte("upsecret")))
	require.NotNil(t, writer2.last)
	assert.Equal(t, radius.CodeAccessReject, writer2.last.Code)
	assert.Equal(t, "password expired", rfc2865.ReplyMessage_GetString(writer2.last))
}

func TestDialRejectCacheStageAcceptClearsReason(t *testing.T) {
	swapDialRejectCache(t)
	svc := dialProxyAuthService()

	dialRejectReasons.Set("bob", "previous failure")
	ctx, writer := dialProxyAuthContext("bob")
	svc.sendProxiedAccept(ctx, radius.New(radius.CodeAccessAccept, []byte("upsecret")))
	require.NotNil(t, writer.last)
	assert.Equal(t, radius.CodeAccessAccept, writer.last.Code)
	// A cached reject reason must never be attached to an Accept.
	assert.Empty(t, rfc2865.ReplyMessage_GetString(writer.last))
	_, ok := dialRejectReasons.Get("bob")
	assert.False(t, ok)

	// After the Accept there is no stale reason to echo on a later reject.
	ctx2, writer2 := dialProxyAuthContext("bob")
	svc.sendProxiedReject(ctx2, nil)
	require.NotNil(t, writer2.last)
	assert.Equal(t, radius.CodeAccessReject, writer2.last.Code)
	assert.Empty(t, rfc2865.ReplyMessage_GetString(writer2.last))
}

// TestPanabitStopDisconnectToUpstream is a lightweight, network-free check of
// the PANABIT Stop side-channel: it targets the upstream CoA port with the
// upstream secret and replays only session identifiers from the request.
func TestPanabitStopDisconnectToUpstream(t *testing.T) {
	previous := dialDisconnectExchange
	defer func() { dialDisconnectExchange = previous }()

	var gotAddr string
	var gotPacket *radius.Packet
	done := make(chan struct{})
	dialDisconnectExchange = func(_ context.Context, packet *radius.Packet, addr string) (*radius.Packet, error) {
		gotPacket = packet
		gotAddr = addr
		close(done)
		return radius.New(radius.CodeDisconnectACK, []byte("upsecret")), nil
	}

	orig := radius.New(radius.CodeAccountingRequest, []byte("nassecret"))
	require.NoError(t, rfc2865.UserName_SetString(orig, "alice"))
	require.NoError(t, rfc2865.CallingStationID_SetString(orig, "00:11:22:33:44:55"))
	require.NoError(t, rfc2865.NASIPAddress_Set(orig, net.ParseIP("192.168.1.10")))
	require.NoError(t, rfc2866.AcctSessionID_SetString(orig, "sess-42"))
	require.NoError(t, rfc2866.AcctStatusType_Set(orig, rfc2866.AcctStatusType_Value_Stop))

	up := &domain.RadiusUpstream{Name: "carrier", Addr: "10.0.0.1", Secret: "upsecret", CoaPort: 0}
	(&AcctService{}).sendPanabitStopDisconnect(context.Background(), &radius.Request{Packet: orig}, up, "alice")

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("disconnect exchange was not invoked")
	}

	assert.Equal(t, "10.0.0.1:3799", gotAddr)
	require.NotNil(t, gotPacket)
	assert.Equal(t, radius.CodeDisconnectRequest, gotPacket.Code)
	assert.Equal(t, []byte("upsecret"), gotPacket.Secret)
	assert.Equal(t, "alice", rfc2865.UserName_GetString(gotPacket))
	assert.Equal(t, "00:11:22:33:44:55", rfc2865.CallingStationID_GetString(gotPacket))
	assert.Equal(t, "sess-42", rfc2866.AcctSessionID_GetString(gotPacket))
	_, hasStatus := gotPacket.Attributes.Lookup(rfc2866.AcctStatusType_Type)
	assert.False(t, hasStatus, "a Disconnect-Request must not carry Acct-Status-Type")
}
