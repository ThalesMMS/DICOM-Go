// Package encapimage builds DICOM image objects around an existing image
// bitstream without decoding or re-encoding it, as DCMTK img2dcm does.
//
// FromJPEG encapsulates a JPEG interchange-format file in a Secondary Capture
// or VL Photographic Image object. The frame header selects the transfer
// syntax; marker segments that can carry personal data (APPn other than the
// JFIF and Adobe headers, COM, and bytes after EOI) are removed unless the
// caller keeps them explicitly, and the report names what was removed by
// marker type and size, never by content.
package encapimage

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

const (
	SecondaryCaptureImageStorage = "1.2.840.10008.5.1.4.1.1.7"
	VLPhotographicImageStorage   = "1.2.840.10008.5.1.4.1.1.77.1.4"

	// JPEGProgressiveRetired is the retired JPEG Full Progression,
	// Non-Hierarchical (Process 10 & 12) transfer syntax. FromJPEG writes it
	// only with JPEGOptions.AllowRetiredProgressive.
	JPEGProgressiveRetired = "1.2.840.10008.1.2.4.55"
)

var (
	ErrNotJPEG                = errors.New("encapimage: input is not a JPEG interchange-format stream")
	ErrMalformedJPEG          = errors.New("encapimage: malformed JPEG stream")
	ErrTruncatedJPEG          = errors.New("encapimage: truncated JPEG stream")
	ErrUnsupportedJPEGProcess = errors.New("encapimage: JPEG process has no DICOM transfer syntax")
	ErrProgressiveJPEG        = errors.New("encapimage: progressive JPEG maps only to a retired transfer syntax")
	ErrUnsupportedComponents  = errors.New("encapimage: JPEG component layout is not supported")
	ErrUnsupportedSOPClass    = errors.New("encapimage: SOP class is not supported")
	ErrInvalidOption          = errors.New("encapimage: invalid option")
)

// JPEGOptions describes the object FromJPEG builds. Empty UIDs are generated
// (2.25 form); empty Type 2 attributes are written without a value.
type JPEGOptions struct {
	// SOPClassUID is SecondaryCaptureImageStorage (default) or
	// VLPhotographicImageStorage, which takes only 8-bit frames and, for
	// lossy color, YCbCr ones (see FromJPEG).
	SOPClassUID       string
	SOPInstanceUID    string
	StudyInstanceUID  string
	SeriesInstanceUID string

	PatientName      string
	PatientID        string
	PatientBirthDate string
	PatientSex       string

	StudyDate              string
	StudyTime              string
	StudyID                string
	StudyDescription       string
	AccessionNumber        string
	ReferringPhysicianName string

	// Modality defaults to OT for Secondary Capture and XC for VL
	// Photographic.
	Modality          string
	SeriesNumber      string // default "1"
	SeriesDescription string
	InstanceNumber    string // default "1"
	ContentDate       string
	ContentTime       string
	Manufacturer      string
	// Laterality is the General Series Laterality, R or L, of a paired body
	// part. Empty writes the Type 2C Attribute without a value (unknown): the
	// object records no Body Part Examined that could show it is not needed.
	Laterality string
	// ImageType is backslash-separated; VL Photographic defaults to
	// DERIVED\SECONDARY as img2dcm writes. Secondary Capture omits it when
	// empty.
	ImageType string
	// ConversionType is the Secondary Capture Conversion Type; default WSD.
	ConversionType string
	// BurnedInAnnotation is YES, NO or empty (omitted).
	BurnedInAnnotation string

	// KeepAPPn keeps every APPn segment. By default only the JFIF APP0 and
	// Adobe APP14 headers, which decoders need to interpret the color
	// components, are kept; Exif, XMP, ICC, maker and other application
	// segments can carry personal data and are removed.
	KeepAPPn bool
	// KeepComments keeps COM segments, removed by default.
	KeepComments bool
	// DiscardICCProfile drops an embedded ICC profile instead of copying it
	// to ICC Profile (0028,2000).
	DiscardICCProfile bool
	// AllowRetiredProgressive encapsulates a progressive (SOF2) stream with
	// the retired transfer syntax 1.2.840.10008.1.2.4.55 instead of failing.
	AllowRetiredProgressive bool
	// Common fills the Patient, Study and equipment modules from a source
	// instance of the same patient (object.InheritCommonModules); the
	// explicit fields above win over it.
	Common object.DerivedCommon
}

