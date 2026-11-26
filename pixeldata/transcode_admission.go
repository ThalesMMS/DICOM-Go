package pixeldata

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata/encapsulated"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// CheckTranscodeDecodeLimits applies to file, before anything is decoded,
// the content-derived limits TranscodePath enforces when it converts file to
// target with options: Number of Frames (TranscodeLimits.MaxFrames) and
// Rows * Columns * Number of Frames (MaxPixels), counted over the image and
// its icons together when it has Icon Image Sequences, and, for an encapsulated
// source it decodes, the Pixel Data input and predicted decoded bytes
// (MaxInputBytes, MaxOutputBytes) and the ratio of the decoded bytes to the
// encapsulated input (MaxExpansionRatio), for the whole object and for each
// frame as the streaming route admits them. TranscodePath checks them only
// once it converts, so a caller that must refuse an object before it starts
// an answer, such as a multipart response, calls this first.
//
// The image attributes of file must be read. An encapsulated source also
// needs its Pixel Data, in memory or deferred
// (object.ReadFileOptions.DeferPixelData), in which case source is the
// seekable input it was read from, and a complete read; a native source may
// be read only up to its Pixel Data (object.ReadFileOptions.StopBeforeTag)
// and needs no source. The check
// shares its inputs and arithmetic with TranscodePath's admission, so it
// refuses what TranscodePath would refuse on these limits; where only one of
// TranscodePath's routes applies a limit it applies it as well, so it errs
// only towards refusal.
//
// It returns nil when the conversion copies the Pixel Data without decoding
// it. A refused object returns an error matching ErrDecompressResourceLimit,
// ErrTranscodeResourceLimit or encapsulated.ErrResourceLimit, which
// IsTranscodeLimitError reports; other errors describe a file whose
// attributes or Pixel Data layout cannot be read.
func CheckTranscodeDecodeLimits(ctx context.Context, file *object.File, source io.ReaderAt, target transfer.Syntax, options TranscodeOptions) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if file == nil || file.Dataset == nil {
		return fmt.Errorf("%w: nil file", ErrMissingMetadata)
	}
	limits, err := ResolveTranscodeLimits(options.Limits)
	if err != nil {
		return err
	}
	syntax, err := sourceTransferSyntax(file)
	if err != nil {
		return err
	}
	target, err = canonicalTargetTransferSyntax(target)
	if err != nil {
		return err
	}
	dataset := file.Dataset
	// An object with Icon Image Sequences takes the whole-object route,
	// which bounds the frames and pixels of the image and its icons
	// together before it converts anything.
	main, icons, err := iconTranscodeParts(ctx, dataset, limits)
	if err != nil {
		return err
	}
	if len(icons) > 0 {
		if err := checkIconPixelFrameBudget(ctx, main, icons, dataset.ValueByteOrder(), limits); err != nil {
			return err
		}
	}
	if syntax.Encapsulated {
		if _, err := strictFileTransferSyntax(file); err != nil {
			return err
		}
	} else {
		// Native samples are copied or rearranged, never decompressed: the
		// whole-object route bounds the frames and pixels of an object whose
		// image attributes are complete and copies the others, and the byte
		// limits follow from the stored size. The Pixel Data need not be read.
		metadata, err := ExtractMetadata(dataset)
		if err != nil {
			return nil
		}
		return validateTranscodeMetadataBounds(metadata, limits)
	}
	plan := buildTranscodePlan(syntax, target, options.ForceReencode, hasAnyPixelData(dataset))
	if plan.mode == transcodeMissingPixelData || plan.mode == transcodeEquivalentClone {
		return nil
	}
	metadata, err := ExtractMetadata(dataset)
	if err != nil {
		return err
	}
	// checkDecodeAdmission covers the frames and pixels bounds the
	// whole-object route also checks before it decodes.
	decode := transcodeDecompressLimits(limits)
	reader, err := NewFrameReader(ctx, file, source, sourceFrameReaderOptions(options, limits))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		// TranscodePath decodes a source it cannot index on its whole-object
		// route, which bounds the frames and pixels first and then admits the
		// whole object only.
		if err := validateTranscodeMetadataBounds(metadata, limits); err != nil {
			return err
		}
		inputBytes, err := wholePixelInputBytes(file, source, limits)
		if err != nil {
			return err
		}
		return checkDecodeAdmission(decode, metadata, syntax, inputBytes)
	}
	defer reader.Close()
	if err := checkDecodeAdmission(decode, metadata, syntax, reader.inputBytes); err != nil {
		return err
	}
	// The streaming route admits each frame again as it decodes it. Within the
	// whole-object limits only the expansion ratio of a frame can exceed them.
	if syntax.UID == transfer.EncapsulatedUncompressedExplicitVRLittleEndian.UID || reader.native != nil || reader.collection != nil {
		return nil
	}
	for i := range metadata.NumberOfFrames {
		if _, err := reader.admittedFrameInputBytes(ctx, i); err != nil {
			if ctx.Err() != nil || IsTranscodeLimitError(err) {
				return err
			}
			// Left to TranscodePath, which reports what it cannot read.
			return nil
		}
	}
	return nil
}

// IsTranscodeLimitError reports whether err is the refusal of a transcode,
// decompression or encapsulated frame index on one of its resource limits.
func IsTranscodeLimitError(err error) bool {
	return errors.Is(err, ErrDecompressResourceLimit) || errors.Is(err, ErrTranscodeResourceLimit) || errors.Is(err, encapsulated.ErrResourceLimit)
}

// wholePixelInputBytes counts what the whole-object route decodes an
// encapsulated source from: the Basic Offset Table plus the fragments.
func wholePixelInputBytes(file *object.File, source io.ReaderAt, limits TranscodeLimits) (int64, error) {
	pixel, ok := file.Dataset.Get(core.TagPixelData)
	if !ok {
		return 0, ErrPixelDataNotFound
	}
	if pixel.Value != nil {
		view, err := pixelDataViewFromElement(pixel)
		if err != nil {
			return 0, err
		}
		return pixelPayloadBytes(view), nil
	}
	locations := file.ValueLocations(core.TagPixelData)
	if source == nil || len(locations) != 1 {
		return 0, fmt.Errorf("%w: deferred Pixel Data needs its seekable source", ErrFrameLayout)
	}
	// That route reads the Basic Offset Table whatever its size.
	items, basic, err := scanItems(source, locations[0].ValueOffset, locations[0].Length, limits.MaxFragments, math.MaxInt)
	if err != nil {
		return 0, err
	}
	total := int64(len(basic))
	for _, item := range items {
		total += int64(item.Length)
	}
	return total, nil
}
