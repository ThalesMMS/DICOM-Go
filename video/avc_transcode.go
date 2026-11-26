package video

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/derivation"
	"github.com/ThalesMMS/dicom-go/dictionary/tags"
	"github.com/ThalesMMS/dicom-go/internal/privatefs"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

const avcCompressionMethod = "ISO_14496_10"

var (
	tagAVCFrameIncrement = core.NewTag(0x0028, 0x0009)
	tagAVCPixelAspect    = core.NewTag(0x0028, 0x0034)
	tagAVCLossy          = core.NewTag(0x0028, 0x2110)
	tagAVCLossyRatio     = core.NewTag(0x0028, 0x2112)
	tagAVCLossyMethod    = core.NewTag(0x0028, 0x2114)
	tagAVCImageType      = core.NewTag(0x0008, 0x0008)
	tagAVCExtendedOffset = core.NewTag(0x7fe0, 0x0001)
	tagAVCExtendedLength = core.NewTag(0x7fe0, 0x0002)
	tagAVCValueLength    = core.NewTag(0x7fe0, 0x0003)
)

// AVCTranscodeOptions explicitly authorizes a new lossy encode in .102.
// CRF is mandatory. Zero MaxOutputBytes uses the normalized input-byte cap.
type AVCTranscodeOptions struct {
	Limits         AVCCodecLimits
	AllowLossy     bool
	CRF            int
	MaxOutputBytes int64
}

type AVCTranscodeReport struct {
	Profile              AVCProfile
	SOPInstanceUID       string
	SourceSOPInstanceUID string
	ContainerBytes       int64
	CompressionRatio     float64
}

func (o AVCTranscodeOptions) normalized() (AVCTranscodeOptions, error) {
	if !o.AllowLossy || o.CRF < 1 || o.CRF > 51 {
		return o, ErrLossyPolicy
	}
	if o.MaxOutputBytes < 0 || o.MaxOutputBytes > 1<<30 {
		return o, ErrMediaTooLarge
	}
	limits, err := o.Limits.normalized()
	o.Limits = limits
	if o.MaxOutputBytes == 0 {
		o.MaxOutputBytes = limits.MaxBytes
	}
	return o, err
}

// TranscodeAVCMP4 constructs a detached DICOM instance with the same Video VL
// SOP Class and presentation geometry. The decoder verifies both the source
// and the encoder output. Nothing is returned on cancellation or mismatch.
func TranscodeAVCMP4(ctx context.Context, source *object.File, decoder AVCDecoder, encoder AVCEncoder, options AVCTranscodeOptions) (*object.File, AVCTranscodeReport, error) {
	options, err := options.normalized()
	if err != nil {
		return nil, AVCTranscodeReport{}, err
	}
	ctx, cancel := avcContext(ctx, options.Limits.MaxDuration)
	defer cancel()
	return transcodeAVCMP4(ctx, source, decoder, encoder, options)
}