// RemovedSegment counts the removed marker segments of one kind.
type RemovedSegment struct {
	// Marker is APP0..APP15 or COM.
	Marker string
	// Identifier is a well-known application identifier (Exif, XMP,
	// ICC_PROFILE, JFXX, MPF, Photoshop, Ducky, Adobe) or empty; it is never
	// copied from the stream.
	Identifier string
	Count      int
	Bytes      int
}

// Report describes a FromJPEG conversion without repeating any of the
// removed content.
type Report struct {
	TransferSyntaxUID string
	// Process is baseline, extended, progressive or lossless.
	Process                   string
	PhotometricInterpretation string
	// Subsampling is 4:4:4, 4:2:2, 4:2:0 or other for three-component
	// streams, empty otherwise.
	Subsampling     string
	RemovedSegments []RemovedSegment
	// TrailingBytesRemoved counts bytes after EOI (for example appended
	// multi-picture images), which are always removed.
	TrailingBytesRemoved int
	// ICCProfileCopied reports that an embedded ICC profile was written to
	// ICC Profile (0028,2000).
	ICCProfileCopied bool
	// GeneratedUIDs names the UID attributes that were generated.
	GeneratedUIDs []string
	// Padded reports the zero byte added after EOI to reach an even length.
	Padded bool
	// PixelDataBytes is the length of the encapsulated fragment.
	PixelDataBytes int
}

var (
	tagImageType              = core.NewTag(0x0008, 0x0008)
	tagSpecificCharacterSet   = core.NewTag(0x0008, 0x0005)
	tagSOPClassUID            = core.NewTag(0x0008, 0x0016)
	tagSOPInstanceUID         = core.NewTag(0x0008, 0x0018)
	tagStudyDate              = core.NewTag(0x0008, 0x0020)
	tagContentDate            = core.NewTag(0x0008, 0x0023)
	tagStudyTime              = core.NewTag(0x0008, 0x0030)
	tagContentTime            = core.NewTag(0x0008, 0x0033)
	tagAccessionNumber        = core.NewTag(0x0008, 0x0050)
	tagModality               = core.NewTag(0x0008, 0x0060)
	tagConversionType         = core.NewTag(0x0008, 0x0064)
	tagManufacturer           = core.NewTag(0x0008, 0x0070)
	tagReferringPhysicianName = core.NewTag(0x0008, 0x0090)
	tagStudyDescription       = core.NewTag(0x0008, 0x1030)
	tagSeriesDescription      = core.NewTag(0x0008, 0x103E)
	tagPatientName            = core.NewTag(0x0010, 0x0010)
	tagPatientID              = core.NewTag(0x0010, 0x0020)
	tagPatientBirthDate       = core.NewTag(0x0010, 0x0030)
	tagPatientSex             = core.NewTag(0x0010, 0x0040)
	tagStudyInstanceUID       = core.NewTag(0x0020, 0x000D)
	tagSeriesInstanceUID      = core.NewTag(0x0020, 0x000E)
	tagStudyID                = core.NewTag(0x0020, 0x0010)
	tagSeriesNumber           = core.NewTag(0x0020, 0x0011)
	tagInstanceNumber         = core.NewTag(0x0020, 0x0013)
	tagPatientOrientation     = core.NewTag(0x0020, 0x0020)
	tagLaterality             = core.NewTag(0x0020, 0x0060)
	tagSamplesPerPixel        = core.NewTag(0x0028, 0x0002)
	tagPhotometric            = core.NewTag(0x0028, 0x0004)
	tagPlanarConfiguration    = core.NewTag(0x0028, 0x0006)
	tagRows                   = core.NewTag(0x0028, 0x0010)
	tagColumns                = core.NewTag(0x0028, 0x0011)
	tagBitsAllocated          = core.NewTag(0x0028, 0x0100)
	tagBitsStored             = core.NewTag(0x0028, 0x0101)
	tagHighBit                = core.NewTag(0x0028, 0x0102)
	tagPixelRepresentation    = core.NewTag(0x0028, 0x0103)
	tagBurnedInAnnotation     = core.NewTag(0x0028, 0x0301)
	tagLossyImageCompression  = core.NewTag(0x0028, 0x2110)
	tagLossyCompressionRatio  = core.NewTag(0x0028, 0x2112)
	tagLossyCompressionMethod = core.NewTag(0x0028, 0x2114)
	tagICCProfile             = core.NewTag(0x0028, 0x2000)
	tagAcquisitionContextSeq  = core.NewTag(0x0040, 0x0555)
	tagPixelData              = core.NewTag(0x7FE0, 0x0010)
)

