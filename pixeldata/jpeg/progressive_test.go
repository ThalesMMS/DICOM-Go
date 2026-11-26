package jpeg

import (
	"bytes"
	"encoding/base64"
	"errors"
	"image"
	stdjpeg "image/jpeg"
	"math"
	"strconv"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/internal/jpegfixture"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
)

// progressiveObject wraps one stream per frame in an 8-bit object.
func progressiveObject(t testing.TB, rows, columns, components int, bitsAllocated uint16, frames ...[]byte) (*object.Object, pixeldata.PixelData) {
	t.Helper()
	photometric := "MONOCHROME2"
	if components == 3 {
		photometric = "YBR_FULL_422"
	}
	elements := []core.Element{
		dicomtest.Uint16Element(tagRows, core.VRUS, nil, uint16(rows)),
		dicomtest.Uint16Element(tagColumns, core.VRUS, nil, uint16(columns)),
		dicomtest.Uint16Element(tagSamplesPerPixel, core.VRUS, nil, uint16(components)),
		dicomtest.NewStringElement(tagPhotometricInterpretation, core.VRCS, photometric),
		dicomtest.NewStringElement(tagNumberOfFrames, core.VRIS, strconv.Itoa(len(frames))),
		dicomtest.Uint16Element(tagBitsAllocated, core.VRUS, nil, bitsAllocated),
		dicomtest.Uint16Element(tagBitsStored, core.VRUS, nil, 8),
		dicomtest.Uint16Element(tagHighBit, core.VRUS, nil, 7),
		dicomtest.Uint16Element(tagPixelRepresentation, core.VRUS, nil, 0),
	}
	if components == 3 {
		elements = append(elements, dicomtest.Uint16Element(tagPlanarConfiguration, core.VRUS, nil, 0))
	}
	padded := make([][]byte, len(frames))
	for i, frame := range frames {
		padded[i] = append([]byte(nil), frame...)
		if len(padded[i])%2 == 1 {
			padded[i] = append(padded[i], 0)
		}
	}
	elements = append(elements, dicomtest.NewFragmentSequenceElement(core.TagPixelData, nil, padded...))
	obj := object.FromElements(elements, nil)
	pixel, err := pixeldata.Extract(obj)
	if err != nil {
		t.Fatal(err)
	}
	return obj, pixel
}

func progressiveRegistry(t testing.TB) *pixeldata.MemoryRegistry {
	t.Helper()
	registry := pixeldata.NewMemoryRegistry()
	if err := Register(registry); err != nil {
		t.Fatal(err)
	}
	return registry
}

func decodeBase64(t testing.TB, value string) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestProgressiveIndependentFixtures decodes libjpeg-turbo cjpeg progressive
// streams (full progression with successive approximation, and spectral
// selection only) under the retired .53 and .55 transfer syntaxes and
// compares them with libjpeg-turbo djpeg's samples within 2 levels (IDCT and
// color-conversion rounding). Each object holds two frames. The PSNR against
// the source stays above 35 dB for gray and 20 dB for color, whose wrapping
// gradients lose detail to the 2x2 chroma subsampling.
func TestProgressiveIndependentFixtures(t *testing.T) {
	registry := progressiveRegistry(t)
	for _, vector := range jpegfixture.ProgressiveVectors() {
		uids := []string{UIDFullProgression}
		if vector.SpectralSelectionOnly {
			// Spectral selection is also a subset of full progression.
			uids = []string{UIDSpectralSelection, UIDFullProgression}
		}
		for _, uid := range uids {
			t.Run(vector.Name+"/"+uid, func(t *testing.T) {
				stream := decodeBase64(t, vector.JPEGBase64)
				want := decodeBase64(t, vector.DjpegBase64)
				obj, pixel := progressiveObject(t, vector.Rows, vector.Columns, vector.Components, 8, stream, stream)
				frames, err := registry.DecodeFrames(uid, pixel, obj)
				if err != nil {
					t.Fatalf("DecodeFrames() error = %v", err)
				}
				if frames.Rows != vector.Rows || frames.Columns != vector.Columns || len(frames.Data) != 2 {
					t.Fatalf("frames %dx%d count %d", frames.Rows, frames.Columns, len(frames.Data))
				}
				if vector.Components == 3 && frames.PhotometricInterpretation != "RGB" {
					t.Fatalf("PhotometricInterpretation = %q, want RGB", frames.PhotometricInterpretation)
				}
				limit, floor := 2, 35.0
				if vector.Components == 3 {
					floor = 20
				}
				for f, got := range frames.Data {
					if len(got) != len(want) {
						t.Fatalf("frame %d has %d bytes, djpeg %d", f, len(got), len(want))
					}
					worst, squared := 0, 0.0
					for i := range got {
						worst = max(worst, abs(int(got[i])-int(want[i])))
						p := i / vector.Components
						source := jpegfixture.ProgressiveSample(p%vector.Columns, p/vector.Columns, i%vector.Components, vector.Components)
						d := float64(int(got[i]) - int(source))
						squared += d * d
					}
					psnr := 10 * math.Log10(255*255/(squared/float64(len(got))))
					if worst > limit || psnr < floor {
						t.Fatalf("frame %d: max |diff| from djpeg %d (limit %d), PSNR %.1f dB", f, worst, limit, psnr)
					}
					t.Logf("frame %d: max |diff| from djpeg %d, PSNR %.1f dB", f, worst, psnr)
				}
			})
		}
	}
}

// TestProgressiveRefusesOtherProcesses keeps the retired progressive
// syntaxes to 8-bit SOF2 frames and the other syntaxes free of SOF2.
func TestProgressiveRefusesOtherProcesses(t *testing.T) {
	registry := progressiveRegistry(t)
	vector := jpegfixture.ProgressiveVectors()[0]
	progressive := decodeBase64(t, vector.JPEGBase64)
	img, err := stdjpeg.Decode(bytes.NewReader(progressive))
	if err != nil {
		t.Fatal(err)
	}
	var baseline bytes.Buffer
	if err := stdjpeg.Encode(&baseline, img.(*image.Gray), &stdjpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name          string
		uid           string
		stream        []byte
		bitsAllocated uint16
		want          error
	}{
		{"baseline SOF0 under .55", UIDFullProgression, baseline.Bytes(), 8, ErrInvalidFragment},
		{"baseline SOF0 under .53", UIDSpectralSelection, baseline.Bytes(), 8, ErrInvalidFragment},
		{"SOF2 under baseline .50", UID, progressive, 8, ErrInvalidFragment},
		{"SOF2 under extended .51", UIDExtended, progressive, 8, ErrInvalidFragment},
		{"unqualified stored precision", UIDFullProgression, progressive, 16, ErrUnsupportedBitsStored},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj, pixel := progressiveObject(t, vector.Rows, vector.Columns, 1, tc.bitsAllocated, tc.stream)
			if tc.bitsAllocated == 16 {
				// 16-bit metadata with a 12-bit Bits Stored.
				obj.SetUS(tagBitsStored, 12)
				obj.SetUS(tagHighBit, 11)
			}
			if _, err := registry.DecodeFrames(tc.uid, pixel, obj); !errors.Is(err, tc.want) {
				t.Fatalf("DecodeFrames() error = %v, want %v", err, tc.want)
			}
		})
	}
	// The SOF0 control decodes under Baseline.
	obj, pixel := progressiveObject(t, vector.Rows, vector.Columns, 1, 8, baseline.Bytes())
	if _, err := registry.DecodeFrames(UID, pixel, obj); err != nil {
		t.Fatalf("baseline control: %v", err)
	}
}