func transcodeAVCMP4(ctx context.Context, source *object.File, decoder AVCDecoder, encoder AVCEncoder, options AVCTranscodeOptions) (*object.File, AVCTranscodeReport, error) {
	if decoder == nil || encoder == nil {
		return nil, AVCTranscodeReport{}, ErrCodecBackend
	}
	input, profile, err := readDICOMAVC(ctx, source, options.Limits.ImportLimits)
	if err != nil {
		return nil, AVCTranscodeReport{}, err
	}
	predecessor, err := derivation.SourceOf(source.Dataset, &derivation.SourceImageForProcessing)
	if err != nil {
		return nil, AVCTranscodeReport{}, err
	}
	output, err := detachedAVCSource(ctx, source, options.Limits.MaxBytes)
	if err != nil {
		return nil, AVCTranscodeReport{}, err
	}
	var decoded, encoded bytes.Buffer
	if err := decodeAVC(ctx, input, profile, decoder, &decoded); err != nil {
		return nil, AVCTranscodeReport{}, err
	}
	writer := boundedAVCWriter{ctx: ctx, dst: &encoded, remaining: options.MaxOutputBytes}
	err = encoder.EncodeAVCMP4(ctx, decoded.Bytes(), profile, AVCEncodeOptions{
		CRF: options.CRF, MaxBytes: options.MaxOutputBytes,
	}, &writer)
	if ctx.Err() != nil {
		return nil, AVCTranscodeReport{}, ctx.Err()
	}
	if writer.err != nil {
		return nil, AVCTranscodeReport{}, writer.err
	}
	if err != nil {
		return nil, AVCTranscodeReport{}, fmt.Errorf("%w: %w", ErrCodecBackend, err)
	}
	outputLimits := options.Limits.ImportLimits
	outputLimits.MaxBytes = options.MaxOutputBytes
	encodedProfile, err := inspectAVC(ctx, encoded.Bytes(), outputLimits)
	if err != nil {
		return nil, AVCTranscodeReport{}, err
	}
	if encodedProfile != profile {
		return nil, AVCTranscodeReport{}, fmt.Errorf("%w: encoder changed geometry, timing or colour", ErrCodecProfile)
	}
	if err := decodeAVC(ctx, encoded.Bytes(), profile, decoder, io.Discard); err != nil {
		return nil, AVCTranscodeReport{}, err
	}
	report := AVCTranscodeReport{
		Profile: profile, SourceSOPInstanceUID: predecessor.SOPInstanceUID,
		ContainerBytes: int64(encoded.Len()), CompressionRatio: float64(decoded.Len()) / float64(encoded.Len()),
	}
	if err := finishAVCTranscode(output, predecessor, encoded.Bytes(), &report); err != nil {
		return nil, AVCTranscodeReport{}, err
	}
	if err := ctx.Err(); err != nil {
		return nil, AVCTranscodeReport{}, err
	}
	return output, report, nil
}

func verifyDICOMAVCMetadata(dataset *object.Object, profile AVCProfile) error {
	class, _ := dataset.GetString(tags.SOPClassUID)
	switch class {
	case "1.2.840.10008.5.1.4.1.1.77.1.1.1", // Video Endoscopic
		"1.2.840.10008.5.1.4.1.1.77.1.2.1", // Video Microscopic
		"1.2.840.10008.5.1.4.1.1.77.1.4.1": // Video Photographic
	default:
		return fmt.Errorf("%w: expected a Video VL SOP Class", ErrCodecProfile)
	}
	for _, expected := range []struct {
		tag   core.Tag
		value int64
	}{
		{tags.Rows, int64(profile.Rows)}, {tags.Columns, int64(profile.Columns)},
		{tags.NumberOfFrames, int64(profile.Frames)}, {tags.SamplesPerPixel, 3},
		{tags.BitsAllocated, 8}, {tags.BitsStored, 8}, {tags.HighBit, 7},
		{tags.PixelRepresentation, 0}, {tags.PlanarConfiguration, 0},
	} {
		got, err := dataset.GetInt(expected.tag)
		if err != nil || got != expected.value {
			return fmt.Errorf("%w: inconsistent %s", ErrCodecProfile, expected.tag)
		}
	}
	photometric, _ := dataset.GetString(tags.PhotometricInterpretation)
	if photometric != "YBR_PARTIAL_420" || dataset.Has(tagAVCPixelAspect) || dataset.Has(tags.FrameTimeVector) {
		return fmt.Errorf("%w: unsupported photometric interpretation, pixel aspect or timing vector", ErrCodecProfile)
	}
	times, err := dataset.GetFloats(tags.FrameTime)
	ptr, ok := dataset.Get(tagAVCFrameIncrement)
	if err != nil || len(times) != 1 || !(math.Abs(times[0]-profile.frameTime()) <= 0.01) ||
		!ok || !isFrameTimePointer(ptr.Value, dataset) {
		return fmt.Errorf("%w: inconsistent constant Frame Time", ErrCodecProfile)
	}
	for _, tag := range []core.Tag{tags.CineRate, core.NewTag(0x0008, 0x2144)} {
		if dataset.Has(tag) {
			rate, err := dataset.GetFloat(tag)
			if err != nil || math.Abs(rate-float64(profile.RateNumerator)/float64(profile.RateDenominator)) > 0.5 {
				return fmt.Errorf("%w: inconsistent display rate", ErrCodecProfile)
			}
		}
	}
	return nil
}