// FromJPEG encapsulates data, a JPEG interchange-format stream, in a new
// DICOM file. The entropy-coded data and the tables are copied unchanged;
// only the marker segments the options remove are dropped.
// VL Photographic Image refuses, with ErrUnsupportedComponents, frames of
// other than 8 bits and lossy RGB (Adobe transform 0) frames, which the VL
// Image Module does not allow.
func FromJPEG(data []byte, opts JPEGOptions) (*object.File, Report, error) {
	var report Report
	sopClass := strings.TrimSpace(opts.SOPClassUID)
	if sopClass == "" {
		sopClass = SecondaryCaptureImageStorage
	}
	if sopClass != SecondaryCaptureImageStorage && sopClass != VLPhotographicImageStorage {
		return nil, report, fmt.Errorf("%w: %s", ErrUnsupportedSOPClass, sopClass)
	}
	burned := strings.ToUpper(strings.TrimSpace(opts.BurnedInAnnotation))
	if burned != "" && burned != "YES" && burned != "NO" {
		return nil, report, fmt.Errorf("%w: Burned In Annotation %q", ErrInvalidOption, opts.BurnedInAnnotation)
	}
	laterality := strings.ToUpper(strings.TrimSpace(opts.Laterality))
	if laterality != "" && laterality != "R" && laterality != "L" {
		return nil, report, fmt.Errorf("%w: Laterality %q", ErrInvalidOption, opts.Laterality)
	}

	stream, err := parseJPEGStream(data)
	if err != nil {
		return nil, report, err
	}
	syntax, process, err := jpegTransferSyntax(stream, opts.AllowRetiredProgressive)
	if err != nil {
		return nil, report, err
	}
	report.TransferSyntaxUID, report.Process = syntax.UID, process
	photometric, subsampling, err := jpegPhotometric(stream, process == "lossless")
	if err != nil {
		return nil, report, err
	}
	report.PhotometricInterpretation, report.Subsampling = photometric, subsampling
	if sopClass == VLPhotographicImageStorage {
		if err := vlImagePixels(stream, process, photometric); err != nil {
			return nil, report, err
		}
	}
	if stream.rows == 0 || stream.columns == 0 {
		return nil, report, fmt.Errorf("%w: %dx%d frame (DNL-defined heights are not supported)", ErrUnsupportedComponents, stream.columns, stream.rows)
	}

	pixels, icc := strippedStream(data, stream, opts, &report)
	report.TrailingBytesRemoved = len(data) - stream.eoi
	if len(pixels)%2 != 0 {
		pixels = append(pixels, 0)
		report.Padded = true
	}
	report.PixelDataBytes = len(pixels)

	return buildImage(opts, imageLayout{rows: stream.rows, columns: stream.columns,
		samples: len(stream.components), bits: stream.precision, process: process,
		photometric: photometric, lossless: process == "lossless", payloadBytes: len(pixels)},
		core.FragmentSequence{Fragments: [][]byte{pixels}}, syntax, report, icc)
}

// vlImagePixels checks a stream against the VL Image Module (PS3.3
// C.8.12.1.1.1-2), whose samples are 8 bits and whose lossy JPEG color
// images are YBR_FULL_422. An RGB lossy stream (Adobe transform 0) cannot be
// relabeled without re-encoding; Secondary Capture takes both.
func vlImagePixels(stream jpegStream, process, photometric string) error {
	if stream.precision != 8 {
		return fmt.Errorf("%w: VL Photographic Image needs 8-bit samples, the frame has %d (use Secondary Capture)", ErrUnsupportedComponents, stream.precision)
	}
	if process != "lossless" && photometric == "RGB" {
		return fmt.Errorf("%w: VL Photographic Image needs YBR_FULL_422 for lossy JPEG, the frame is RGB (use Secondary Capture)", ErrUnsupportedComponents)
	}
	return nil
}

