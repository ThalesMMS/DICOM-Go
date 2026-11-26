package parametricmap

import (
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/render"
	"github.com/ThalesMMS/dicom-go/roi"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var (
	testIntegerPaddingValue = core.NewTag(0x0028, 0x0120)
	testIntegerPaddingLimit = core.NewTag(0x0028, 0x0121)
)

func TestIntegerPixelPaddingThroughPart10(t *testing.T) {
	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian} {
		t.Run(syntax.UID, func(t *testing.T) {
			for _, tc := range []struct {
				name        string
				signed      bool
				stored      []int64
				padding     int64
				rangeLimit  int64
				useRange    bool
				wantPadding []bool
				wantValues  []float64
			}{
				{"signed inclusive range", true, []int64{-32768, -32000, 100, 200}, -32768, -32000, true,
					[]bool{true, true, false, false}, []float64{201, 401}},
				{"signed single value", true, []int64{-32768, -32767, 100, 200}, -32768, 0, false,
					[]bool{true, false, false, false}, []float64{-65533, 201, 401}},
				{"unsigned range above signed maximum", false, []int64{65534, 65535, 100, 200}, 65535, 65534, true,
					[]bool{true, true, false, false}, []float64{201, 401}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					file := integerPaddingMap(t, tc.signed, 16, tc.stored)
					if !tc.signed {
						file.Dataset.Put(derivedio.CS(derivedio.TagPhotometricInterpretation, "MONOCHROME1"))
					}
					file.Dataset.Put(integerPaddingElement(testIntegerPaddingValue, tc.signed, tc.padding))
					end := tc.padding
					if tc.useRange {
						file.Dataset.Put(integerPaddingElement(testIntegerPaddingLimit, tc.signed, tc.rangeLimit))
						end = tc.rangeLimit
					}
					file.TransferSyntax = syntax
					file.Meta.Put(derivedio.UI(derivedio.TagTransferSyntaxUID, syntax.UID))
					wire := rereadPart10(t, file)
					defer wire.Close()

					meta, err := ReadMetadata(wire)
					want := Padding{Value: float64(tc.padding), RangeLimit: float64(end)}
					if err != nil {
						t.Fatal(err)
					}
					if meta.Padding == nil || *meta.Padding != want {
						t.Fatalf("ReadMetadata padding = %v, want %v", meta.Padding, want)
					}
					m, err := Read(wire)
					if err != nil {
						t.Fatal(err)
					}
					domain, err := m.WindowDomain(0)
					if err != nil || !domain.Known || domain.Min != tc.wantValues[0] || domain.Max != tc.wantValues[len(tc.wantValues)-1] {
						t.Fatalf("WindowDomain = %+v, %v, want measured values %v", domain, err, tc.wantValues)
					}
					if m.CachedBytes() != 0 {
						t.Fatalf("WindowDomain populated the frame cache")
					}
					values, mask := make([]float64, 4), make([]bool, 4)
					counts, err := m.DecodeFrameInto(0, values, mask)
					if err != nil || !slices.Equal(mask, tc.wantPadding) || counts != (VoxelCounts{Valid: len(tc.wantValues), Padding: 4 - len(tc.wantValues)}) {
						t.Fatalf("DecodeFrameInto = %v, %v, %+v, %v", values, mask, counts, err)
					}
					var measured []float64
					for i, padding := range mask {
						if padding {
							if !math.IsNaN(values[i]) {
								t.Errorf("padding at %d = %v, want NaN", i, values[i])
							}
						} else {
							measured = append(measured, values[i])
						}
					}
					if !slices.Equal(measured, tc.wantValues) {
						t.Errorf("measured values = %v, want %v", measured, tc.wantValues)
					}
					if value, ok, err := m.SamplePatient(render.Vec3{}); ok || err != nil {
						t.Errorf("SamplePatient on a padding voxel = %v, %v, %v", value, ok, err)
					}
					full := roi.NewRasterMask(2, 2)
					full.SetRun(0, 0, 2)
					full.SetRun(1, 0, 2)
					stats, err := m.MaskStatistics(0, full)
					if err != nil || stats.Count != counts.Valid || stats.Unmeasured != counts.Padding ||
						stats.Min != tc.wantValues[0] || stats.Max != tc.wantValues[len(tc.wantValues)-1] {
						t.Errorf("MaskStatistics = %+v, %v, want values %v and %d unmeasured", stats, err, tc.wantValues, counts.Padding)
					}
				})
			}
		})
	}
}

