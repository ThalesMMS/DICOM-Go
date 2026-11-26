package encapimage

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/validation"
	"github.com/ThalesMMS/dicom-go/validation/iod"
	"github.com/ThalesMMS/dicom-go/video"
)

const (
	MultiFrameGrayscaleByteSC     = "1.2.840.10008.5.1.4.1.1.7.2"
	MultiFrameGrayscaleWordSC     = "1.2.840.10008.5.1.4.1.1.7.3"
	MultiFrameTrueColorSC         = "1.2.840.10008.5.1.4.1.1.7.4"
	VideoPhotographicImageStorage = "1.2.840.10008.5.1.4.1.1.77.1.4.1"
)

type Format string

const (
	Auto  Format = "auto"
	PNG   Format = "png"
	BMP   Format = "bmp"
	PNM   Format = "pnm"
	PGX   Format = "pgx"
	Raw   Format = "raw"
	JPEG  Format = "jpeg"
	J2K   Format = "j2k"
	JLS   Format = "jls"
	Video Format = "video"
)

var (
	ErrImportLimit      = errors.New("encapimage: image import resource limit")
	ErrUnsupportedImage = errors.New("encapimage: unsupported image input or layout")
	ErrMalformedImage   = errors.New("encapimage: malformed image input")
)

// ImportLimits bounds input, frame count, pixel dimensions and admitted native
// spans. It is not an RSS limit. Zero uses safe defaults; negative fields refuse.
type ImportLimits struct {
	MaxInputBytes   int64
	MaxDecodedBytes int64
	MaxPixels       int64
	MaxFrames       int
}

func (l ImportLimits) normalized() (ImportLimits, error) {
	if l.MaxInputBytes < 0 || l.MaxInputBytes > 1<<30 || l.MaxDecodedBytes < 0 || l.MaxPixels < 0 || l.MaxFrames < 0 || l.MaxFrames > 100000 {
		return l, ErrInvalidOption
	}
	if l.MaxInputBytes == 0 {
		l.MaxInputBytes = 256 << 20
	}
	if l.MaxDecodedBytes == 0 {
		l.MaxDecodedBytes = 512 << 20
	}
	if l.MaxPixels == 0 {
		l.MaxPixels = 16 << 20
	}
	if l.MaxFrames == 0 {
		l.MaxFrames = 1024
	}
	return l, nil
}

// ImageOptions imports one or more identical-layout frames. JPEGOptions shares
// identity/common metadata with FromJPEG. Empty SOPClassUID chooses SC for one
// frame and the compatible Multi-frame SC class for several frames. Explicit
// VLPhotographic takes one unsigned 8-bit frame. Video selects VideoPhotographic.
// AnatomicRegion identifies the caller-supplied imaged anatomy. No anatomy is inferred.
type AnatomicRegion struct{ CodeValue, CodingSchemeDesignator, CodeMeaning string }

type ImageOptions struct {
	// AnatomicRegion is required for video, and optional for VL Photographic.
	AnatomicRegion *AnatomicRegion
	JPEGOptions
	Format Format
	Limits ImportLimits
	Raw    RawOptions
	// SeriesSource supplies Patient/Study/Series metadata, never the new SOP UID.
	// The caller keeps the source open and immutable during this call. Explicit
	// option fields win; a conflicting Study UID with inherited Series UID refuses.
	SeriesSource *object.Object
	// J2KLossless is the caller's declaration that the full source was losslessly
	// encoded. COD/QCD reversible/no-quantization constraints are also checked,
	// but headers alone cannot prove absence of quality-layer truncation. No
	// decode/original comparison or generic entropy acceptance is implied.
	J2KLossless bool
}

type ImportReport struct {
	Report
	Format                            Format
	Rows, Columns, Frames, BitsStored int
	Encapsulated                      bool
	Decoded                           bool
	LosslessDeclared                  bool
	VideoContainer                    video.Container
}

type sourceFrame struct {
	imageLayout
	data   []byte
	syntax transfer.Syntax
	icc    []byte
	report Report
	format Format
}

