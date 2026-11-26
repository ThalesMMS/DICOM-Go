package pixeldata

import (
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

const (
	// DiagnosticDefaultSamplesPerPixel: Samples per Pixel was absent and
	// DecodeRecovery.DefaultImagePixelModule assumed 1.
	DiagnosticDefaultSamplesPerPixel = "default-samples-per-pixel"
	// DiagnosticDefaultPhotometricInterpretation: Photometric Interpretation
	// was absent or empty and DecodeRecovery.DefaultImagePixelModule assumed
	// MONOCHROME2.
	DiagnosticDefaultPhotometricInterpretation = "default-photometric-interpretation"
	// DiagnosticNumberOfFramesFromPlanes: Number of Frames was absent and
	// DecodeRecovery.DefaultImagePixelModule took it from the ACR-NEMA
	// Planes (0028,0012) of a three-dimensional image.
	DiagnosticNumberOfFramesFromPlanes = "number-of-frames-from-planes"
	// DiagnosticUnpacked12Bit: Bits Allocated 12 Pixel Data packed two
	// samples to three bytes was unpacked to 16 bits allocated
	// (DecodeRecovery.Unpack12BitPacked).
	DiagnosticUnpacked12Bit = "unpacked-12-bit"
	// DiagnosticBitsAllocated12As16: Bits Allocated 12 Pixel Data holding
	// one 16-bit word per sample was read as 16 bits allocated
	// (DecodeRecovery.Unpack12BitPacked).
	DiagnosticBitsAllocated12As16 = "bits-allocated-12-as-16"
	// DiagnosticLIBIDODimensions: the LIBIDO ACR-NEMA Recognition Code
	// declared Columns in Rows and Rows in Columns. Recovery swapped them
	// and removed the consumed Recognition Code to avoid applying it twice.
	DiagnosticLIBIDODimensions = object.LegacyDiagnosticLIBIDODimensions
	// DiagnosticLIBIDORGB24: a native LIBIDO 24-bit RGB pixel was declared
	// as one unsigned 24-bit sample. Recovery described the unchanged bytes
	// as three interleaved unsigned 8-bit RGB samples.
	DiagnosticLIBIDORGB24 = object.LegacyDiagnosticLIBIDORGB24
)

var (
	tagImageDimensions = core.NewTag(0x0028, 0x0005)
	tagPlanes          = core.NewTag(0x0028, 0x0012)
	tagRecognitionCode = core.NewTag(0x0008, 0x0010)
)

// withLegacyImageRecovery returns dataset, or a copy of it with the legacy
// Image Pixel Module repairs that recovery allows, in the order GDCM applies
// them to ACR-NEMA objects: Number of Frames from Planes, Bits Allocated 12
// read as 16 (unpacked or as words), then Samples per Pixel 1 and Photometric
// Interpretation MONOCHROME2. A default is applied only when the rest of the
// module is present and, for native Pixel Data, its length fits one sample
// per pixel and not three. The input is never mutated; every repair is
// returned as a diagnostic. Native Bits Allocated 12 Pixel Data of neither
// length is refused with ErrPixelDataSizeMismatch.
func withLegacyImageRecovery(dataset *object.Object, source transfer.Syntax, recovery DecodeRecovery) (*object.Object, []string, error) {
	if !recovery.DefaultImagePixelModule && !recovery.Unpack12BitPacked {
		return dataset, nil, nil
	}
	pixel, err := ExtractView(dataset)
	if err != nil {
		return dataset, nil, nil
	}
	native := !pixel.Encapsulated && !source.Encapsulated
	order := dataset.ValueByteOrder()
	elements := dataset.Elements()
	var applied []string

	frames, err := numberOfFrames(dataset)
	if err != nil {
		return dataset, nil, nil
	}
	if recovery.DefaultImagePixelModule {
		_, hasFrames := dataset.GetString(tagNumberOfFrames)
		dims, _ := getUint16(dataset, tagImageDimensions)
		if planes, ok := getUint16(dataset, tagPlanes); ok && !hasFrames && dims == 3 && planes > 1 {
			frames = int(planes)
			elements = replaceElement(elements, core.Element{
				Header: core.ElementHeader{Tag: tagNumberOfFrames, VR: core.VRIS},
				Value:  core.StringValue{strconv.Itoa(frames)},
			})
			applied = append(applied, DiagnosticNumberOfFramesFromPlanes)
		}
	}

	rows, okRows := getUint16(dataset, tagRows)
	columns, okColumns := getUint16(dataset, tagColumns)
	bits, okBits := getUint16(dataset, tagBitsAllocated)
	if !okRows || !okColumns || !okBits {
		return dataset, nil, nil
	}
	libidoRGB := false
	if recovery.DefaultImagePixelModule && native && rows > 0 && columns > 0 && isLIBIDOImage(dataset) {
		repaired := false
		if rows != columns {
			rows, columns = columns, rows
			elements = replaceElement(elements, uint16Element(tagRows, rows, order))
			elements = replaceElement(elements, uint16Element(tagColumns, columns, order))
			applied = append(applied, DiagnosticLIBIDODimensions)
			repaired = true
		}
		if isLIBIDORGB24(dataset, rows, columns, frames, int64(len(pixel.Raw))) {
			bits = 8
			for _, replacement := range []core.Element{
				uint16Element(tagSamplesPerPixel, 3, order),
				photometricInterpretationElement("RGB"),
				uint16Element(tagPlanarConfiguration, 0, order),
				uint16Element(tagBitsAllocated, 8, order),
				uint16Element(tagBitsStored, 8, order),
				uint16Element(tagHighBit, 7, order),
			} {
				elements = replaceElement(elements, replacement)
			}
			applied = append(applied, DiagnosticLIBIDORGB24)
			libidoRGB, repaired = true, true
		}
		if repaired {
			// The output is normalized metadata, not a LIBIDO declaration.
			// Keeping the marker would swap dimensions on another decode or
			// when UpgradeLegacy is subsequently applied.
			kept := make([]core.Element, 0, len(elements))
			for _, element := range elements {
				if element.Tag() != tagRecognitionCode {
					kept = append(kept, element)
				}
			}
			elements = kept
		}
	}
	samples, hasSamples := getUint16(dataset, tagSamplesPerPixel)
	if !hasSamples {
		samples = 1
	}
	raw := pixel.Raw
	if recovery.Unpack12BitPacked && native && bits == 12 {
		// Zero dimensions are left to the decoder's metadata check.
		if count, ok := checkedDecompressProduct(int64(rows), int64(columns), int64(frames), int64(samples)); ok {
			packed, size := count/2*3, int64(len(raw))
			switch {
			case count%2 == 0 && (size == packed || packed%2 == 1 && size == packed+1):
				raw = unpack12Bit(raw[:packed], order)
				elements = replaceElement(elements, core.NewRawElement(core.TagPixelData, core.VROW, raw))
				applied = append(applied, DiagnosticUnpacked12Bit)
			case count <= math.MaxInt64/2 && size == count*2:
				applied = append(applied, DiagnosticBitsAllocated12As16)
			default:
				return nil, nil, fmt.Errorf("%w: Bits Allocated 12 Pixel Data of %d bytes holds %d samples neither packed 12-bit (3 bytes per 2) nor as 16-bit words", ErrPixelDataSizeMismatch, size, count)
			}
			bits = 16
			elements = replaceElement(elements, uint16Element(tagBitsAllocated, 16, order))
		}
	}

	if recovery.DefaultImagePixelModule {
		// RGB24 already supplies its complete sample and photometric module.
		// Do not derive monochrome defaults from the original declarations.
		if libidoRGB {
			out := object.FromElements(elements, nil)
			out.SetValueByteOrder(order)
			return out, applied, nil
		}
		photometric, _ := dataset.GetString(tagPhotometricInterpretation)
		photometric = normalizedPhotometric(photometric)
		missingPhotometric := photometric == ""
		fits := !native || fitsOneSample(int64(len(raw)), rows, columns, bits, frames)
		switch {
		case !hasSamples && (missingPhotometric || singleSamplePhotometric(photometric)) && fits:
			elements = replaceElement(elements, uint16Element(tagSamplesPerPixel, 1, order))
			applied = append(applied, DiagnosticDefaultSamplesPerPixel)
			if missingPhotometric {
				elements = replaceElement(elements, photometricInterpretationElement("MONOCHROME2"))
				applied = append(applied, DiagnosticDefaultPhotometricInterpretation)
			}
		case hasSamples && samples == 1 && missingPhotometric && fits:
			elements = replaceElement(elements, photometricInterpretationElement("MONOCHROME2"))
			applied = append(applied, DiagnosticDefaultPhotometricInterpretation)
		}
	}
	if len(applied) == 0 {
		return dataset, nil, nil
	}
	out := object.FromElements(elements, nil)
	out.SetValueByteOrder(order)
	return out, applied, nil
}

func isLIBIDOImage(dataset *object.Object) bool {
	if class, _ := dataset.GetUID(core.NewTag(0x0008, 0x0016)); strings.TrimSpace(class) != "" {
		return false
	}
	code, _ := dataset.GetString(tagRecognitionCode)
	return code == "ACRNEMA_LIBIDO_1.0" || code == "ACRNEMA_LIBIDO_1.1"
}

func isLIBIDORGB24(dataset *object.Object, rows, columns uint16, frames int, bytes int64) bool {
	for tag, want := range map[core.Tag]uint16{tagBitsAllocated: 24, tagBitsStored: 24, tagHighBit: 23, tagPixelRepresentation: 0} {
		if value, ok := getUint16(dataset, tag); !ok || value != want {
			return false
		}
	}
	if samples, ok := getUint16(dataset, tagSamplesPerPixel); dataset.Has(tagSamplesPerPixel) && (!ok || samples != 1) {
		return false
	}
	if photometric, _ := dataset.GetString(tagPhotometricInterpretation); strings.TrimSpace(photometric) != "" {
		return false
	}
	if planar, ok := getUint16(dataset, tagPlanarConfiguration); dataset.Has(tagPlanarConfiguration) && (!ok || planar != 0) {
		return false
	}
	frameBytes := int64(rows) * int64(columns) * 3
	return frameBytes > 0 && frames > 0 && int64(frames) <= math.MaxInt64/frameBytes && bytes == frameBytes*int64(frames)
}

// fitsOneSample reports whether n native bytes hold one sample per pixel and
// cannot hold three.
func fitsOneSample(n int64, rows, columns, bits uint16, frames int) bool {
	one := Metadata{Rows: rows, Columns: columns, SamplesPerPixel: 1, BitsAllocated: bits, NumberOfFrames: frames}.TotalSize()
	three := Metadata{Rows: rows, Columns: columns, SamplesPerPixel: 3, BitsAllocated: bits, NumberOfFrames: frames}.TotalSize()
	return one > 0 && n >= one && (three <= 0 || n < three)
}

// unpack12Bit expands every three bytes into two 12-bit samples, the first
// from the first byte and the low nibble of the second, the other from the
// high nibble of the second byte and the third byte, written as 16-bit words
// in order.
func unpack12Bit(packed []byte, order binary.ByteOrder) []byte {
	out := make([]byte, len(packed)/3*4)
	for i, o := 0, 0; i+2 < len(packed); i, o = i+3, o+4 {
		b0, b1, b2 := uint16(packed[i]), uint16(packed[i+1]), uint16(packed[i+2])
		order.PutUint16(out[o:], b0|(b1&0x0f)<<8)
		order.PutUint16(out[o+2:], b1>>4|b2<<4)
	}
	return out
}

func uint16Element(tag core.Tag, value uint16, order binary.ByteOrder) core.Element {
	raw := make([]byte, 2)
	order.PutUint16(raw, value)
	return core.NewRawElement(tag, core.VRUS, raw)
}

func singleSamplePhotometric(value string) bool {
	switch strings.TrimSpace(value) {
	case "MONOCHROME1", "MONOCHROME2", "PALETTE COLOR":
		return true
	}
	return false
}