func TestIntegerPixelPaddingRetainsDeferredFramesAfterClose(t *testing.T) {
	file := integerPaddingMap(t, true, 12, []int64{-2048, -1, 100, 200})
	file.Dataset.Put(integerPaddingElement(testIntegerPaddingValue, true, -2048))
	reread := rereadPart10(t, file)
	defer reread.Close()
	path := filepath.Join(t.TempDir(), "integer-map.dcm")
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(object.WriteFile(out, reread), out.Close()); err != nil {
		t.Fatal(err)
	}
	wire, err := object.OpenFileWithOptions(path, object.ReadFileOptions{DeferPixelData: true})
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := Read(wire)
	pixel, found := wire.Dataset.Get(core.TagPixelData)
	if err := errors.Join(readErr, wire.Close()); err != nil {
		t.Fatal(err)
	}
	if !found || pixel.Value != nil {
		t.Fatalf("Read materialized Pixel Data in its input")
	}
	values, err := got.FrameValues(0)
	if err != nil || len(values) != 4 || !math.IsNaN(values[0]) || !slices.Equal(values[1:], []float64{-1, 201, 401}) {
		t.Fatalf("FrameValues after closing input = %v, %v", values, err)
	}
}

func TestIntegerPixelPaddingRejectsInvalidMetadata(t *testing.T) {
	for _, tc := range []struct {
		name     string
		elements []core.Element
	}{
		{"limit without value", []core.Element{integerPaddingElement(testIntegerPaddingLimit, true, -1)}},
		{"unsigned VR on signed data", []core.Element{derivedio.US(testIntegerPaddingValue, 0)}},
		{"outside Bits Stored", []core.Element{integerPaddingElement(testIntegerPaddingValue, true, -2049)}},
		{"out-of-range limit", []core.Element{
			integerPaddingElement(testIntegerPaddingValue, true, -2048),
			integerPaddingElement(testIntegerPaddingLimit, true, 2048),
		}},
		{"two values", []core.Element{integerPaddingElement(testIntegerPaddingValue, true, -2048, -2047)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := integerPaddingMap(t, true, 12, []int64{-2048, -1, 100, 200})
			for _, element := range tc.elements {
				file.Dataset.Put(element)
			}
			for _, read := range []func(*object.File) (*Map, error){Read, ReadMetadata} {
				if m, err := read(file); m != nil || !errors.Is(err, ErrInvalidPadding) {
					t.Errorf("map present = %t, err = %v, want ErrInvalidPadding", m != nil, err)
				}
			}
		})
	}
}

func integerPaddingMap(t *testing.T, signed bool, bitsStored int, stored []int64) *object.File {
	t.Helper()
	file := testMapFile(t, PayloadInteger, make([]float64, len(stored)), 1, true, core.Tag{})
	if signed {
		file.Dataset.Put(derivedio.US(derivedio.TagPixelRepresentation, 1))
	}
	file.Dataset.Put(derivedio.US(derivedio.TagBitsStored, uint16(bitsStored)))
	file.Dataset.Put(derivedio.US(derivedio.TagHighBit, uint16(bitsStored-1)))
	raw := make([]byte, len(stored)*2)
	for i, value := range stored {
		binary.LittleEndian.PutUint16(raw[i*2:], uint16(value))
	}
	file.Dataset.Put(derivedio.Raw(derivedio.TagPixelData, core.VROW, raw))
	return file
}

func integerPaddingElement(tag core.Tag, signed bool, values ...int64) core.Element {
	if !signed {
		return derivedio.US(tag, uint16(values[0]))
	}
	ss := make(core.Int16Value, len(values))
	for i, v := range values {
		ss[i] = int16(v)
	}
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: core.VRSS}, Value: ss}
}
