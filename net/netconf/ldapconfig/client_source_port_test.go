package ldapconfig

import (
	"context"
	"net/url"
	"testing"

	"github.com/ThalesMMS/dicom-go/internal/sourceport/sourceporttest"
)

// TestSessionDialRetriesASourcePortCollision: the directory connection is
// made again when the source port the system chose collides with a TIME_WAIT
// connection to the same directory server (#2011).
func TestSessionDialRetriesASourcePortCollision(t *testing.T) {
	address, accepted := sourceporttest.Server(t)
	options, err := normalized(Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, collided := sourceporttest.Context(t, context.Background(), address)
	err = withLDAPSession(ctx, Config{AllowAnonymousCleartext: true}, &url.URL{Scheme: "ldap", Host: address}, options, func(*ldapSession) error { return nil })
	if err != nil {
		t.Fatalf("session after a source port collision: %v", err)
	}
	if !collided() || !accepted(2) {
		t.Fatalf("collided = %t, connected = %t; want the dial to meet the collision and connect again", collided(), accepted(2))
	}
}
