package netconf

import (
	"context"
	"math"
	"unsafe"

	"github.com/ThalesMMS/dicom-go/net/ul"
)

// Limits are finite logical admission bounds, not process RSS ceilings. Zero
// fields choose defaults. Raw JSON, the model and a compiled catalog can coexist.
type Limits struct {
	MaxJSONBytes                                                                  int64
	MaxDepth, MaxScalarBytes, MaxNameBytes, MaxDescriptionBytes, MaxHostnameBytes int
	MaxConnections, MaxAEs, MaxCapabilities, MaxCapabilitiesPerAE                 int
	MaxTransferSyntaxes, MaxTransferSyntaxesPerCapability, MaxCompiledBytes       int
}
type Options struct{ Limits Limits }

func normalized(o Options) (Options, error) {
	d := Limits{MaxJSONBytes: 1 << 20, MaxDepth: 16, MaxScalarBytes: 8192, MaxNameBytes: 128, MaxDescriptionBytes: 4096, MaxHostnameBytes: 253, MaxConnections: 16, MaxAEs: 64, MaxCapabilities: 4096, MaxCapabilitiesPerAE: 1024, MaxTransferSyntaxes: 16384, MaxTransferSyntaxesPerCapability: 256, MaxCompiledBytes: 8 << 20}
	if o.Limits.MaxJSONBytes < 0 || o.Limits.MaxJSONBytes >= int64(math.MaxInt) {
		return o, ErrResourceLimit
	}
	if o.Limits.MaxJSONBytes == 0 {
		o.Limits.MaxJSONBytes = d.MaxJSONBytes
	}
	pairs := [][2]*int{{&o.Limits.MaxDepth, &d.MaxDepth}, {&o.Limits.MaxScalarBytes, &d.MaxScalarBytes}, {&o.Limits.MaxNameBytes, &d.MaxNameBytes}, {&o.Limits.MaxDescriptionBytes, &d.MaxDescriptionBytes}, {&o.Limits.MaxHostnameBytes, &d.MaxHostnameBytes}, {&o.Limits.MaxConnections, &d.MaxConnections}, {&o.Limits.MaxAEs, &d.MaxAEs}, {&o.Limits.MaxCapabilities, &d.MaxCapabilities}, {&o.Limits.MaxCapabilitiesPerAE, &d.MaxCapabilitiesPerAE}, {&o.Limits.MaxTransferSyntaxes, &d.MaxTransferSyntaxes}, {&o.Limits.MaxTransferSyntaxesPerCapability, &d.MaxTransferSyntaxesPerCapability}, {&o.Limits.MaxCompiledBytes, &d.MaxCompiledBytes}}
	for _, p := range pairs {
		if *p[0] < 0 || *p[0] > math.MaxInt/1024 {
			return o, ErrResourceLimit
		}
		if *p[0] == 0 {
			*p[0] = *p[1]
		}
	}
	// The parser uses a bounded recursive schema walk; callers cannot turn its
	// stack admission into an arbitrarily deep generic JSON traversal.
	if o.Limits.MaxDepth > 256 {
		return o, ErrResourceLimit
	}
	return o, nil
}

type budget struct{ used, max int }

func (b *budget) add(n int) error {
	if n < 0 || n > b.max-b.used {
		return ErrResourceLimit
	}
	b.used += n
	return nil
}
func modelFootprint(ctx context.Context, d *Device, l Limits) error {
	if len(d.DeviceName) > l.MaxNameBytes || len(d.Description) > l.MaxDescriptionBytes {
		return ErrResourceLimit
	}
	if len(d.Connections) > l.MaxConnections || len(d.ApplicationEntities) > l.MaxAEs {
		return ErrResourceLimit
	}
	b := budget{max: l.MaxCompiledBytes}
	if err := b.add(int(unsafe.Sizeof(Device{})) + len(d.DeviceName) + len(d.Description) + 64*(len(d.Connections)+len(d.ApplicationEntities))); err != nil {
		return err
	}
	caps, syntaxes := 0, 0
	for _, c := range d.Connections {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(c.ID) > l.MaxNameBytes || len(c.Hostname) > l.MaxHostnameBytes {
			return ErrResourceLimit
		}
		if err := b.add(int(unsafe.Sizeof(Connection{})) + len(c.ID) + len(c.Hostname) + 1); err != nil {
			return err
		}
	}
	for _, ae := range d.ApplicationEntities {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(ae.AETitle) > ul.MaxAETitleLength {
			return ErrResourceLimit
		}
		if len(ae.TransferCapabilities) > l.MaxCapabilitiesPerAE || len(ae.TransferCapabilities) > l.MaxCapabilities-caps || len(ae.ConnectionIDs) > l.MaxConnections {
			return ErrResourceLimit
		}
		caps += len(ae.TransferCapabilities)
		if err := b.add(int(unsafe.Sizeof(ApplicationEntity{})) + len(ae.AETitle) + 1 + 64*(len(ae.ConnectionIDs)+len(ae.TransferCapabilities))); err != nil {
			return err
		}
		for _, id := range ae.ConnectionIDs {
			if len(id) > l.MaxNameBytes {
				return ErrResourceLimit
			}
			if err := b.add(16 + len(id)); err != nil {
				return err
			}
		}
		for _, tc := range ae.TransferCapabilities {
			if len(tc.SOPClassUID) > 64 {
				return ErrResourceLimit
			}
			if len(tc.TransferSyntaxUIDs) > l.MaxTransferSyntaxesPerCapability || len(tc.TransferSyntaxUIDs) > l.MaxTransferSyntaxes-syntaxes {
				return ErrResourceLimit
			}
			syntaxes += len(tc.TransferSyntaxUIDs)
			if err := b.add(int(unsafe.Sizeof(TransferCapability{})) + len(tc.SOPClassUID) + len(tc.Role) + 64*len(tc.TransferSyntaxUIDs)); err != nil {
				return err
			}
			for _, ts := range tc.TransferSyntaxUIDs {
				if len(ts) > 64 {
					return ErrResourceLimit
				}
				if err := b.add(16 + len(ts)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
