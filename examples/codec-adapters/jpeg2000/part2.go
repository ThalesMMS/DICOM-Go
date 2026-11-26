package jpeg2000

import (
	"context"
	"encoding/binary"
	"fmt"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/examples/codec-adapters/jpeg2000/internal/j2kcore"
	"github.com/ThalesMMS/dicom-go/internal/j2kpart2"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// RegisterCorePart2 registers the bounded Annex J difference-collection
// decoder for .92 and .93. It accepts one 2–16-frame grayscale collection,
// with 1–16 stored bits for reversible input or eight bits for irreversible
// input. Other Part 2 transforms are refused explicitly.
func RegisterCorePart2(registry pixeldata.Registry) error {
	if registry == nil {
		return pixeldata.ErrCodecRegistryNil
	}
	for _, syntax := range []transfer.Syntax{transfer.JPEG2000Part2Lossless, transfer.JPEG2000Part2} {
		if err := registry.RegisterCodec(syntax.UID, part2Codec{lossless: syntax.UID == transfer.JPEG2000Part2Lossless.UID}); err != nil {
			return err
		}
	}
	return nil
}

type part2Codec struct{ lossless bool }

func (c part2Codec) Decode(pixel pixeldata.PixelData, obj *object.Object) (pixeldata.Frames, error) {
	return c.DecodeContext(context.Background(), pixel, obj)
}

func (c part2Codec) DecodeContext(ctx context.Context, pixel pixeldata.PixelData, obj *object.Object) (pixeldata.Frames, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return pixeldata.Frames{}, err
	}
	metadata, err := pixeldata.ExtractMetadata(obj)
	if err != nil {
		return pixeldata.Frames{}, err
	}
	if metadata.NumberOfFrames < 2 || metadata.NumberOfFrames > j2kpart2.MaxFrames ||
		int64(metadata.Rows)*int64(metadata.Columns)*int64(metadata.NumberOfFrames) > j2kpart2.MaxSamples {
		return pixeldata.Frames{}, ErrResourceLimit
	}
	if !pixel.Encapsulated || metadata.SamplesPerPixel != 1 || metadata.PlanarConfigurationPresent ||
		metadata.BitsAllocated != 8 && metadata.BitsAllocated != 16 ||
		metadata.HighBit != metadata.BitsStored-1 || metadata.PixelRepresentation > 1 ||
		metadata.PhotometricInterpretation != "MONOCHROME1" && metadata.PhotometricInterpretation != "MONOCHROME2" ||
		len(pixel.Sequence.Fragments) != 1 || len(pixel.Sequence.OffsetTable) != 0 ||
		obj.Has(core.NewTag(0x7fe0, 0x0001)) || obj.Has(core.NewTag(0x7fe0, 0x0002)) {
		return pixeldata.Frames{}, ErrUnsupportedMetadata
	}
	encoded := pixel.Sequence.Fragments[0]
	if len(encoded) > 32<<20 {
		return pixeldata.Frames{}, ErrResourceLimit
	}
	collection, err := j2kpart2.Inspect(encoded)
	if err != nil {
		return pixeldata.Frames{}, fmt.Errorf("%w: %w", ErrUnsupportedCodestreamFeature, err)
	}
	if collection.Frames != metadata.NumberOfFrames || collection.Rows != int(metadata.Rows) ||
		collection.Columns != int(metadata.Columns) || collection.Bits != int(metadata.BitsStored) ||
		collection.Signed != (metadata.PixelRepresentation == 1) || c.lossless && !collection.Reversible ||
		!collection.Reversible && metadata.BitsAllocated != 8 {
		return pixeldata.Frames{}, ErrUnsupportedMetadata
	}
	stream, err := j2kcore.ParseCodestream(collection.CodedComponents(encoded))
	if err != nil {
		return pixeldata.Frames{}, classifyCoreError(err)
	}
	decoded, err := j2kcore.DecodeContextOptions(ctx, stream, j2kcore.Limits{
		MaxComponents: j2kpart2.MaxFrames, MaxSamples: j2kpart2.MaxSamples,
		MaxTileWorkBytes: 128 << 20,
	}, j2kcore.DecodeOptions{AnnexJ: &collection.Profile})
	if err != nil {
		return pixeldata.Frames{}, classifyCoreError(err)
	}
	frames := make([][]byte, len(decoded.Components))
	for index, component := range decoded.Components {
		if err := ctx.Err(); err != nil {
			return pixeldata.Frames{}, err
		}
		frames[index] = make([]byte, metadata.FrameSize())
		for i, sample := range component {
			if metadata.BitsAllocated == 8 {
				frames[index][i] = byte(sample)
			} else {
				binary.LittleEndian.PutUint16(frames[index][2*i:], uint16(sample))
			}
		}
	}
	return pixeldata.Frames{Rows: int(metadata.Rows), Columns: int(metadata.Columns), Data: frames}, nil
}