// jpegTransferSyntax maps the frame header process to its transfer syntax
// (PS3.5 A.4.1 and PS3.6 Table A-1). Hierarchical, arithmetic-coded and JPEG-LS
// frames have no active transfer syntax this encapsulation writes.
func jpegTransferSyntax(stream jpegStream, allowProgressive bool) (transfer.Syntax, string, error) {
	switch stream.sofMarker {
	case markerSOF0:
		if stream.precision != 8 {
			return transfer.Syntax{}, "", fmt.Errorf("%w: baseline frame with %d-bit precision", ErrMalformedJPEG, stream.precision)
		}
		return transfer.JPEGBaseline, "baseline", nil
	case markerSOF1:
		if stream.precision != 8 && stream.precision != 12 {
			return transfer.Syntax{}, "", fmt.Errorf("%w: extended frame with %d-bit precision", ErrMalformedJPEG, stream.precision)
		}
		return transfer.JPEGExtended, "extended", nil
	case markerSOF2:
		if !allowProgressive {
			return transfer.Syntax{}, "", ErrProgressiveJPEG
		}
		if stream.precision != 8 && stream.precision != 12 {
			return transfer.Syntax{}, "", fmt.Errorf("%w: progressive frame with %d-bit precision", ErrMalformedJPEG, stream.precision)
		}
		syntax, ok := transfer.DefaultRegistry.Get(JPEGProgressiveRetired)
		if !ok {
			return transfer.Syntax{}, "", fmt.Errorf("%w: %s is not registered", ErrUnsupportedJPEGProcess, JPEGProgressiveRetired)
		}
		return syntax, "progressive", nil
	case markerSOF3:
		if stream.precision < 2 || stream.precision > 16 {
			return transfer.Syntax{}, "", fmt.Errorf("%w: lossless frame with %d-bit precision", ErrMalformedJPEG, stream.precision)
		}
		for _, predictor := range stream.predictors {
			if predictor != 1 {
				return transfer.JPEGLosslessNonHierarchical, "lossless", nil
			}
		}
		return transfer.JPEGLosslessSV1, "lossless", nil
	}
	return transfer.Syntax{}, "", fmt.Errorf("%w: frame marker FF%02X (%s)", ErrUnsupportedJPEGProcess, stream.sofMarker, sofName(stream.sofMarker))
}

func sofName(marker byte) string {
	switch {
	case marker >= 0xc5 && marker <= 0xc7:
		return "hierarchical"
	case marker >= 0xc9 && marker <= 0xcb:
		return "arithmetic coding"
	case marker >= 0xcd:
		return "hierarchical arithmetic coding"
	}
	return "unknown"
}

// jpegPhotometric chooses the Photometric Interpretation. The color space of
// three-component streams follows the Adobe transform flag, then JFIF, then
// the R/G/B component identifiers, as libjpeg does; PS3.5 8.2.1 notes JFIF is
// not required, so without any of them lossy streams are YCbCr and lossless
// ones RGB. Lossy YCbCr is YBR_FULL_422 whatever the chroma sampling
// (PS3.3 C.8.12.1.1.1 permits no other value for VL images).
func jpegPhotometric(stream jpegStream, lossless bool) (string, string, error) {
	switch len(stream.components) {
	case 1:
		return "MONOCHROME2", "", nil
	case 3:
	default:
		return "", "", fmt.Errorf("%w: %d components", ErrUnsupportedComponents, len(stream.components))
	}
	subsampling := chromaSubsampling(stream.components)
	ycc := !lossless
	switch {
	case stream.adobeTransform == 0:
		ycc = false
	case stream.adobeTransform == 1:
		ycc = true
	case stream.jfif:
		ycc = true
	case stream.components[0].id == 'R' && stream.components[1].id == 'G' && stream.components[2].id == 'B':
		ycc = false
	}
	if lossless {
		if ycc {
			return "", "", fmt.Errorf("%w: lossless stream with a YCbCr color transform", ErrUnsupportedComponents)
		}
		return "RGB", subsampling, nil
	}
	if !ycc {
		return "RGB", subsampling, nil
	}
	return "YBR_FULL_422", subsampling, nil
}

func chromaSubsampling(components []frameComponent) string {
	y, cb, cr := components[0], components[1], components[2]
	if cb != (frameComponent{id: cb.id, horizontal: cr.horizontal, vertical: cr.vertical}) {
		return "other"
	}
	switch {
	case y.horizontal == cb.horizontal && y.vertical == cb.vertical:
		return "4:4:4"
	case y.horizontal == 2*cb.horizontal && y.vertical == cb.vertical:
		return "4:2:2"
	case y.horizontal == 2*cb.horizontal && y.vertical == 2*cb.vertical:
		return "4:2:0"
	}
	return "other"
}

