package pixeldata

import (
	"bytes"
	"encoding/binary"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// DiagnosticJP2ContainerUnwrapped: encapsulated JPEG 2000 Pixel Data held JP2
// files instead of codestreams (PS3.5 A.4.4 requires the codestream alone);
// DecodeRecovery.UnwrapJP2Container decoded the codestream box of each.
const DiagnosticJP2ContainerUnwrapped = "jp2-container-unwrapped"

// jp2Signature is the JPEG 2000 Signature box that starts every JP2 file
// (ISO/IEC 15444-1 I.5.1).
var jp2Signature = []byte{0, 0, 0, 0x0c, 'j', 'P', ' ', ' ', 0x0d, 0x0a, 0x87, 0x0a}

// jp2Codestream returns the contents of the first Contiguous Codestream box
// ("jp2c", ISO/IEC 15444-1 I.5.4) of a JP2 file. Boxes are walked by their
// lengths (I.4): 0 extends to the end of the data, 1 announces a 64-bit
// length.
func jp2Codestream(data []byte) ([]byte, bool) {
	if !bytes.HasPrefix(data, jp2Signature) {
		return nil, false
	}
	for offset := 0; offset+8 <= len(data); {
		length := uint64(binary.BigEndian.Uint32(data[offset:]))
		boxType := string(data[offset+4 : offset+8])
		header := uint64(8)
		switch length {
		case 0:
			length = uint64(len(data) - offset)
		case 1:
			if offset+16 > len(data) {
				return nil, false
			}
			length, header = binary.BigEndian.Uint64(data[offset+8:]), 16
		}
		if length < header || length > uint64(len(data)-offset) {
			return nil, false
		}
		if boxType == "jp2c" {
			return data[offset+int(header) : offset+int(length)], true
		}
		offset += int(length)
	}
	return nil, false
}

// unwrapJP2 returns dataset, or a copy of it whose encapsulated JPEG 2000
// Pixel Data holds the codestreams of the JP2 files found there: one frame
// whose fragments together form a JP2 file, or one JP2 file per fragment when
// there are as many fragments as frames. The Basic Offset Table is dropped;
// the input is never mutated.
func unwrapJP2(dataset *object.Object, source transfer.Syntax) (*object.Object, bool) {
	if !transfer.IsJPEG2000TransferSyntax(source.UID) {
		return dataset, false
	}
	pixel, err := ExtractView(dataset)
	if err != nil || !pixel.Encapsulated || len(pixel.Sequence.Fragments) == 0 || !bytes.HasPrefix(pixel.Sequence.Fragments[0], jp2Signature) {
		return dataset, false
	}
	frames, err := numberOfFrames(dataset)
	if err != nil {
		return dataset, false
	}
	var codestreams [][]byte
	switch {
	case frames == 1:
		codestream, ok := jp2Codestream(bytes.Join(pixel.Sequence.Fragments, nil))
		if !ok {
			return dataset, false
		}
		codestreams = [][]byte{codestream}
	case frames == len(pixel.Sequence.Fragments):
		for _, fragment := range pixel.Sequence.Fragments {
			codestream, ok := jp2Codestream(fragment)
			if !ok {
				return dataset, false
			}
			codestreams = append(codestreams, codestream)
		}
	default:
		return dataset, false
	}
	for i, codestream := range codestreams {
		if len(codestream)%2 != 0 {
			// Fragments have even lengths (PS3.5 A.4); the codestream
			// ends with EOC, so a trailing pad byte is harmless.
			codestreams[i] = append(codestream[:len(codestream):len(codestream)], 0)
		}
	}
	element := core.Element{
		Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB, Length: core.UndefinedLength},
		Value:  core.FragmentSequence{Fragments: codestreams},
	}
	out := object.FromElements(replaceElement(dataset.Elements(), element), nil)
	out.SetValueByteOrder(dataset.ValueByteOrder())
	return out, true
}
