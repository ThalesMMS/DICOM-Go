package pixeldata

import (
	"encoding/binary"
	"strconv"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// DiagnosticReconciledWithCodestream: at least one Image Pixel Module
// attribute was replaced by the JPEG frame header value
// (DecodeRecovery.ReconcileWithCodestream); DecompressReport.Reconciliations
// lists each one.
const DiagnosticReconciledWithCodestream = "metadata-reconciled-with-codestream"

// MetadataReconciliation records one Image Pixel Module attribute that
// disagreed with the codestream. Declared is empty when the attribute was
// absent. Values are numbers or defined terms, never free text.
type MetadataReconciliation struct {
	Attribute string
	Tag       core.Tag
	Declared  string
	Used      string
}

// jpegFrameHeader is the part of a JPEG SOFn segment (ISO/IEC 10918-1 B.2.2)
// that the data set must agree with.
type jpegFrameHeader struct {
	marker     byte
	precision  int
	rows       int
	columns    int
	components int
}

func (h jpegFrameHeader) lossless() bool { return h.marker == 0xc3 || h.marker == 0xc7 }

// codestreamHeader is what a codestream says about the Image Pixel Module.
type codestreamHeader struct {
	rows, columns, components, precision int
	// hasSign is set for codestreams that carry the sample sign (the JPEG
	// 2000 SIZ Ssiz), which then decides Pixel Representation.
	hasSign, signed bool
	// colorPhotometric replaces a Photometric Interpretation that does not
	// fit three components.
	colorPhotometric string
}

var jpegReconcileSyntaxes = map[string]bool{
	transfer.JPEGBaseline.UID:                true,
	transfer.JPEGExtended.UID:                true,
	transfer.JPEGLosslessNonHierarchical.UID: true,
	transfer.JPEGLosslessSV1.UID:             true,
}

// codestreamHeaderOf reads the header of the first codestream of an
// encapsulated JPEG or JPEG 2000 (Part 1, Part 2, HTJ2K) Pixel Data.
func codestreamHeaderOf(fragment []byte, source transfer.Syntax) (codestreamHeader, bool) {
	switch {
	case jpegReconcileSyntaxes[source.UID]:
		frame, ok := parseJPEGFrameHeader(fragment)
		if !ok {
			return codestreamHeader{}, false
		}
		photometric := "YBR_FULL_422"
		if frame.lossless() {
			photometric = "RGB"
		}
		return codestreamHeader{rows: frame.rows, columns: frame.columns, components: frame.components, precision: frame.precision, colorPhotometric: photometric}, true
	case transfer.IsJPEG2000TransferSyntax(source.UID):
		return parseJPEG2000SIZ(fragment)
	}
	return codestreamHeader{}, false
}

// reconcileWithCodestream returns dataset, or a copy of it whose Rows,
// Columns, Samples per Pixel, Photometric Interpretation, Planar
// Configuration, Bits Allocated, Bits Stored and High Bit (and, for JPEG
// 2000, Pixel Representation) agree with the header of the first codestream,
// as GDCM does when it takes the precision and sign of the codestream. Only
// disagreeing or absent attributes change, each one reported; the input is
// never mutated.
func reconcileWithCodestream(dataset *object.Object, source transfer.Syntax) (*object.Object, []MetadataReconciliation) {
	pixel, err := ExtractView(dataset)
	if err != nil || !pixel.Encapsulated || len(pixel.Sequence.Fragments) == 0 {
		return dataset, nil
	}
	header, ok := codestreamHeaderOf(pixel.Sequence.Fragments[0], source)
	if !ok || header.precision < 2 || header.precision > 16 || (header.components != 1 && header.components != 3) {
		return dataset, nil
	}
	order := dataset.ValueByteOrder()
	elements := dataset.Elements()
	var recs []MetadataReconciliation
	setUS := func(name string, tag core.Tag, want int) {
		got, ok := getUint16(dataset, tag)
		if ok && int(got) == want {
			return
		}
		declared := ""
		if ok {
			declared = strconv.Itoa(int(got))
		}
		elements = replaceElement(elements, uint16Element(tag, uint16(want), order))
		recs = append(recs, MetadataReconciliation{Attribute: name, Tag: tag, Declared: declared, Used: strconv.Itoa(want)})
	}
	if header.rows > 0 {
		setUS("Rows", tagRows, header.rows)
	}
	setUS("Columns", tagColumns, header.columns)
	setUS("SamplesPerPixel", tagSamplesPerPixel, header.components)

	photometric, _ := dataset.GetString(tagPhotometricInterpretation)
	photometric = normalizedPhotometric(photometric)
	if !photometricFitsComponents(photometric, header.components) {
		want := "MONOCHROME2"
		if header.components == 3 {
			want = header.colorPhotometric
		}
		elements = replaceElement(elements, photometricInterpretationElement(want))
		recs = append(recs, MetadataReconciliation{Attribute: "PhotometricInterpretation", Tag: tagPhotometricInterpretation, Declared: photometric, Used: want})
	}
	if header.components == 3 {
		setUS("PlanarConfiguration", tagPlanarConfiguration, 0)
	}

	allocated := 8
	if header.precision > 8 {
		allocated = 16
	}
	if got, ok := getUint16(dataset, tagBitsAllocated); !ok || got != 8 && got != 16 || int(got) < header.precision {
		setUS("BitsAllocated", tagBitsAllocated, allocated)
	}
	setUS("BitsStored", tagBitsStored, header.precision)
	setUS("HighBit", tagHighBit, header.precision-1)
	if header.hasSign {
		representation := 0
		if header.signed {
			representation = 1
		}
		setUS("PixelRepresentation", tagPixelRepresentation, representation)
	}
	if len(recs) == 0 {
		return dataset, nil
	}
	out := object.FromElements(elements, nil)
	out.SetValueByteOrder(order)
	return out, recs
}

// parseJPEG2000SIZ reads the image and component size marker segment (ISO/IEC
// 15444-1 A.5.1), which follows SOC. All components must share their
// precision and sign and be sampled at full resolution.
func parseJPEG2000SIZ(data []byte) (codestreamHeader, bool) {
	// SOC, then SIZ: Lsiz, Rsiz, Xsiz, Ysiz, XOsiz, YOsiz, XTsiz, YTsiz,
	// XTOsiz, YTOsiz, Csiz, then Ssiz, XRsiz, YRsiz per component.
	const fixed = 2 + 2 + 2 + 2 + 8*4 + 2
	if len(data) < fixed || data[0] != 0xff || data[1] != 0x4f || data[2] != 0xff || data[3] != 0x51 {
		return codestreamHeader{}, false
	}
	width := int64(binary.BigEndian.Uint32(data[8:])) - int64(binary.BigEndian.Uint32(data[16:]))
	height := int64(binary.BigEndian.Uint32(data[12:])) - int64(binary.BigEndian.Uint32(data[20:]))
	components := int(binary.BigEndian.Uint16(data[40:]))
	if width <= 0 || height <= 0 || width > 0xffff || height > 0xffff || components < 1 || len(data) < fixed+3*components {
		return codestreamHeader{}, false
	}
	header := codestreamHeader{rows: int(height), columns: int(width), components: components, hasSign: true, colorPhotometric: "RGB"}
	for c := 0; c < components; c++ {
		ssiz, xr, yr := data[fixed+3*c], data[fixed+3*c+1], data[fixed+3*c+2]
		precision, signed := int(ssiz&0x7f)+1, ssiz&0x80 != 0
		if xr != 1 || yr != 1 || c > 0 && (precision != header.precision || signed != header.signed) {
			return codestreamHeader{}, false
		}
		header.precision, header.signed = precision, signed
	}
	return header, true
}

func photometricFitsComponents(photometric string, components int) bool {
	switch photometric {
	case "MONOCHROME1", "MONOCHROME2":
		return components == 1
	case "RGB", "YBR_FULL", "YBR_FULL_422", "YBR_PARTIAL_422", "YBR_PARTIAL_420", "YBR_ICT", "YBR_RCT":
		return components == 3
	}
	return false
}

// parseJPEGFrameHeader walks the marker segments of the start of a JPEG
// codestream up to its first SOFn. It reads only data it was given and stops
// at the first scan.
func parseJPEGFrameHeader(data []byte) (jpegFrameHeader, bool) {
	if len(data) < 2 || data[0] != 0xff || data[1] != 0xd8 {
		return jpegFrameHeader{}, false
	}
	for i := 2; i+4 <= len(data); {
		if data[i] != 0xff {
			return jpegFrameHeader{}, false
		}
		marker := data[i+1]
		switch {
		case marker == 0xff: // fill byte
			i++
			continue
		case marker == 0x01 || marker >= 0xd0 && marker <= 0xd7: // TEM, RSTn
			i += 2
			continue
		case marker == 0xda || marker == 0xd9: // SOS, EOI
			return jpegFrameHeader{}, false
		}
		length := int(binary.BigEndian.Uint16(data[i+2:]))
		if length < 2 || i+2+length > len(data) {
			return jpegFrameHeader{}, false
		}
		if marker >= 0xc0 && marker <= 0xcf && marker != 0xc4 && marker != 0xc8 && marker != 0xcc {
			if length < 8 {
				return jpegFrameHeader{}, false
			}
			segment := data[i+4:]
			return jpegFrameHeader{
				marker:     marker,
				precision:  int(segment[0]),
				rows:       int(binary.BigEndian.Uint16(segment[1:])),
				columns:    int(binary.BigEndian.Uint16(segment[3:])),
				components: int(segment[5]),
			}, true
		}
		i += 2 + length
	}
	return jpegFrameHeader{}, false
}