// FromImages builds a new image Part 10 model. Compressed inputs are framed
// and inspected without recoding; video retains the original container. Native
// formats are decoded into canonical interleaved little-endian stored samples.
// Inputs are not mutated. No file is published on failure.
func FromImages(ctx context.Context, inputs [][]byte, opts ImageOptions) (*object.File, ImportReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, ImportReport{}, err
	}
	limits, err := opts.Limits.normalized()
	if err != nil {
		return nil, ImportReport{}, err
	}
	if len(inputs) == 0 {
		return nil, ImportReport{}, ErrInvalidOption
	}
	if len(inputs) > limits.MaxFrames {
		return nil, ImportReport{}, ErrImportLimit
	}
	if len(inputs) > 1 && pageVectorBytes(len(inputs)) > 65534 {
		return nil, ImportReport{}, ErrImportLimit
	}
	var total int64
	for _, b := range inputs {
		if int64(len(b)) > limits.MaxInputBytes-total {
			return nil, ImportReport{}, ErrImportLimit
		}
		total += int64(len(b))
	}
	if !validImportText(opts.JPEGOptions) {
		return nil, ImportReport{}, ErrInvalidOption
	}
	if opts.AnatomicRegion != nil {
		c := opts.AnatomicRegion
		if !validRegion(c) {
			return nil, ImportReport{}, ErrInvalidOption
		}
	}
	if err = inheritSeries(ctx, &opts); err != nil {
		return nil, ImportReport{}, err
	}
	for _, uid := range []string{opts.SOPClassUID, opts.SOPInstanceUID, opts.StudyInstanceUID, opts.SeriesInstanceUID} {
		if uid != "" && !core.IsValidUID(uid) {
			return nil, ImportReport{}, ErrInvalidOption
		}
	}
	if opts.BurnedInAnnotation != "" && opts.BurnedInAnnotation != "YES" && opts.BurnedInAnnotation != "NO" {
		return nil, ImportReport{}, ErrInvalidOption
	}
	if opts.Laterality != "" && opts.Laterality != "R" && opts.Laterality != "L" {
		return nil, ImportReport{}, ErrInvalidOption
	}
	if opts.BurnedInAnnotation == "" {
		opts.BurnedInAnnotation = "YES"
	}
	if opts.Common.Source != nil && opts.SOPInstanceUID != "" {
		sourceUID, _ := opts.Common.Source.GetString(tagSOPInstanceUID)
		if sourceUID == opts.SOPInstanceUID {
			return nil, ImportReport{}, ErrInvalidOption
		}
	}
	format := opts.Format
	if format == "" {
		format = Auto
	}
	if format != Raw && opts.Raw != (RawOptions{}) {
		return nil, ImportReport{}, ErrInvalidOption
	}
	if format == Raw && (opts.J2KLossless || opts.KeepAPPn || opts.KeepComments || opts.DiscardICCProfile || opts.AllowRetiredProgressive) {
		return nil, ImportReport{}, ErrInvalidOption
	}
	if format == Video || format == Auto && len(inputs) == 1 && isVideo(inputs[0]) {
		return importVideo(ctx, inputs, opts, limits)
	}
	var frames []sourceFrame
	var decoded int64
	for _, b := range inputs {
		if err := ctx.Err(); err != nil {
			return nil, ImportReport{}, err
		}
		remaining := limits
		remaining.MaxDecodedBytes -= decoded
		remaining.MaxFrames -= len(frames)
		if remaining.MaxFrames <= 0 {
			return nil, ImportReport{}, ErrImportLimit
		}
		var next []sourceFrame
		if format == Raw {
			next, err = readRaw(ctx, b, opts.Raw, remaining)
		} else {
			var f sourceFrame
			f, err = readImage(ctx, b, format, opts, remaining)
			next = []sourceFrame{f}
		}
		if err != nil {
			if e := ctx.Err(); e != nil {
				return nil, ImportReport{}, e
			}
			return nil, ImportReport{}, err
		}
		if len(next) > limits.MaxFrames-len(frames) {
			return nil, ImportReport{}, ErrImportLimit
		}
		for _, f := range next {
			n, e := admitImage(f.imageLayout, limits)
			if e != nil {
				return nil, ImportReport{}, e
			}
			if n > limits.MaxDecodedBytes-decoded {
				return nil, ImportReport{}, ErrImportLimit
			}
			if !f.syntax.Encapsulated {
				if _, e := nativePixelDataLength(decoded + n); e != nil {
					return nil, ImportReport{}, e
				}
			}
			decoded += n
			if len(frames) > 0 && !sameLayout(frames[0], f) {
				return nil, ImportReport{}, fmt.Errorf("%w: frames differ", ErrUnsupportedImage)
			}
			frames = append(frames, f)
		}
	}
	if len(frames) == 0 {
		return nil, ImportReport{}, ErrMalformedImage
	}
	first := frames[0]
	class := opts.SOPClassUID
	if class == "" {
		class = SecondaryCaptureImageStorage
		if len(frames) > 1 {
			class, err = multiClass(first.imageLayout)
			if err != nil {
				return nil, ImportReport{}, err
			}
		}
	}
	if err = checkImageClass(class, first, len(frames)); err != nil {
		return nil, ImportReport{}, err
	}
	if class != SecondaryCaptureImageStorage && class != VLPhotographicImageStorage && pageVectorBytes(len(frames)) > 65534 {
		return nil, ImportReport{}, ErrImportLimit
	}
	opts.SOPClassUID = class
	var value core.Value
	if first.syntax.Encapsulated {
		payloads := make([][]byte, len(frames))
		for i, f := range frames {
			payloads[i] = make([]byte, len(f.data)+(len(f.data)&1))
			copy(payloads[i], f.data)
		}
		value = core.FragmentSequence{Fragments: payloads}
	} else {
		nativeLength, e := nativePixelDataLength(decoded)
		if e != nil {
			return nil, ImportReport{}, e
		}
		data := make([]byte, nativeLength)
		pos := 0
		for _, f := range frames {
			if err := ctx.Err(); err != nil {
				return nil, ImportReport{}, err
			}
			pos += copy(data[pos:], f.data)
		}
		value = core.RawValue(data)
	}
	rep := first.report
	rep.RemovedSegments = nil
	rep.TrailingBytesRemoved = 0
	for _, f := range frames {
		rep.TrailingBytesRemoved += f.report.TrailingBytesRemoved
		for _, removed := range f.report.RemovedSegments {
			merged := false
			for i := range rep.RemovedSegments {
				p := &rep.RemovedSegments[i]
				if p.Marker == removed.Marker && p.Identifier == removed.Identifier {
					p.Count += removed.Count
					p.Bytes += removed.Bytes
					merged = true
					break
				}
			}
			if !merged {
				rep.RemovedSegments = append(rep.RemovedSegments, removed)
			}
		}
	}
	rep.TransferSyntaxUID = first.syntax.UID
	rep.PhotometricInterpretation = first.photometric
	rep.Process = first.process
	rep.PixelDataBytes = 0
	rep.Padded = false
	for _, f := range frames {
		rep.PixelDataBytes += len(f.data) + (len(f.data) & 1)
		rep.Padded = rep.Padded || len(f.data)&1 != 0
	}
	if !first.syntax.Encapsulated {
		rep.PixelDataBytes = int(decoded) + (int(decoded) & 1)
		rep.Padded = decoded&1 != 0
	}
	first.frameCount = len(frames)
	first.payloadBytes = 0
	for _, f := range frames {
		first.payloadBytes += len(f.data)
	}
	file, rep, err := buildImage(opts.JPEGOptions, first.imageLayout, value, first.syntax, rep, first.icc)
	if err != nil {
		return nil, ImportReport{}, err
	}
	if opts.AnatomicRegion != nil {
		addRegion(file.Dataset, *opts.AnatomicRegion)
	}
	if class != SecondaryCaptureImageStorage && class != VLPhotographicImageStorage {
		addMultiframe(file.Dataset, len(frames))
	}
	if err := ctx.Err(); err != nil {
		return nil, ImportReport{}, err
	}
	if err := checkImported(ctx, file); err != nil {
		return nil, ImportReport{}, err
	}
	return file, ImportReport{Report: rep, Format: first.format, Rows: first.rows, Columns: first.columns, Frames: len(frames), BitsStored: first.bits, Encapsulated: first.syntax.Encapsulated, Decoded: !first.syntax.Encapsulated, LosslessDeclared: first.format == J2K && opts.J2KLossless}, nil
}

