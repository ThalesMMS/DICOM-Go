package encapsulated

import (
	"context"

	"github.com/ThalesMMS/dicom-go/internal/decoderecovery"
)

// Recovery selects opt-in tolerances for malformed encapsulated layouts. The
// zero value is strict. Recoveries are carried in the context so codecs that
// only receive a context apply the caller's choice.
type Recovery struct {
	// IgnoreInconsistentBasicOffsetTable replaces a Basic Offset Table that
	// contradicts Number of Frames or the Item layout (wrong entry count,
	// unaligned, unordered or out-of-range offsets) with the bounded marker
	// scan used for an empty table. Exactly Number of Frames complete
	// codestreams must be found; frames are never invented or dropped.
	IgnoreInconsistentBasicOffsetTable bool
}

// WithRecovery returns ctx carrying r. pixeldata.DecompressOptions.Recovery
// sets the same context value.
func WithRecovery(ctx context.Context, r Recovery) context.Context {
	return decoderecovery.With(ctx, decoderecovery.Options{IgnoreInconsistentBasicOffsetTable: r.IgnoreInconsistentBasicOffsetTable})
}

// RecoveryFromContext returns the recoveries carried by ctx (strict if none).
func RecoveryFromContext(ctx context.Context) Recovery {
	o := decoderecovery.From(ctx)
	return Recovery{IgnoreInconsistentBasicOffsetTable: o.IgnoreInconsistentBasicOffsetTable}
}

// RecoveryReport describes a recovery a Plan applied. It holds counts and a
// structural reason only.
type RecoveryReport struct {
	BasicOffsetTableIgnored bool
	// BasicOffsetTableEntries is the number of 32-bit entries discarded.
	BasicOffsetTableEntries int
	// Frames is the number of codestreams the marker scan found.
	Frames int
	// Reason is the strict layout error that the recovery replaced.
	Reason string
}

// Recovery reports the recovery applied while building the plan, if any.
func (p *Plan) Recovery() RecoveryReport {
	if p == nil {
		return RecoveryReport{}
	}
	return p.recovery
}
