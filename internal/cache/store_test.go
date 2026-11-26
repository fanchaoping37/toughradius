package cache

import (
	"context"
	"testing"
	"time"
)

func TestMemoryStoreGetSetDelete(t *testing.T) {
	ctx := context.Background()
	s := NewMemory()
	if _, ok, _ := s.Get(ctx, "missing"); ok {
		t.Fatal("missing key returned")
	}
	if err := s.Set(ctx, "k", "v", time.Minute); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := s.Get(ctx, "k"); err != nil || !ok || v != "v" {
		t.Fatalf("get: %q %v %v", v, ok, err)
	}
	if err := s.Delete(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Get(ctx, "k"); ok {
		t.Fatal("deleted key still present")
	}
}

func TestMemoryStoreTTL(t *testing.T) {
	ctx := context.Background()
	s := NewMemory()
	if err := s.Set(ctx, "exp", "1", 20*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(40 * time.Millisecond)
	if _, ok, _ := s.Get(ctx, "exp"); ok {
		t.Fatal("expired key still returned")
	}
}

func TestMemoryStoreSetNX(t *testing.T) {
	ctx := context.Background()
	s := NewMemory()
	if ok, _ := s.SetNX(ctx, "idem", "resp", time.Minute); !ok {
		t.Fatal("first SetNX should win")
	}
	if ok, _ := s.SetNX(ctx, "idem", "resp2", time.Minute); ok {
		t.Fatal("second SetNX must lose")
	}
	if v, _, _ := s.Get(ctx, "idem"); v != "resp" {
		t.Fatalf("value overwritten: %s", v)
	}
	// After TTL, key can be reclaimed.
	if err := s.Set(ctx, "short", "x", 10*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	if ok, _ := s.SetNX(ctx, "short", "y", time.Minute); !ok {
		t.Fatal("expired key not reclaimed")
	}
}

func TestNewDisabledIsMemory(t *testing.T) {
	store, err := New(Config{Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store.(*MemoryStore); !ok {
		t.Fatalf("expected memory store, got %T", store)
	}
	if err := store.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}