func isFrameTimePointer(value core.Value, dataset *object.Object) bool {
	switch tagsValue := value.(type) {
	case core.TagValue:
		return len(tagsValue) == 1 && tagsValue[0] == tags.FrameTime
	case core.RawValue:
		return len(tagsValue) == 4 &&
			dataset.ValueByteOrder().Uint16(tagsValue) == tags.FrameTime.Group &&
			dataset.ValueByteOrder().Uint16(tagsValue[2:]) == tags.FrameTime.Element
	default:
		return false
	}
}

const avcMetadataBytes = 16 << 20

func detachedAVCSource(ctx context.Context, source *object.File, maxInput int64) (*object.File, error) {
	err := source.Dataset.WalkContext(ctx, object.WalkOptions{MaxDepth: 32, MaxElements: 100_000},
		func(path []core.Tag, element core.Element) error {
			tag := element.Tag()
			switch tag {
			case core.NewTag(0xfffa, 0xfffa), core.NewTag(0x4ffe, 0x0001),
				core.NewTag(0x0400, 0x0500), core.NewTag(0x5200, 0x9229), core.NewTag(0x5200, 0x9230):
				return fmt.Errorf("%w: signed, encrypted or functional-group video", ErrCodecProfile)
			}
			if len(path) > 1 && tag == core.TagPixelData {
				return fmt.Errorf("%w: nested Pixel Data", ErrCodecProfile)
			}
			return nil
		})
	if err != nil {
		return nil, err
	}
	return object.CloneFileWithOptions(source, object.CloneFileOptions{
		MaxElementBytes: avcMetadataBytes, MaxPixelDataBytes: maxInput, MaxTotalBytes: maxInput + avcMetadataBytes,
		MaxSequenceDepth: 32, MaxElements: 100_000, MaxFragments: 1,
	})
}

func finishAVCTranscode(output *object.File, predecessor derivation.Source, encoded []byte, report *AVCTranscodeReport) error {
	result := output.Dataset
	uid, err := core.NewUID()
	if err != nil {
		return err
	}
	report.SOPInstanceUID = uid
	imageType, _ := result.GetStrings(tagAVCImageType)
	if len(imageType) < 2 {
		imageType = []string{"DERIVED", "SECONDARY"}
	} else {
		imageType = slices.Clone(imageType)
		imageType[0], imageType[1] = "DERIVED", "SECONDARY"
	}
	for _, attr := range []struct {
		tag    core.Tag
		vr     core.VR
		values []string
	}{
		{tags.SOPInstanceUID, core.VRUI, []string{uid}},
		{tagAVCImageType, core.VRCS, imageType},
		{tagAVCLossy, core.VRCS, []string{"01"}},
	} {
		if err := result.SetStrings(attr.tag, attr.vr, attr.values...); err != nil {
			return err
		}
	}
	if err := appendAVCLossHistory(result, report.CompressionRatio); err != nil {
		return err
	}
	if err := derivation.ApplyTo(result, derivation.Derivation{
		Description: derivation.LossyDescription(avcCompressionMethod, report.CompressionRatio),
		Codes:       []derivation.Code{derivation.LossyCompression},
		Sources:     []derivation.Source{predecessor},
	}); err != nil {
		return err
	}
	for _, tag := range []core.Tag{tagAVCExtendedOffset, tagAVCExtendedLength, tagAVCValueLength} {
		result.Remove(tag)
	}
	if len(encoded)&1 != 0 {
		encoded = append(encoded, 0)
	}
	result.Put(core.Element{
		Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB, Length: core.UndefinedLength, LengthSet: true},
		Value:  core.FragmentSequence{Fragments: [][]byte{encoded}},
	})
	output.TransferSyntax = transfer.MPEG4HP41
	return output.RebuildFileMeta()
}

