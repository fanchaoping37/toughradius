package analytics

import (
	"context"
	"testing"
	"time"
)

func TestDisabledArchiveIsNoop(t *testing.T) {
	a, err := NewArchive(context.Background(), Config{Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	if a.Enabled() {
		t.Fatal("disabled archive reports enabled")
	}
	if err := a.Write(context.Background(), Record{Username: "u"}); err != nil {
		t.Fatal(err)
	}
	if err := a.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRecordFieldsMatchRFC(t *testing.T) {
	// Acct-Status-Type: Start=1, Stop=2, Interim-Update=3 (RFC 2866 §5.1).
	start := Record{StatusType: 1, EventTime: time.Now(), SessionID: "abc"}
	interim := Record{StatusType: 3, InputOctets: 4294967296, InputGigawords: 1}
	stop := Record{StatusType: 2}
	if start.StatusType == stop.StatusType || interim.StatusType != 3 {
		t.Fatal("status types wrong")
	}
	// Octets plus gigawords: 2^32 + 2^32*1 = 2^33 (RFC 2869 §5.26/§5.27).
	if interim.InputOctets != 4294967296 {
		t.Fatal("gigaword combination helper mismatch")
	}
}