func admitImage(l imageLayout, limits ImportLimits) (int64, error) {
	if l.rows <= 0 || l.columns <= 0 || l.rows > 65535 || l.columns > 65535 || l.bits < 1 || l.bits > 16 || (l.samples != 1 && l.samples != 3) || l.signed < 0 || l.signed > 1 {
		return 0, ErrUnsupportedImage
	}
	if l.samples == 3 && l.signed != 0 {
		return 0, ErrUnsupportedImage
	}
	pixels := int64(l.rows) * int64(l.columns)
	if pixels > limits.MaxPixels {
		return 0, ErrImportLimit
	}
	allocated := l.allocated
	if allocated == 0 {
		allocated = 8
		if l.bits > 8 {
			allocated = 16
		}
	}
	if allocated != 8 && allocated != 16 || l.bits > allocated {
		return 0, ErrUnsupportedImage
	}
	n := pixels * int64(l.samples) * int64(allocated/8)
	if n > limits.MaxDecodedBytes || n > int64(math.MaxInt)-1 || n > 4294967294 {
		return 0, ErrImportLimit
	}
	return n, nil
}
func sameLayout(a, b sourceFrame) bool {
	return a.rows == b.rows && a.columns == b.columns && a.samples == b.samples && a.bits == b.bits && a.signed == b.signed && effectiveAllocated(a.imageLayout) == effectiveAllocated(b.imageLayout) && a.photometric == b.photometric && a.lossless == b.lossless && a.lossyUnknown == b.lossyUnknown && a.method == b.method && bytes.Equal(a.icc, b.icc) && a.syntax.UID == b.syntax.UID
}
func effectiveAllocated(l imageLayout) int {
	if l.allocated > 0 {
		return l.allocated
	}
	if l.bits > 8 {
		return 16
	}
	return 8
}
func multiClass(l imageLayout) (string, error) {
	if l.signed != 0 {
		return "", ErrUnsupportedSOPClass
	}
	if l.samples == 1 && l.bits == 8 && effectiveAllocated(l) == 8 {
		return MultiFrameGrayscaleByteSC, nil
	}
	if l.samples == 1 && l.bits > 8 && effectiveAllocated(l) == 16 {
		return MultiFrameGrayscaleWordSC, nil
	}
	if l.samples == 3 && l.bits == 8 && effectiveAllocated(l) == 8 && (l.photometric == "RGB" || l.photometric == "YBR_FULL_422" || l.photometric == "YBR_RCT" || l.photometric == "YBR_ICT") {
		return MultiFrameTrueColorSC, nil
	}
	return "", ErrUnsupportedSOPClass
}
func checkImageClass(class string, f sourceFrame, n int) error {
	switch class {
	case SecondaryCaptureImageStorage:
		if n != 1 {
			return ErrUnsupportedSOPClass
		}
	case VLPhotographicImageStorage:
		if n != 1 || f.bits != 8 || effectiveAllocated(f.imageLayout) != 8 || f.signed != 0 {
			return ErrUnsupportedSOPClass
		}
		if f.samples == 3 && !f.lossless && f.photometric == "RGB" {
			return ErrUnsupportedComponents
		}
	case MultiFrameGrayscaleByteSC, MultiFrameGrayscaleWordSC, MultiFrameTrueColorSC:
		c, e := multiClass(f.imageLayout)
		if e != nil || c != class {
			return ErrUnsupportedSOPClass
		}
	default:
		return ErrUnsupportedSOPClass
	}
	return nil
}
func addMultiframe(ds *object.Object, n int) {
	ds.Put(stringElement(core.NewTag(0x0028, 0x0008), core.VRIS, strconv.Itoa(n)))
	ds.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0009), VR: core.VRAT}, Value: core.TagValue{core.NewTag(0x0018, 0x2001)}})
	pages := make(core.StringValue, n)
	for i := range pages {
		pages[i] = strconv.Itoa(i + 1)
	}
	ds.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0018, 0x2001), VR: core.VRIS}, Value: pages})
	if !ds.Has(tagBurnedInAnnotation) {
		ds.Put(stringElement(tagBurnedInAnnotation, core.VRCS, "YES"))
	}
	if func() bool { uid, _ := ds.GetString(tagSOPClassUID); return uid == MultiFrameGrayscaleWordSC }() {
		ds.Put(stringElement(core.NewTag(0x0028, 0x1052), core.VRDS, "0"))
		ds.Put(stringElement(core.NewTag(0x0028, 0x1053), core.VRDS, "1"))
		ds.Put(stringElement(core.NewTag(0x0028, 0x1054), core.VRLO, "US"))
	}
	if value, _ := ds.GetString(tagPhotometric); value == "MONOCHROME2" {
		ds.Put(stringElement(core.NewTag(0x2050, 0x0020), core.VRCS, "IDENTITY"))
	}
}
func checkIOD(ds *object.Object) error {
	report, err := iod.Validate(ds, iod.Options{CheckValues: true})
	if err != nil {
		return err
	}
	if errors := report.Errors(); len(errors) > 0 {
		return fmt.Errorf("%w: output IOD %s", ErrInvalidOption, errors[0].String())
	}
	return nil
}
func inheritSeries(ctx context.Context, opts *ImageOptions) error {
	if opts.SeriesSource == nil {
		return nil
	}
	ds, _, err := opts.SeriesSource.ConvertTextToUTF8(ctx, object.CharsetConversionOptions{})
	if err != nil {
		return err
	}
	study, _ := ds.GetString(tagStudyInstanceUID)
	series, _ := ds.GetString(tagSeriesInstanceUID)
	if !core.IsValidUID(study) || !core.IsValidUID(series) {
		return ErrInvalidOption
	}
	if opts.StudyInstanceUID != "" && opts.StudyInstanceUID != study && opts.SeriesInstanceUID == "" {
		return ErrInvalidOption
	}
	if opts.Common.Source != nil && opts.Common.Source != opts.SeriesSource {
		return ErrInvalidOption
	}
	opts.Common.Source = ds
	fields := []struct {
		tag core.Tag
		dst *string
	}{{tagSeriesInstanceUID, &opts.SeriesInstanceUID}, {tagStudyInstanceUID, &opts.StudyInstanceUID}, {tagSeriesNumber, &opts.SeriesNumber}, {tagSeriesDescription, &opts.SeriesDescription}, {tagModality, &opts.Modality}}
	for _, f := range fields {
		if *f.dst == "" {
			*f.dst, _ = ds.GetString(f.tag)
		}
	}
	return nil
}
func detectImage(b []byte) Format {
	switch {
	case bytes.HasPrefix(b, []byte{137, 'P', 'N', 'G', 13, 10, 26, 10}):
		return PNG
	case bytes.HasPrefix(b, []byte("BM")):
		return BMP
	case bytes.HasPrefix(b, []byte("PG ")):
		return PGX
	case len(b) >= 2 && b[0] == 'P' && (b[1] == '2' || b[1] == '3' || b[1] == '5' || b[1] == '6'):
		return PNM
	case len(b) >= 2 && b[0] == 255 && b[1] == 0x4f:
		return J2K
	case len(b) >= 2 && b[0] == 255 && b[1] == 0xd8:
		return detectJPEGFamily(b)
	}
	return Auto
}
func readImage(ctx context.Context, b []byte, format Format, opts ImageOptions, limits ImportLimits) (sourceFrame, error) {
	if format == Auto {
		format = detectImage(b)
	}
	if format != JPEG && (opts.KeepAPPn || opts.KeepComments || opts.DiscardICCProfile || opts.AllowRetiredProgressive) {
		return sourceFrame{}, ErrInvalidOption
	}
	if opts.J2KLossless && format != J2K {
		return sourceFrame{}, ErrInvalidOption
	}
	switch format {
	case PNG, BMP:
		return readRaster(ctx, b, format, limits)
	case PNM, PGX:
		return readPortable(ctx, b, format, limits)
	case JPEG, J2K, JLS:
		return readCompressed(ctx, b, format, opts, limits)
	}
	return sourceFrame{}, fmt.Errorf("%w: input format", ErrUnsupportedImage)
}
func isVideo(b []byte) bool {
	return len(b) >= 12 && string(b[4:8]) == "ftyp" || len(b) >= 4 && bytes.Equal(b[:4], []byte{0, 0, 1, 0xba})
}
func importVideo(ctx context.Context, inputs [][]byte, opts ImageOptions, limits ImportLimits) (*object.File, ImportReport, error) {
	if len(inputs) != 1 || opts.J2KLossless || opts.KeepAPPn || opts.KeepComments || opts.DiscardICCProfile || opts.AllowRetiredProgressive || opts.SOPClassUID != "" && opts.SOPClassUID != VideoPhotographicImageStorage {
		return nil, ImportReport{}, ErrInvalidOption
	}
	if opts.AnatomicRegion == nil {
		return nil, ImportReport{}, fmt.Errorf("%w: video requires anatomic region code", ErrInvalidOption)
	}
	info, err := video.InspectSource(ctx, inputs[0], video.ImportLimits{MaxBytes: limits.MaxInputBytes, MaxFrames: limits.MaxFrames, MaxPixels: limits.MaxPixels, MaxDecodedBytes: limits.MaxDecodedBytes})
	if err != nil {
		return nil, ImportReport{}, err
	}
	l := imageLayout{rows: info.Rows, columns: info.Columns, samples: 3, bits: 8, photometric: "YBR_PARTIAL_420", process: "video", lossless: false, payloadBytes: len(inputs[0]), frameCount: info.Frames, method: info.CompressionMethod}
	opts.SOPClassUID = VideoPhotographicImageStorage
	data := make([]byte, len(inputs[0])+(len(inputs[0])&1))
	copy(data, inputs[0])
	rep := Report{Process: "video", TransferSyntaxUID: info.TransferSyntax.UID, PhotometricInterpretation: l.photometric, PixelDataBytes: len(data), Padded: len(inputs[0])&1 != 0}
	file, rep, err := buildImage(opts.JPEGOptions, l, core.FragmentSequence{Fragments: [][]byte{data}}, info.TransferSyntax, rep, nil)
	if err != nil {
		return nil, ImportReport{}, err
	}
	ds := file.Dataset
	addRegion(ds, *opts.AnatomicRegion)
	ds.Put(stringElement(core.NewTag(0x0028, 0x0008), core.VRIS, strconv.Itoa(info.Frames)))
	ds.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0009), VR: core.VRAT}, Value: core.TagValue{core.NewTag(0x0018, 0x1063)}})
	ds.Put(stringElement(core.NewTag(0x0018, 0x1063), core.VRDS, strconv.FormatFloat(1000/info.FrameRate, 'g', 10, 64)))
	ds.Put(stringElement(tagLossyCompressionMethod, core.VRCS, info.CompressionMethod))
	if err = checkImported(ctx, file); err != nil {
		return nil, ImportReport{}, err
	}
	if err = ctx.Err(); err != nil {
		return nil, ImportReport{}, err
	}
	return file, ImportReport{Report: rep, Format: Video, Rows: l.rows, Columns: l.columns, Frames: info.Frames, BitsStored: 8, Encapsulated: true, VideoContainer: info.Container}, nil
}

