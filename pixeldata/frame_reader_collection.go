package pixeldata

import (
	"context"
	"fmt"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/j2kpart2"
	"github.com/ThalesMMS/dicom-go/object"
)

func (r *FrameReader) indexPart2Collection(ctx context.Context, file *object.File) error {
	if len(r.basic) != 0 || file.Dataset.Has(tagExtendedOffsetTable) || file.Dataset.Has(tagExtendedOffsetTableLengths) {
		return fmt.Errorf("%w: Annex J collection requires empty offset tables", ErrFrameLayout)
	}
	size, err := r.fragments.FragmentSize(0)
	if err != nil {
		return err
	}
	limits, err := normalizedDecompressLimits(r.options.Limits)
	if err != nil {
		return err
	}
	if size > 32<<20 || size > uint64(limits.MaxInputBytes) {
		return ErrTranscodeResourceLimit
	}
	data := make([]byte, int(size))
	if err := r.readFrameFragment(ctx, 0, data); err != nil {
		return err
	}
	layout, err := j2kpart2.Inspect(data)
	if err != nil || layout.Frames != r.metadata.NumberOfFrames || layout.Rows != int(r.metadata.Rows) ||
		layout.Columns != int(r.metadata.Columns) || layout.Bits != int(r.metadata.BitsStored) ||
		layout.Signed != (r.metadata.PixelRepresentation == 1) {
		return fmt.Errorf("%w: invalid Annex J collection", ErrFrameLayout)
	}
	r.collection = data
	return nil
}

func (r *FrameReader) decodePart2CollectionFrame(ctx context.Context, i int, report *DecompressReport, withObject bool) (*object.Object, []byte, Metadata, error) {
	// Each call owns its complete decode. No cached samples survive a
	// registry replacement, and concurrent requests cannot race over buffers.
	proxy := object.FromElements(r.file.Dataset.Elements(), nil)
	proxy.Put(core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB, Length: core.UndefinedLength, LengthSet: true},
		Value: core.FragmentSequence{Fragments: [][]byte{r.collection}}})
	decoded, _, err := DecompressDataSetContext(ctx, proxy, r.syntax, DecompressOptions{
		Registry: r.options.Registry, Limits: r.options.Limits, Report: report,
	})
	if err != nil {
		return nil, nil, Metadata{}, err
	}
	defer decoded.Close()
	frames, err := ExtractNativeFramesView(decoded)
	if err != nil || len(frames.Data) != r.metadata.NumberOfFrames {
		return nil, nil, Metadata{}, ErrFrameLayout
	}
	raw := core.CloneBytes(frames.Data[i])
	metadata := frames.Metadata
	metadata.NumberOfFrames = 1
	if !withObject {
		return nil, raw, metadata, nil
	}
	output, err := datasetWithNativePixelData(r.newDecodeProxy(), metadata, raw, r.syntax)
	return output, raw, metadata, err
}