// strippedStream copies SOI..EOI without the removed segments and returns
// the ICC profile reassembled from APP2 ICC_PROFILE chunks, if complete.
func strippedStream(data []byte, stream jpegStream, opts JPEGOptions, report *Report) ([]byte, []byte) {
	out := make([]byte, 0, stream.eoi)
	removed := map[[2]string]int{}
	var iccChunks [][]byte
	iccCount, iccValid := 0, true
	for _, segment := range stream.segments {
		identifier := ""
		isAPP := segment.marker >= markerAPP0 && segment.marker <= 0xef
		if isAPP {
			identifier = applicationIdentifier(segment.marker, segment.payload(data))
		}
		if segment.marker == markerAPP2 && identifier == "ICC_PROFILE" {
			iccChunks, iccCount, iccValid = addICCChunk(iccChunks, iccCount, iccValid, segment.payload(data))
		}
		keep := true
		switch {
		case isAPP:
			keep = opts.KeepAPPn || segment.marker == markerAPP0 && identifier == "JFIF" || segment.marker == markerAPPE && identifier == "Adobe"
		case segment.marker == markerCOM:
			keep = opts.KeepComments
		}
		if keep {
			out = append(out, data[segment.start:segment.end]...)
			continue
		}
		name := "COM"
		if isAPP {
			name = "APP" + strconv.Itoa(int(segment.marker-markerAPP0))
		}
		key := [2]string{name, identifier}
		index, ok := removed[key]
		if !ok {
			index = len(report.RemovedSegments)
			removed[key] = index
			report.RemovedSegments = append(report.RemovedSegments, RemovedSegment{Marker: name, Identifier: identifier})
		}
		entry := &report.RemovedSegments[index]
		entry.Count++
		entry.Bytes += segment.end - segment.start
	}
	var icc []byte
	if iccValid && iccCount > 0 && len(iccChunks) == iccCount {
		for _, chunk := range iccChunks {
			if chunk == nil {
				icc = nil
				break
			}
			icc = append(icc, chunk...)
		}
	}
	return out, icc
}

// addICCChunk stores one ICC_PROFILE chunk (ICC.1 B.4: identifier, 1-based
// sequence number, chunk count, data).
func addICCChunk(chunks [][]byte, count int, valid bool, payload []byte) ([][]byte, int, bool) {
	const header = len("ICC_PROFILE\x00") + 2
	if !valid || len(payload) < header {
		return chunks, count, false
	}
	sequence, total := int(payload[header-2]), int(payload[header-1])
	if total == 0 || sequence == 0 || sequence > total || count != 0 && count != total {
		return chunks, count, false
	}
	if chunks == nil {
		chunks = make([][]byte, total)
	}
	if chunks[sequence-1] != nil {
		return chunks, count, false
	}
	chunks[sequence-1] = payload[header:]
	return chunks, total, true
}

// applicationIdentifier returns a fixed name for well-known APPn
// identifiers; unknown identifiers are not echoed because the bytes may be
// arbitrary content.
func applicationIdentifier(marker byte, payload []byte) string {
	known := []struct {
		marker byte
		prefix string
		name   string
	}{
		{markerAPP0, "JFIF\x00", "JFIF"},
		{markerAPP0, "JFXX\x00", "JFXX"},
		{0xe1, "Exif\x00", "Exif"},
		{0xe1, "http://ns.adobe.com/xap/1.0/\x00", "XMP"},
		{0xe1, "http://ns.adobe.com/xmp/extension/\x00", "XMP"},
		{markerAPP2, "ICC_PROFILE\x00", "ICC_PROFILE"},
		{markerAPP2, "MPF\x00", "MPF"},
		{markerAPP2, "FPXR\x00", "FPXR"},
		{0xec, "Ducky", "Ducky"},
		{0xed, "Photoshop 3.0\x00", "Photoshop"},
		{markerAPPE, "Adobe", "Adobe"},
	}
	for _, k := range known {
		if marker == k.marker && bytes.HasPrefix(payload, []byte(k.prefix)) {
			return k.name
		}
	}
	return ""
}

func formatRatio(ratio float64) string {
	if math.IsInf(ratio, 0) || math.IsNaN(ratio) {
		return "1"
	}
	value, err := object.FormatDS(math.Round(ratio*1000) / 1000)
	if err != nil {
		return "1"
	}
	return value
}

func newUID() (string, error) {
	uid, err := core.NewUID()
	if err != nil {
		return "", fmt.Errorf("encapimage: %w", err)
	}
	return uid, nil
}

func isASCII(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

func defaultString(value, fallback string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return fallback
}

func stringElement(tag core.Tag, vr core.VR, value string) core.Element {
	return core.Element{
		Header: core.ElementHeader{Tag: tag, VR: vr},
		Value:  core.StringValue{strings.TrimSpace(value)},
	}
}

func usElement(tag core.Tag, value int) core.Element {
	return object.USElement(tag, binary.LittleEndian, uint16(value))
}