func detectJPEGFamily(b []byte) Format {
	for at := 2; at+4 <= len(b) && at < 65536; {
		if b[at] != 255 {
			return JPEG
		}
		code := b[at+1]
		if code == 0xf7 {
			return JLS
		}
		if code == 0xda || code >= 0xc0 && code <= 0xcf {
			return JPEG
		}
		n := int(b[at+2])<<8 | int(b[at+3])
		if n < 2 || n > len(b)-at-2 {
			return JPEG
		}
		at += 2 + n
	}
	return JPEG
}

func validRegion(c *AnatomicRegion) bool {
	for i, v := range []string{c.CodeValue, c.CodingSchemeDesignator, c.CodeMeaning} {
		cap := 16
		if i == 2 {
			cap = 64
		}
		if strings.TrimSpace(v) == "" || len(v) > cap || !utf8.ValidString(v) || strings.ContainsAny(v, "\\\r\n\x00") {
			return false
		}
	}
	return true
}
func addRegion(ds *object.Object, c AnatomicRegion) {
	for _, v := range []string{c.CodeValue, c.CodingSchemeDesignator, c.CodeMeaning} {
		for _, r := range v {
			if r > 127 {
				ds.Put(stringElement(tagSpecificCharacterSet, core.VRCS, "ISO_IR 192"))
				break
			}
		}
	}
	elements := []core.Element{stringElement(core.NewTag(0x0008, 0x0100), core.VRSH, c.CodeValue), stringElement(core.NewTag(0x0008, 0x0102), core.VRSH, c.CodingSchemeDesignator), stringElement(core.NewTag(0x0008, 0x0104), core.VRLO, c.CodeMeaning)}
	ds.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x2218), VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{Elements: elements}}}})
}

