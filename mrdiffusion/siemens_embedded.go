package mrdiffusion

import (
	"context"
	"fmt"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/siemens"
)

// SourceSiemensCSAEmbedded identifies the MRDiffusion base64 dataset in the
// Siemens CSA image header. Reading it requires explicit caller opt-in.
const SourceSiemensCSAEmbedded Source = "siemens-csa-embedded"

// SiemensEmbeddedOptions preserves the explicit private-creator lookup policy
// and bounds both the CSA header and the decoded MRDiffusion dataset.
type SiemensEmbeddedOptions struct {
	Lookup siemens.LookupOptions
	Limits siemens.EmbeddedDatasetLimits
}

// Keep legacy semantic causes inspectable, while avoiding raw textual numeric
// values in the new opt-in reader's diagnostic string.
type embeddedDiffusionError struct{ cause error }

func (e *embeddedDiffusionError) Error() string {
	return "dicom/mrdiffusion: embedded diffusion attribute rejected"
}

func (e *embeddedDiffusionError) Unwrap() error { return e.cause }

func embeddedStandardFrame(dataset *object.Object) (Frame, error) {
	if gradients, ok := dataset.GetSequence(tagDiffusionGradientDirSeq); ok && (len(gradients) != 1 || gradients[0] == nil) {
		return Frame{}, fmt.Errorf("%w: embedded gradient item count", ErrInvalid)
	}
	frame, err := standardFrame(dataset, SourceSiemensCSAEmbedded)
	if err != nil {
		return Frame{}, &embeddedDiffusionError{cause: err}
	}
	return frame, nil
}

// ReadSiemensEmbeddedContext reads MRDiffusion from the CSA image header without
// changing Read's historical source precedence or decoding defaults. The
// embedded raw EVRLE dataset may carry the standard attributes at the top level,
// a single MR Diffusion Sequence item, or Shared/Per-Frame Functional Groups.
// Conflicting functional groups or multi-item macros are rejected instead of
// choosing an arbitrary item. The returned Tag locates the outer CSA header.
func ReadSiemensEmbeddedContext(ctx context.Context, dataset *object.Object, frameIndex int, opts SiemensEmbeddedOptions) (Frame, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Frame{}, err
	}
	if frameIndex < 0 {
		return Frame{}, fmt.Errorf("%w: negative frame index", ErrInvalid)
	}
	header, err := siemens.ImageHeader(dataset, opts.Lookup)
	if err != nil {
		return Frame{}, err
	}
	if err := ctx.Err(); err != nil {
		return Frame{}, err
	}
	embedded, err := header.EmbeddedDatasetContext(ctx, "MRDiffusion", opts.Limits)
	if err != nil {
		return Frame{}, err
	}
	defer embedded.Close()
	items, source, err := embedded.FunctionalGroupItems(frameIndex, tagMRDiffusionSequence)
	if err != nil {
		return Frame{}, fmt.Errorf("%w: embedded functional groups conflict", ErrInvalid)
	}
	var frame Frame
	if len(items) > 0 {
		if len(items) != 1 {
			return Frame{}, fmt.Errorf("%w: embedded macro item count", ErrInvalid)
		}
		frame, err = embeddedStandardFrame(items[0])
		frame.FunctionalGroup = source
	} else if items, ok := embedded.GetSequence(tagMRDiffusionSequence); ok {
		if len(items) != 1 || items[0] == nil {
			return Frame{}, fmt.Errorf("%w: embedded macro item count", ErrInvalid)
		}
		frame, err = embeddedStandardFrame(items[0])
	} else {
		if !embedded.Has(tagDiffusionBValue) {
			return Frame{}, ErrNotFound
		}
		values, valueErr := numbers(embedded, tagDiffusionBValue, core.VRFD)
		if valueErr != nil {
			return Frame{}, &embeddedDiffusionError{cause: valueErr}
		}
		if len(values) == 0 {
			return Frame{}, ErrNotFound
		}
		frame, err = embeddedStandardFrame(embedded)
	}
	if err != nil {
		return Frame{}, err
	}
	if err := ctx.Err(); err != nil {
		return Frame{}, err
	}
	frame.FrameIndex = frameIndex
	frame.Source = SourceSiemensCSAEmbedded
	frame.Tag = header.Tag
	return frame, nil
}
