package audit

import (
	"context"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/internal/sourceport/sourceporttest"
)

// TestSyslogTCPRetriesASourcePortCollision: the audit logger connects again
// when the source port the system chose collides with a TIME_WAIT connection
// to the audit record repository, instead of failing the flush (#2011).
func TestSyslogTCPRetriesASourcePortCollision(t *testing.T) {
	address, accepted := sourceporttest.Server(t)
	logger := &SyslogLogger{Network: SyslogTCP, Address: address}
	defer logger.Close()
	if err := logger.LogRaw(5, time.Now(), []byte("<AuditMessage/>")); err != nil {
		t.Fatal(err)
	}
	ctx, collided := sourceporttest.Context(t, context.Background(), address)
	if err := logger.Flush(ctx); err != nil {
		t.Fatalf("flush after a source port collision: %v", err)
	}
	if !collided() || !accepted(2) {
		t.Fatalf("collided = %t, connected = %t; want the dial to meet the collision and connect again", collided(), accepted(2))
	}
}