// Syntax-check metadata separately so pixel/ICC payloads are not duplicated by
// the validation model. Their framing/byte spans were admitted by the readers.
func checkImported(ctx context.Context, f *object.File) error {
	ds := f.Dataset.ToDataSet()
	elements := make([]core.Element, 0, len(ds.Elements))
	for _, e := range ds.Elements {
		if e.Header.Tag != tagPixelData && e.Header.Tag != tagICCProfile {
			elements = append(elements, e)
		}
	}
	_, err := validation.ValidateDataSet(ctx, core.DataSet{Elements: elements}, validation.Options{Mode: validation.ModeStrict, Dictionary: std.Dictionary, ByteOrder: binary.LittleEndian, MaxDepth: 32, MaxElements: 100000, StopFirst: true})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: output metadata syntax", ErrInvalidOption)
	}
	return checkIOD(f.Dataset)
}

// IS uses a short explicit-VR length; page-number vectors cannot exceed it.
func pageVectorBytes(n int) int64 {
	if n <= 0 {
		return 0
	}
	total := int64(n - 1)
	first, digits := 1, 1
	for first <= n {
		last := first*10 - 1
		if last > n {
			last = n
		}
		total += int64(last-first+1) * int64(digits)
		first *= 10
		digits++
	}
	return total + (total & 1)
}