func appendAVCLossHistory(result *object.Object, ratio float64) error {
	methods, _ := result.GetStrings(tagAVCLossyMethod)
	ratios, _ := result.GetStrings(tagAVCLossyRatio)
	if len(methods) == 1 && strings.TrimSpace(methods[0]) == "" {
		methods = nil
	}
	if len(ratios) == 1 && strings.TrimSpace(ratios[0]) == "" {
		ratios = nil
	}
	if len(methods) == 0 {
		// The predecessor is already AVC. Its compression ratio was not
		// measured against its original acquisition and remains unspecified.
		methods = []string{avcCompressionMethod}
	}
	for _, method := range methods {
		if strings.TrimSpace(method) == "" {
			return fmt.Errorf("%w: empty lossy compression method", ErrCodecProfile)
		}
	}
	if len(ratios) != 0 {
		if len(ratios) != len(methods) {
			return fmt.Errorf("%w: unaligned lossy compression history", ErrCodecProfile)
		}
		values, err := result.GetFloats(tagAVCLossyRatio)
		if err != nil || len(values) != len(ratios) {
			return fmt.Errorf("%w: invalid lossy compression ratios", ErrCodecProfile)
		}
		for _, value := range values {
			if !(value > 0) || math.IsInf(value, 0) {
				return fmt.Errorf("%w: nonpositive lossy compression ratio", ErrCodecProfile)
			}
		}
	}
	if len(ratios) != 0 {
		current, err := object.FormatDS(ratio)
		if err != nil {
			return err
		}
		if err := result.SetStrings(tagAVCLossyRatio, core.VRDS, append(slices.Clone(ratios), current)...); err != nil {
			return err
		}
	} else {
		// Avoid asserting a missing ratio for any predecessor generation.
		result.Remove(tagAVCLossyRatio)
	}
	return result.SetStrings(tagAVCLossyMethod, core.VRCS, append(slices.Clone(methods), avcCompressionMethod)...)
}

// TranscodeAVCMP4Path reads one regular Part 10 file and atomically replaces a
// separate destination only after encode, verification and serialization.
func TranscodeAVCMP4Path(ctx context.Context, sourcePath, targetPath string, decoder AVCDecoder, encoder AVCEncoder, options AVCTranscodeOptions) (AVCTranscodeReport, error) {
	options, err := options.normalized()
	if err != nil {
		return AVCTranscodeReport{}, err
	}
	ctx, cancel := avcContext(ctx, options.Limits.MaxDuration)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return AVCTranscodeReport{}, err
	}
	input, err := os.Open(sourcePath)
	if err != nil {
		return AVCTranscodeReport{}, err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return AVCTranscodeReport{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > options.Limits.MaxBytes+avcMetadataBytes {
		return AVCTranscodeReport{}, ErrMediaTooLarge
	}
	if err := separateAVCTarget(targetPath, info); err != nil {
		return AVCTranscodeReport{}, err
	}
	source, err := object.ReadFileWithOptions(avcReader{ctx: ctx, source: input}, object.ReadFileOptions{
		MaxTotalBytes: options.Limits.MaxBytes + avcMetadataBytes, MaxElementBytes: avcMetadataBytes,
		MaxPixelDataBytes: options.Limits.MaxBytes, MaxFragments: 1, MaxSequenceDepth: 32, MaxElements: 100_000,
	})
	if err != nil {
		if ctx.Err() != nil {
			return AVCTranscodeReport{}, ctx.Err()
		}
		return AVCTranscodeReport{}, err
	}
	defer source.Close()
	output, report, err := transcodeAVCMP4(ctx, source, decoder, encoder, options)
	if err != nil {
		return AVCTranscodeReport{}, err
	}
	if err := publishAVC(ctx, targetPath, output, info, options.MaxOutputBytes+avcMetadataBytes); err != nil {
		return AVCTranscodeReport{}, err
	}
	return report, nil
}

func separateAVCTarget(target string, source os.FileInfo) error {
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || os.SameFile(source, info) {
		return fmt.Errorf("%w: target must be a separate regular file", ErrCodecProfile)
	}
	return nil
}

func publishAVC(ctx context.Context, target string, output *object.File, source os.FileInfo, limit int64) error {
	temp, err := os.CreateTemp(filepath.Dir(target), ".dicom-video-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	defer temp.Close()
	if err := privatefs.Secure(temp.Name()); err != nil {
		return err
	}
	if err := object.WriteFile(&boundedAVCWriter{ctx: ctx, dst: temp, remaining: limit}, output); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := separateAVCTarget(target, source); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return privatefs.Replace(temp.Name(), target)
}

type avcReader struct {
	ctx    context.Context
	source io.Reader
}

func (r avcReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.source.Read(p)
}
