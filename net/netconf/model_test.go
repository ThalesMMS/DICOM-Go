package netconf

import (
	"context"
	"errors"
	"testing"
)

func TestCallerModelsValidateWithoutMutation(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Device)
		want error
	}{
		{"duplicate canonical AE", func(d *Device) { d.ApplicationEntities[1].AETitle = " CT_SCP " }, ErrInvalid},
		{"case remains distinct", func(d *Device) { d.ApplicationEntities[1].AETitle = "ct_scp" }, nil},
		{"dangling", func(d *Device) { d.ApplicationEntities[0].ConnectionIDs[0] = "missing" }, ErrInvalid},
		{"duplicate connection", func(d *Device) { d.Connections = append(d.Connections, d.Connections[0]) }, ErrInvalid},
		{"wildcard SOP", func(d *Device) { d.ApplicationEntities[0].TransferCapabilities[0].SOPClassUID = "*" }, ErrInvalid},
		{"role", func(d *Device) { d.ApplicationEntities[0].TransferCapabilities[0].Role = "BOTH" }, ErrInvalid},
		{"duplicate TS", func(d *Device) {
			c := &d.ApplicationEntities[0].TransferCapabilities[0]
			c.TransferSyntaxUIDs = append(c.TransferSyntaxUIDs, c.TransferSyntaxUIDs[0])
		}, ErrInvalid},
		{"duplicate role", func(d *Device) {
			a := &d.ApplicationEntities[0]
			a.TransferCapabilities = append(a.TransferCapabilities, a.TransferCapabilities[0])
		}, ErrInvalid},
		{"invalid host", func(d *Device) { d.Connections[0].Hostname = "bad/host" }, ErrInvalid},
		{"port", func(d *Device) { d.Connections[0].Port = 65536 }, ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := fixture(t)
			tc.edit(d)
			before := d.ApplicationEntities[0].AETitle
			err := Validate(context.Background(), d, Options{})
			if !errors.Is(err, tc.want) {
				t.Fatalf("error %v want %v", err, tc.want)
			}
			if d.ApplicationEntities[0].AETitle != before {
				t.Fatal("source changed")
			}
		})
	}
}
func TestFiniteLimitsRejectInvalidSettings(t *testing.T) {
	for _, l := range []Limits{{MaxAEs: -1}, {MaxJSONBytes: -1}, {MaxDepth: 257}, {MaxCompiledBytes: -1}} {
		if _, err := CompileConnection(context.Background(), fixture(t), "", Options{Limits: l}); !errors.Is(err, ErrResourceLimit) {
			t.Fatalf("limits %+v: %v", l, err)
		}
	}
}