// Options above ImageType are single DICOM values. A delimiter embedded in a
// StringValue would otherwise serialize as a different VM when read back.
func validImportText(o JPEGOptions) bool {
	for _, v := range []string{o.SOPClassUID, o.SOPInstanceUID, o.StudyInstanceUID, o.SeriesInstanceUID, o.PatientName, o.PatientID, o.PatientBirthDate, o.PatientSex, o.StudyDate, o.StudyTime, o.StudyID, o.StudyDescription, o.AccessionNumber, o.ReferringPhysicianName, o.Modality, o.SeriesNumber, o.SeriesDescription, o.InstanceNumber, o.ContentDate, o.ContentTime, o.Manufacturer, o.Laterality, o.ConversionType, o.BurnedInAnnotation} {
		if !utf8.ValidString(v) || strings.ContainsAny(v, "\\\r\n\x00") {
			return false
		}
	}
	return utf8.ValidString(o.ImageType) && !strings.ContainsAny(o.ImageType, "\r\n\x00")
}

// nativePixelDataLength projects the padded native value before allocation.
func nativePixelDataLength(decoded int64) (int, error) {
	// Both ceilings are even, so adding the final odd-byte padding fits too.
	if decoded < 0 || decoded > 4294967294 || decoded > int64(math.MaxInt)-1 {
		return 0, ErrImportLimit
	}
	return int(decoded) + (int(decoded) & 1), nil
}
