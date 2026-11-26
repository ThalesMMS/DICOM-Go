package netconf

import (
	"bytes"
	"context"
	"errors"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"reflect"
	"testing"
)

func TestCompileFrozenRolesAndDetachedViews(t *testing.T) {
	d := fixture(t)
	c, err := CompileConnection(context.Background(), d, "", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if c.Address() != "127.0.0.1:0" || c.TLSRequired() || !reflect.DeepEqual(c.AETitles(), []string{"CT_SCP", "MR_SCP"}) {
		t.Fatalf("connection %s/%v/%v", c.Address(), c.TLSRequired(), c.AETitles())
	}
	d.ApplicationEntities[0].AETitle = "changed"
	d.ApplicationEntities[0].TransferCapabilities[0].TransferSyntaxUIDs[0] = "1.2"
	view := c.Profiles()
	view[0].AETitle = "changed"
	view[0].Profile.Rules[0].Roles.SCU = false
	view[0].Profile.Rules[0].TransferSyntaxes[0] = "1.3"
	titles := c.AETitles()
	titles[0] = "changed"
	again := c.Profiles()
	if again[0].AETitle != "CT_SCP" || !again[0].Profile.Rules[0].Roles.SCU || again[0].Profile.Rules[0].Roles.SCP || again[0].Profile.Rules[0].TransferSyntaxes[0] != ul.ExplicitVRLittleEndian {
		t.Fatalf("mutable registry: %+v", again[0])
	}
}
func TestCompileInstalledSelectionAndDualRoles(t *testing.T) {
	no := false
	d := fixture(t)
	d.ApplicationEntities[0].Installed = &no
	c, err := CompileConnection(context.Background(), d, "loopback", Options{})
	if err != nil || !reflect.DeepEqual(c.AETitles(), []string{"MR_SCP"}) {
		t.Fatalf("disabled AE: %v/%v", c, err)
	}
	d.Installed = &no
	if c, err = CompileConnection(context.Background(), d, "", Options{}); c != nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("disabled Device: %v", err)
	}
	d = fixture(t)
	a := &d.ApplicationEntities[0]
	tc := a.TransferCapabilities[1]
	tc.Role = RoleSCU
	a.TransferCapabilities = append(a.TransferCapabilities, tc)
	c, err = CompileConnection(context.Background(), d, "", Options{})
	if err != nil {
		t.Fatal(err)
	}
	r := c.Profiles()[0].Profile.Rules[1]
	if !r.Roles.SCU || !r.Roles.SCP {
		t.Fatalf("roles %+v", r.Roles)
	}
	a.TransferCapabilities[2].TransferSyntaxUIDs = []string{ul.ImplicitVRLittleEndian}
	if c, err = CompileConnection(context.Background(), d, "", Options{}); c != nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("different dual TS: %v", err)
	}
}

func TestInactiveAECanRoundTripWithoutAListenerCapability(t *testing.T) {
	d := fixture(t)
	no := false
	d.ApplicationEntities[0].Installed = &no
	d.ApplicationEntities[0].AssociationAcceptor = false
	d.ApplicationEntities[0].TransferCapabilities = nil
	raw, err := Marshal(context.Background(), d, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Parse(context.Background(), bytes.NewReader(raw), Options{}); err != nil {
		t.Fatalf("inactive round trip: %v", err)
	}
	c, err := CompileConnection(context.Background(), d, "", Options{})
	if err != nil || !reflect.DeepEqual(c.AETitles(), []string{"MR_SCP"}) {
		t.Fatalf("inactive model %v/%v", c, err)
	}
}
