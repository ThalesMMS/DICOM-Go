package jpegls

import (
	"context"
	"fmt"

	"github.com/ThalesMMS/dicom-go/internal/decoderecovery"
	"github.com/ThalesMMS/dicom-go/pixeldata/encapsulated"
)

// CodestreamInfo describes headers of the existing qualified JPEG-LS framing
// parser. It does not prove that entropy decodes or that a source was unsigned.
type CodestreamInfo struct {
	Rows, Columns, Components, Precision, Near, Interleave, MaxValue int
	// HPColorTransform is nonzero only with explicit HP recovery in ctx.
	HPColorTransform int
}

// InspectCodestream reads coding metadata without reconstructing samples.
// maxBytes zero selects256MiB; negative refuses. Context surrounds the existing
// bounded private header parser and is polled by structural framing validation.
func InspectCodestream(ctx context.Context, data []byte, maxBytes int64) (CodestreamInfo, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return CodestreamInfo{}, err
	}
	if maxBytes < 0 {
		return CodestreamInfo{}, ErrInvalidCodestream
	}
	if maxBytes == 0 {
		maxBytes = 256 << 20
	}
	if int64(len(data)) > maxBytes {
		return CodestreamInfo{}, fmt.Errorf("%w: input byte limit", ErrInvalidCodestream)
	}
	if err := encapsulated.ValidateFrame(ctx, data, encapsulated.JPEGLS); err != nil {
		return CodestreamInfo{}, err
	}
	h, _, err := splitScansWithHP(data, decoderecovery.From(ctx).JPEGLSHPColorTransforms)
	if err != nil {
		return CodestreamInfo{}, err
	}
	if err = ctx.Err(); err != nil {
		return CodestreamInfo{}, err
	}
	return CodestreamInfo{Rows: h.rows, Columns: h.columns, Components: h.components, Precision: h.precision, Near: h.near, Interleave: h.ilv, MaxValue: h.preset.maxval, HPColorTransform: h.hpTransform}, nil
}
