package pixeldata

import (
	"context"
	"fmt"

	"github.com/ThalesMMS/dicom-go/pixeldata/encapsulated"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// JPEGFrameInfo contains verifiable coding parameters from one structurally
// complete JPEG still-image frame. Sign and photometric interpretation are not
// encoded unambiguously by all JPEG processes and are deliberately not inferred.
// This header/framing inspection does not prove entropy decodability.
type JPEGFrameInfo struct {
	Rows, Columns, Components, Precision int
	TransferSyntaxUID                    string
	Predictor                            int // First SOS predictor; the selected syntax accounts for every scan.
}

// InspectJPEGFrame reuses the JPEG SOF and structural marker parsers. It accepts
// Baseline SOF0, Extended SOF1 and non-hierarchical Lossless SOF3; progressive,
// arithmetic and hierarchical coding are outside this inspection profile.
// maxBytes must be positive. A caller must bound decoded dimensions before
// allocating samples and use a syntax-specific decoder to validate entropy.
func InspectJPEGFrame(ctx context.Context, data []byte, maxBytes int64) (JPEGFrameInfo, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return JPEGFrameInfo{}, err
	}
	if maxBytes <= 0 || int64(len(data)) > maxBytes {
		return JPEGFrameInfo{}, fmt.Errorf("%w: JPEG input limit", ErrIncompatiblePixelData)
	}
	if err := encapsulated.ValidateFrame(ctx, data, encapsulated.JPEG); err != nil {
		return JPEGFrameInfo{}, err
	}
	h, ok := parseJPEGFrameHeader(data)
	if !ok || h.rows <= 0 || h.columns <= 0 || (h.components != 1 && h.components != 3) || h.precision < 2 || h.precision > 16 {
		return JPEGFrameInfo{}, ErrIncompatiblePixelData
	}
	info := JPEGFrameInfo{Rows: h.rows, Columns: h.columns, Components: h.components, Precision: h.precision}
	switch h.marker {
	case 0xc0:
		if h.precision != 8 {
			return JPEGFrameInfo{}, ErrIncompatiblePixelData
		}
		info.TransferSyntaxUID = transfer.JPEGBaseline.UID
	case 0xc1:
		if h.precision > 12 {
			return JPEGFrameInfo{}, ErrIncompatiblePixelData
		}
		info.TransferSyntaxUID = transfer.JPEGExtended.UID
	case 0xc3:
		predictors, err := encapsulated.JPEGScanSelectionValues(ctx, data)
		if err != nil {
			return JPEGFrameInfo{}, err
		}
		info.Predictor = predictors[0] // First scan; syntax classification uses all scans.
		info.TransferSyntaxUID = transfer.JPEGLosslessSV1.UID
		for _, predictor := range predictors {
			if predictor < 1 || predictor > 7 {
				return JPEGFrameInfo{}, ErrIncompatiblePixelData
			}
			if predictor != 1 {
				info.TransferSyntaxUID = transfer.JPEGLosslessNonHierarchical.UID
			}
		}
	default:
		return JPEGFrameInfo{}, ErrIncompatiblePixelData
	}
	return info, ctx.Err()
}
