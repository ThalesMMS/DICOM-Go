package pixeldata

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata/encapsulated"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// DecodeFileFramesView decodes all frames in a materialized DICOM file.
// Native frames may borrow the dataset's stored bytes and are read-only.
// Encapsulated frames own their decoded bytes. The boolean is true for
// encapsulated data and the external video and JPIP routes. On success,
// metadata describes the returned samples, keeping the source frame count.
// A nil registry uses DefaultRegistry. For deferred Pixel Data use FrameReader.
func DecodeFileFramesView(ctx context.Context, file *object.File, registry Registry) (metadata Metadata, frames [][]byte, encapsulated bool, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	metadata, pixels, encapsulated, err := fileFramePixels(ctx, file)
	if err != nil {
		return metadata, nil, encapsulated, err
	}
	return decodeFileFrames(ctx, file, metadata, pixels, encapsulated, registry)
}

// DecodeFileFrame returns an owned frame from a materialized DICOM file.
// It uses FrameReader to decode just index when the encapsulated frames
// can be addressed individually. Codecs with layouts that cannot be indexed
// still use batch decoding. Native samples keep the same stored byte order
// and packing as DecodeFileFramesView.
func DecodeFileFrame(ctx context.Context, file *object.File, index int, registry Registry) (Metadata, []byte, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	metadata, pixels, special, err := fileFramePixels(ctx, file)
	if err != nil {
		return metadata, nil, special, err
	}
	uid := transfer.NormalizeUID(file.TransferSyntax.UID)
	external := transfer.IsVideoTransferSyntax(uid) || IsJPIPReferencedTransferSyntax(uid)
	if !external && (index < 0 || index >= metadata.NumberOfFrames) {
		return metadata, nil, special, fileFrameIndexError(index, metadata.NumberOfFrames)
	}
	if !external && pixels.Encapsulated && metadata.NumberOfFrames > 1 {
		reader, err := NewFrameReader(ctx, file, nil, FrameReaderOptions{Registry: registry})
		if err == nil {
			frame, decoded, err := reader.Frame(ctx, index)
			if err != nil {
				return metadata, nil, true, err
			}
			decoded.NumberOfFrames = metadata.NumberOfFrames
			return decoded, frame, true, nil
		}
		// Layouts such as multifragment Deflated Image Frame Compression
		// can be accepted by a batch codec even if FrameReader cannot
		// locate the frames. Resource refusals and cancellation still apply.
		if !errors.Is(err, transfer.ErrUnknownTransferSyntax) &&
			(!errors.Is(err, ErrFrameLayout) || isContextTermination(err) ||
				errors.Is(err, encapsulated.ErrResourceLimit) || errors.Is(err, ErrDecompressResourceLimit)) {
			return metadata, nil, true, err
		}
	}
	var frames [][]byte
	if special {
		metadata, frames, special, err = decodeFileFrames(ctx, file, metadata, pixels, special, registry)
	} else {
		frames, err = splitNativeFrameRange(pixels, file.Dataset, file.Dataset.ValueByteOrder(), metadata, index, index+1)
		index = 0
	}
	if err != nil {
		return metadata, nil, special, err
	}
	if index >= len(frames) {
		return metadata, nil, special, fileFrameIndexError(index, len(frames))
	}
	frame := core.CloneBytes(frames[index])
	if err := ctx.Err(); err != nil {
		return metadata, nil, special, err
	}
	return metadata, frame, special, nil
}

func fileFrameIndexError(index, count int) error {
	return fmt.Errorf("%w: decoded frame index %d out of range [0,%d)", ErrInvalidMetadata, index, count)
}

func fileFramePixels(ctx context.Context, file *object.File) (Metadata, PixelData, bool, error) {
	if err := ctx.Err(); err != nil {
		return Metadata{}, PixelData{}, false, err
	}
	if file == nil || file.Dataset == nil {
		return Metadata{}, PixelData{}, false, fmt.Errorf("%w: file has no dataset", ErrMissingMetadata)
	}
	if _, partial := file.PartialRead(); partial {
		return Metadata{}, PixelData{}, false, object.ErrPartialFile
	}
	if _, incomplete := file.Dataset.IncompletePixelData(); incomplete {
		return Metadata{}, PixelData{}, false, object.ErrPartialFile
	}
	uid := transfer.NormalizeUID(file.TransferSyntax.UID)
	metadata, metaErr := ExtractMetadata(file.Dataset)
	if IsJPIPReferencedTransferSyntax(uid) {
		return metadata, PixelData{}, true, metaErr
	}
	pixels, err := ExtractView(file.Dataset)
	if transfer.IsVideoTransferSyntax(uid) {
		if metaErr != nil {
			return Metadata{}, PixelData{}, true, metaErr
		}
		if err != nil {
			return metadata, PixelData{}, true, fmt.Errorf("%w: %v", mediaPayloadNotRenderableError(uid, pixels), err)
		}
		return metadata, pixels, true, nil
	}
	if err != nil {
		return metadata, PixelData{}, false, err
	}
	return metadata, pixels, pixels.Encapsulated, metaErr
}

func decodeFileFrames(ctx context.Context, file *object.File, metadata Metadata, pixels PixelData, special bool, registry Registry) (Metadata, [][]byte, bool, error) {
	if !special {
		frames, err := splitNativeFrameRange(pixels, file.Dataset, file.Dataset.ValueByteOrder(), metadata, 0, metadata.NumberOfFrames)
		if err != nil {
			return metadata, nil, false, err
		}
		if err := ctx.Err(); err != nil {
			return metadata, nil, false, err
		}
		return metadata, frames, false, nil
	}
	if registry == nil {
		registry = DefaultRegistry
	}
	frames, err := decodeFramesWithRegistry(ctx, registry, file.TransferSyntax.UID, pixels, file.Dataset)
	if err != nil {
		return metadata, nil, true, err
	}
	metadata = decompressedOutputMetadata(metadata, file.TransferSyntax, frames)
	metadata.PhotometricInterpretation = strings.Trim(metadata.PhotometricInterpretation, " \x00")
	if metadata.PhotometricInterpretation == "" {
		return metadata, nil, true, missingMetadataError("PhotometricInterpretation", tagPhotometricInterpretation)
	}
	return metadata, frames.Data, true, nil
}
