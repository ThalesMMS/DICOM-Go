package pixeldata

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

const (
	// DiagnosticPhotometricFromSamples reports an assumed photometric from a
	// declared sample count; this is a producer-selected convention, not detection.
	DiagnosticPhotometricFromSamples = "photometric-from-samples"
	// DiagnosticInvalidPhotometricReplaced reports an unrecognized, nonempty
	// photometric replaced under the producer-selected recovery convention.
	DiagnosticInvalidPhotometricReplaced = "invalid-photometric-replaced"
	// DiagnosticSamplesFromPhotometric reports a sample count derived from a
	// recognized photometric and a matching native byte count.
	DiagnosticSamplesFromPhotometric = "samples-per-pixel-from-photometric"
	// DiagnosticIgnoredPlanarConfiguration reports invalid mono/palette PC=1,
	// or PC=1 discarded under the inferred-ARGB interleaved convention.
	DiagnosticIgnoredPlanarConfiguration = "ignored-planar-configuration"
	// DiagnosticDefaultPlanarConfiguration reports assumed interleaved color data
	// when Planar Configuration is absent.
	DiagnosticDefaultPlanarConfiguration = "default-planar-configuration"
)

func photometricSamples(name string) uint16 {
	switch name {
	case "MONOCHROME1", "MONOCHROME2", "PALETTE COLOR":
		return 1
	case "RGB", "HSV", "YBR_FULL", "YBR_FULL_422", "YBR_PARTIAL_422", "YBR_PARTIAL_420", "YBR_ICT", "YBR_RCT", "XYB":
		return 3
	case "ARGB", "CMYK":
		return 4
	}
	return 0
}

// withPhotometricRecovery uses metadata contracts, never pixel values, to choose
// a layout. A successful size check is necessary but cannot prove the colors.
func withPhotometricRecovery(dataset *object.Object, source transfer.Syntax, limits DecompressLimits) (*object.Object, []string, error) {
	pixel, err := ExtractView(dataset)
	if err != nil {
		return nil, nil, err
	}
	if source.Encapsulated || pixel.Encapsulated {
		return nil, nil, fmt.Errorf("%w: photometric recovery requires native Pixel Data", ErrUnsupportedDecompression)
	}
	if int64(len(pixel.Raw)) > limits.MaxInputBytes {
		return nil, nil, decompressLimitError("MaxInputBytes", int64(len(pixel.Raw)), limits.MaxInputBytes)
	}
	fail := func(field string) (*object.Object, []string, error) {
		return nil, nil, fmt.Errorf("%w: photometric recovery cannot establish %s", ErrInvalidMetadata, field)
	}
	rows, okR := getUint16(dataset, tagRows)
	cols, okC := getUint16(dataset, tagColumns)
	bits, okB := getUint16(dataset, tagBitsAllocated)
	frames, err := numberOfFrames(dataset)
	if err != nil {
		return nil, nil, err
	}
	if !okR || !okC || !okB || rows == 0 || cols == 0 || (bits != 1 && bits != 8 && bits != 16) {
		return fail("native dimensions/bit depth")
	}
	samples, hasSamples := getUint16(dataset, tagSamplesPerPixel)
	if dataset.Has(tagSamplesPerPixel) && !hasSamples {
		return fail("SamplesPerPixel")
	}
	// CS is VM 1 and bounded before text decoding; a malformed/multivalued
	// value is not a producer-selected unknown photometric spelling.
	if elem, present := dataset.Get(tagPhotometricInterpretation); present {
		if elem.VR() != core.VRCS {
			return fail("PhotometricInterpretation VR")
		}
		if values, typed := elem.Value.(core.StringValue); typed {
			if len(values) > 1 || len(values) == 1 && (len(values[0]) > 18 || strings.Contains(values[0], "\\")) {
				return fail("PhotometricInterpretation VM/length")
			}
		} else if raw, ok := elem.RawBytes(); !ok || len(raw) > 18 || bytes.IndexByte(raw, '\\') >= 0 {
			return fail("PhotometricInterpretation VM/length")
		}
	}
	name, hasName := dataset.GetString(tagPhotometricInterpretation)
	if dataset.Has(tagPhotometricInterpretation) && !hasName {
		return fail("PhotometricInterpretation")
	}
	name = normalizedPhotometric(name)
	wanted := photometricSamples(name)
	inferred := wanted == 0
	var changes []core.Element
	var diagnostics []string
	if inferred {
		switch samples {
		case 1:
			name = "MONOCHROME2"
		case 3:
			name = "RGB"
		case 4:
			name = "ARGB"
		default:
			return fail("photometric from declared sample count")
		}
		wanted = samples
		changes = append(changes, photometricInterpretationElement(name))
		code := DiagnosticPhotometricFromSamples
		if original, _ := dataset.GetString(tagPhotometricInterpretation); normalizedPhotometric(original) != "" {
			code = DiagnosticInvalidPhotometricReplaced
		}
		diagnostics = append(diagnostics, code)
	}
	if name == "YBR_PARTIAL_420" || name == "YBR_ICT" || name == "YBR_RCT" || name == "XYB" {
		return fail("native photometric")
	}
	if wanted > 1 {
		signed, ok := getUint16(dataset, tagPixelRepresentation)
		if !ok || signed != 0 || bits == 1 {
			return fail("unsigned 8/16-bit color samples")
		}
	}
	if !hasSamples || samples != wanted {
		changes = append(changes, uint16Element(tagSamplesPerPixel, wanted, dataset.ValueByteOrder()))
		diagnostics = append(diagnostics, DiagnosticSamplesFromPhotometric)
	}
	planar, hasPlanar := getUint16(dataset, tagPlanarConfiguration)
	if dataset.Has(tagPlanarConfiguration) && !hasPlanar || hasPlanar && planar > 1 {
		return fail("PlanarConfiguration")
	}
	if hasPlanar && planar == 1 && (wanted == 1 || inferred && wanted == 4) {
		changes = append(changes, uint16Element(tagPlanarConfiguration, 0, dataset.ValueByteOrder()))
		diagnostics = append(diagnostics, DiagnosticIgnoredPlanarConfiguration)
	} else if !hasPlanar && wanted > 1 {
		changes = append(changes, uint16Element(tagPlanarConfiguration, 0, dataset.ValueByteOrder()))
		diagnostics = append(diagnostics, DiagnosticDefaultPlanarConfiguration)
	}
	if len(changes) == 0 {
		return dataset, nil, nil
	}
	// 4:2:2 has three described color components but two stored samples per pixel.
	if isSubsampled422(name) && (cols%2 != 0 || planar != 0) {
		return fail("native 4:2:2 layout")
	}
	size := Metadata{Rows: rows, Columns: cols, SamplesPerPixel: wanted, BitsAllocated: bits, NumberOfFrames: frames, PhotometricInterpretation: name}.TotalSize()
	if size <= 0 || int64(len(pixel.Raw)) != size && !(size%2 == 1 && int64(len(pixel.Raw)) == size+1 && pixel.Raw[len(pixel.Raw)-1] == 0) {
		return fail("exact native byte count")
	}
	if size > limits.MaxNativeBytes {
		return nil, nil, decompressLimitError("MaxNativeBytes", size, limits.MaxNativeBytes)
	}
	elements := dataset.Elements()
	for _, change := range changes {
		elements = replaceElement(elements, change)
	}
	out := object.FromElements(elements, nil)
	out.SetValueByteOrder(dataset.ValueByteOrder())
	return out, diagnostics, nil
}
