package pixeldata

import (
	"encoding/binary"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// LossyEvidence names what decided a LossyState.
type LossyEvidence string

const (
	// LossyEvidenceNone: nothing says the pixels were lossy compressed.
	LossyEvidenceNone LossyEvidence = ""
	// LossyEvidenceAttribute: Lossy Image Compression (0028,2110) is 01.
	LossyEvidenceAttribute LossyEvidence = "attribute"
	// LossyEvidenceTransferSyntax: the transfer syntax is lossy only (JPEG
	// Baseline and Extended, video, JPEG XL JPEG recompression).
	LossyEvidenceTransferSyntax LossyEvidence = "transfer-syntax"
	// LossyEvidenceCodestream: the first codestream uses a lossy process (a
	// JPEG DCT frame, a JPEG-LS scan with NEAR > 0, a JPEG 2000 irreversible
	// wavelet or quantization).
	LossyEvidenceCodestream LossyEvidence = "codestream"
)

// LossyState says whether the pixels of a data set have been through lossy
// compression (PS3.3 C.7.6.1.1.5), from the attribute, the transfer syntax
// and the first codestream, as GDCM combines them. Method is the defined
// term of Lossy Image Compression Method for the evidence found, empty when
// it is the attribute or unknown.
type LossyState struct {
	Lossy    bool
	Evidence LossyEvidence
	Method   string
}

// Lossy Image Compression Method defined terms (PS3.3 C.7.6.1.1.5.1).
const (
	lossyMethodJPEG    = "ISO_10918_1"
	lossyMethodJPEGLS  = "ISO_14495_1"
	lossyMethodJ2K     = "ISO_15444_1"
	lossyMethodHTJ2K   = "ISO_15444_15"
	lossyMethodJPEGXL  = "ISO_18181_1"
	lossyMethodMPEG2   = "ISO_13818_2"
	lossyMethodMPEG4   = "ISO_14496_10"
	lossyMethodHEVC    = "ISO_23008_2"
	lossyStateMaxBytes = 1 << 20
)

// DetectLossyState reports the lossy compression state of a file's pixels.
// Codestream evidence needs the encapsulated Pixel Data in memory; a
// deferred value is judged by the attribute and transfer syntax alone.
func DetectLossyState(file *object.File) LossyState {
	if file == nil || file.Dataset == nil {
		return LossyState{}
	}
	return DetectDataSetLossyState(file.Dataset, file.TransferSyntax)
}

// DetectDataSetLossyState is DetectLossyState for a data set encoded in
// syntax.
func DetectDataSetLossyState(dataset *object.Object, syntax transfer.Syntax) LossyState {
	if dataset == nil {
		return LossyState{}
	}
	if value, _ := dataset.GetString(tagLossyImageCompression); strings.TrimSpace(value) == "01" {
		return LossyState{Lossy: true, Evidence: LossyEvidenceAttribute}
	}
	uid := transfer.NormalizeUID(syntax.UID)
	if method, ok := lossyOnlySyntaxMethod(uid); ok {
		state := LossyState{Lossy: true, Evidence: LossyEvidenceTransferSyntax, Method: method}
		if uid == transfer.JPEGBaseline.UID || uid == transfer.JPEGExtended.UID {
			if lossy, known := codestreamLossy(dataset, uid); known && !lossy {
				return LossyState{}
			}
		}
		return state
	}
	if lossy, known := codestreamLossy(dataset, uid); known && lossy {
		return LossyState{Lossy: true, Evidence: LossyEvidenceCodestream, Method: codestreamLossyMethod(uid)}
	}
	return LossyState{}
}

func lossyOnlySyntaxMethod(uid string) (string, bool) {
	switch {
	case uid == transfer.JPEGBaseline.UID, uid == transfer.JPEGExtended.UID:
		return lossyMethodJPEG, true
	case uid == transfer.JPEGXLJPEGRecompression.UID:
		return lossyMethodJPEG, true
	case transfer.IsVideoTransferSyntax(uid):
		switch uid {
		case transfer.MPEG2MPML.UID, transfer.MPEG2MPMLF.UID, transfer.MPEG2MPHL.UID, transfer.MPEG2MPHLF.UID:
			return lossyMethodMPEG2, true
		case transfer.HEVCMP51.UID, transfer.HEVCM10P51.UID:
			return lossyMethodHEVC, true
		}
		return lossyMethodMPEG4, true
	}
	return "", false
}

func codestreamLossyMethod(uid string) string {
	switch {
	case transfer.IsJPEGLSTransferSyntax(uid):
		return lossyMethodJPEGLS
	case uid == transfer.HTJ2K.UID || uid == transfer.HTJ2KLossless.UID || uid == transfer.HTJ2KLosslessRPCL.UID:
		return lossyMethodHTJ2K
	case transfer.IsJPEG2000TransferSyntax(uid):
		return lossyMethodJ2K
	case transfer.IsJPEGXLTransferSyntax(uid):
		return lossyMethodJPEGXL
	}
	return lossyMethodJPEG
}

// codestreamLossy inspects the first fragment of encapsulated Pixel Data.
// known is false when there is no codestream to read or it is not
// understood.
func codestreamLossy(dataset *object.Object, uid string) (lossy, known bool) {
	element, ok := dataset.Get(core.TagPixelData)
	if !ok {
		return false, false
	}
	sequence, ok := element.Value.(core.FragmentSequence)
	if !ok || len(sequence.Fragments) == 0 {
		return false, false
	}
	data := sequence.Fragments[0]
	if len(data) > lossyStateMaxBytes {
		data = data[:lossyStateMaxBytes]
	}
	switch {
	case uid == transfer.JPEGBaseline.UID, uid == transfer.JPEGExtended.UID,
		uid == transfer.JPEGLosslessNonHierarchical.UID, uid == transfer.JPEGLosslessSV1.UID:
		frame, ok := parseJPEGFrameHeader(data)
		if !ok {
			return false, false
		}
		return !frame.lossless() && frame.marker != 0xcb && frame.marker != 0xcf, true
	case transfer.IsJPEGLSTransferSyntax(uid):
		return jpegLSNearLossy(data)
	case transfer.IsJPEG2000TransferSyntax(uid):
		if codestream, ok := jp2Codestream(data); ok {
			data = codestream
		}
		return jpeg2000Irreversible(data)
	}
	return false, false
}

// jpegLSNearLossy reads the NEAR parameter of every scan header (ISO/IEC
// 14495-1 C.2.3) in data. Entropy-coded JPEG-LS data never holds 0xFF
// followed by a byte with its high bit set, so SOS markers are found by
// scanning.
func jpegLSNearLossy(data []byte) (lossy, known bool) {
	if len(data) < 4 || data[0] != 0xff || data[1] != 0xd8 {
		return false, false
	}
	for i := 2; i+5 < len(data); i++ {
		if data[i] != 0xff || data[i+1] != 0xda {
			continue
		}
		length := int(binary.BigEndian.Uint16(data[i+2:]))
		if length < 6 || i+2+length > len(data) {
			return false, false
		}
		components := int(data[i+4])
		near := i + 5 + 2*components
		if components < 1 || near >= i+2+length {
			return false, false
		}
		if data[near] != 0 {
			return true, true
		}
		known = true
		i += 1 + length
	}
	return false, known
}

// jpeg2000Irreversible walks the main header of a JPEG 2000 codestream
// (ISO/IEC 15444-1 A.4-A.6) to its first tile: the 9-7 irreversible wavelet
// in a COD or COC, or scalar quantization in a QCD or QCC, is lossy.
func jpeg2000Irreversible(data []byte) (lossy, known bool) {
	if len(data) < 4 || data[0] != 0xff || data[1] != 0x4f {
		return false, false
	}
	components := 0
	for i := 2; i+4 <= len(data); {
		if data[i] != 0xff {
			return false, false
		}
		marker := data[i+1]
		if marker == 0x90 || marker == 0x93 || marker == 0xd9 { // SOT, SOD, EOC
			return lossy, known
		}
		length := int(binary.BigEndian.Uint16(data[i+2:]))
		if length < 2 || i+2+length > len(data) {
			return false, false
		}
		segment := data[i+4 : i+2+length]
		switch marker {
		case 0x51: // SIZ
			if len(segment) >= 36 {
				components = int(binary.BigEndian.Uint16(segment[34:]))
			}
		case 0x52: // COD: Scod, SGcod (4), SPcod: levels, xcb, ycb, style, transform
			if len(segment) < 10 {
				return false, false
			}
			known = true
			lossy = lossy || segment[9] == 0
		case 0x53: // COC: Ccoc, Scoc, SPcoc
			offset := 1
			if components >= 257 {
				offset = 2
			}
			if len(segment) < offset+6 {
				return false, false
			}
			lossy = lossy || segment[offset+5] == 0
		case 0x5c: // QCD: Sqcd
			if len(segment) < 1 {
				return false, false
			}
			known = true
			lossy = lossy || segment[0]&0x1f != 0
		case 0x5d: // QCC: Cqcc, Sqcc
			offset := 1
			if components >= 257 {
				offset = 2
			}
			if len(segment) < offset+1 {
				return false, false
			}
			lossy = lossy || segment[offset]&0x1f != 0
		}
		i += 2 + length
	}
	return false, false
}

// withDecodedLossy adds a codec's report that its output is lossy (#2028) to
// state, keeping the evidence found in the data set.
func withDecodedLossy(state LossyState, frames Frames) LossyState {
	if frames.Lossy && !state.Lossy {
		return LossyState{Lossy: true}
	}
	return state
}

// stampLossyState records state on a decompressed data set (#1142): Lossy
// Image Compression becomes 01 and, when no lossy history exists, the method
// and the ratio of native to encoded bytes are added together. An existing
// history is kept as is.
func stampLossyState(dataset *object.Object, state LossyState, nativeBytes, inputBytes int64) {
	if dataset == nil || !state.Lossy {
		return
	}
	dataset.Put(core.Element{Header: core.ElementHeader{Tag: tagLossyImageCompression, VR: core.VRCS}, Value: core.StringValue{"01"}})
	if state.Method == "" || nativeBytes <= 0 || inputBytes <= 0 ||
		dataset.Has(tagLossyImageCompressionMethod) || dataset.Has(tagLossyImageCompressionRatio) {
		return
	}
	ratio := strconv.FormatFloat(float64(nativeBytes)/float64(inputBytes), 'g', 8, 64)
	dataset.Put(core.Element{Header: core.ElementHeader{Tag: tagLossyImageCompressionRatio, VR: core.VRDS}, Value: core.StringValue{ratio}})
	dataset.Put(core.Element{Header: core.ElementHeader{Tag: tagLossyImageCompressionMethod, VR: core.VRCS}, Value: core.StringValue{state.Method}})
}
