package jpegls

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// Keep the independent encoded frames, source RGB and generator tied to the
// reviewed provenance instead of silently accepting a replaced oracle corpus.
func TestHPFixtureProvenance(t *testing.T) {
	root := filepath.Join("..", "codecfixture", "testdata", "codecs", "jpegls-hp")
	data, err := os.ReadFile(filepath.Join(root, "generation.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Assets []struct {
			Path   string
			Bytes  int
			SHA256 string
		}
		Fixtures   []json.RawMessage
		Provenance struct{ Synthetic, NoPHI bool }
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Assets) != 15 || len(manifest.Fixtures) != 12 || !manifest.Provenance.Synthetic || !manifest.Provenance.NoPHI {
		t.Fatal("incomplete independent fixture provenance")
	}
	seen := make(map[string]bool)
	for _, asset := range manifest.Assets {
		if asset.Path != filepath.Base(asset.Path) || seen[asset.Path] {
			t.Fatalf("unsafe or duplicate asset %q", asset.Path)
		}
		seen[asset.Path] = true
		data, err := os.ReadFile(filepath.Join(root, asset.Path))
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(data)
		if len(data) != asset.Bytes || hex.EncodeToString(digest[:]) != asset.SHA256 {
			t.Fatalf("independent fixture %q changed without provenance", asset.Path)
		}
	}
}

func hpFixture(t testing.TB, bits, hp, ilv int) ([]byte, []byte, pixeldata.Metadata) {
	t.Helper()
	root := filepath.Join("..", "codecfixture", "testdata", "codecs", "jpegls-hp")
	prefix := fmt.Sprintf("rgb%d-33x17", bits)
	stream, err := os.ReadFile(filepath.Join(root, fmt.Sprintf("%s-hp%d-ilv%d.jls", prefix, hp, ilv)))
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join(root, prefix+".raw"))
	if err != nil {
		t.Fatal(err)
	}
	return stream, want, encoderMetadata(17, 33, 3, uint16(bits), "RGB")
}

func hpContext() context.Context {
	return pixeldata.DecodeContextWithRecovery(context.Background(), pixeldata.DecodeRecovery{JPEGLSHPColorTransforms: true})
}

func TestHPColorTransformsIndependentFixtures(t *testing.T) {
	for _, bits := range []int{8, 16} {
		for _, hp := range []int{1, 2, 3} {
			for _, ilv := range []int{1, 2} {
				t.Run(fmt.Sprintf("rgb%d/hp%d/ilv%d", bits, hp, ilv), func(t *testing.T) {
					stream, want, meta := hpFixture(t, bits, hp, ilv)
					obj, pixel := jpeglsObjectWithFragment(t, meta, stream)
					defer obj.Close()
					if frames, err := New().Decode(pixel, obj); !errors.Is(err, ErrInvalidCodestream) || len(frames.Data) != 0 {
						t.Fatalf("strict frames=%d error=%v", len(frames.Data), err)
					}
					frames, err := New().DecodeContext(hpContext(), pixel, obj)
					if err != nil {
						t.Fatal(err)
					}
					if len(frames.Data) != 1 || !bytes.Equal(frames.Data[0], want) || !slices.Equal(frames.Diagnostics, []string{hpDiagnostic(hp), pixeldata.DiagnosticJPEGLSHPBackendPureGo}) {
						t.Fatalf("independent RGB differs, diagnostics=%v", frames.Diagnostics)
					}
					if _, err := InspectCodestream(context.Background(), stream, 0); !errors.Is(err, ErrInvalidCodestream) {
						t.Fatalf("strict inspect: %v", err)
					}
					info, err := InspectCodestream(hpContext(), stream, 0)
					if err != nil || info.HPColorTransform != hp || info.Precision != bits || info.Interleave != ilv {
						t.Fatalf("info=%+v error=%v", info, err)
					}
					if frames, err := NewNearLossless().DecodeContext(hpContext(), pixel, obj); !errors.Is(err, ErrInvalidCodestream) || len(frames.Data) != 0 {
						t.Fatal("HP enabled under .81")
					}
				})
			}
		}
	}
}

func TestHPColorTransformsRejectUnqualifiedProfiles(t *testing.T) {
	base, _, meta := hpFixture(t, 8, 1, 1)
	app, sof, sos := markerOffset(t, base, 0xe8), markerOffset(t, base, 0xf7), markerOffset(t, base, 0xda)
	marker := append([]byte(nil), base[app:app+9]...)
	for _, tc := range []struct {
		name   string
		change func([]byte) []byte
		want   error
	}{
		{"unknown transform", func(b []byte) []byte { b[app+8] = 4; return b }, ErrInvalidCodestream},
		{"duplicate marker", func(b []byte) []byte { return insertAt(b, app, marker) }, ErrInvalidCodestream},
		{"misplaced marker", func(b []byte) []byte { return insertAt(b, len(b)-2, marker) }, ErrInvalidCodestream},
		{"precision", func(b []byte) []byte { b[sof+4] = 12; return b }, ErrInvalidCodestream},
		{"components", func(b []byte) []byte { b[sof+9] = 2; return b }, ErrInvalidCodestream},
		{"near lossless", func(b []byte) []byte { b[sos+11] = 1; return b }, ErrInvalidCodestream},
		{"noninterleaved", func(b []byte) []byte { b[sos+12] = 0; return b }, ErrInvalidCodestream},
		{"custom MAXVAL", func(b []byte) []byte { return insertAt(b, sos, lseID1(127, 0, 0, 0, 0)) }, ErrInvalidCodestream},
		{"truncated entropy", func(b []byte) []byte { return b[:len(b)-3] }, ErrInvalidCodestream},
		{"marker missing transform", func(b []byte) []byte { b[app+3] = 6; return append(b[:app+8], b[app+9:]...) }, ErrInvalidCodestream},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := tc.change(append([]byte(nil), base...))
			obj, pixel := jpeglsObjectWithFragment(t, meta, stream)
			defer obj.Close()
			if got, err := New().DecodeContext(hpContext(), pixel, obj); !errors.Is(err, tc.want) || len(got.Data) != 0 {
				t.Fatalf("frames=%d error=%v want=%v", len(got.Data), err, tc.want)
			}
		})
	}
	for _, tc := range []struct {
		name   string
		change func(*pixeldata.Metadata)
		want   error
	}{
		{"size", func(m *pixeldata.Metadata) { m.Rows++ }, pixeldata.ErrPixelDataSizeMismatch},
		{"budget", func(m *pixeldata.Metadata) { m.Rows = 65535; m.Columns = 65535 }, pixeldata.ErrEncoderOutputInvalid},
		{"precision mismatch", func(m *pixeldata.Metadata) { m.BitsAllocated = 16 }, pixeldata.ErrIncompatiblePixelData},
		{"stored bits", func(m *pixeldata.Metadata) { m.BitsStored = 7; m.HighBit = 6 }, pixeldata.ErrIncompatiblePixelData},
		{"signed", func(m *pixeldata.Metadata) { m.PixelRepresentation = 1 }, pixeldata.ErrUnsupportedEncoderMetadata},
		{"planar", func(m *pixeldata.Metadata) { m.PlanarConfiguration = 1 }, pixeldata.ErrUnsupportedEncoderMetadata},
		{"photometric", func(m *pixeldata.Metadata) { m.PhotometricInterpretation = "YBR_FULL" }, pixeldata.ErrUnsupportedEncoderMetadata},
	} {
		t.Run("metadata/"+tc.name, func(t *testing.T) {
			m := meta
			tc.change(&m)
			obj, pixel := jpeglsObjectWithFragment(t, m, base)
			defer obj.Close()
			if got, err := New().DecodeContext(hpContext(), pixel, obj); !errors.Is(err, tc.want) || len(got.Data) != 0 {
				t.Fatalf("frames=%d error=%v want=%v", len(got.Data), err, tc.want)
			}
		})
	}
}

func TestHPColorTransformsPreserveStandardAndCancel(t *testing.T) {
	stream, want, meta := interleaveFixture(t, 2)
	obj, pixel := jpeglsObjectWithFragment(t, meta, stream)
	defer obj.Close()
	got, err := New().DecodeContext(hpContext(), pixel, obj)
	if err != nil || !bytes.Equal(got.Data[0], want) || len(got.Diagnostics) != 0 {
		t.Fatalf("standard diagnostics=%v error=%v", got.Diagnostics, err)
	}
	stream, want, meta = hpFixture(t, 16, 3, 2)
	obj, pixel = jpeglsObjectWithFragment(t, meta, stream)
	defer obj.Close()
	ctx := &decoderCancelContext{Context: hpContext(), remaining: 5}
	if got, err := New().DecodeContext(ctx, pixel, obj); !errors.Is(err, context.Canceled) || len(got.Data) != 0 {
		t.Fatalf("cancel frames=%d error=%v", len(got.Data), err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := New().DecodeContext(hpContext(), pixel, obj)
			if err != nil || !bytes.Equal(got.Data[0], want) {
				t.Errorf("concurrent error=%v", err)
			}
		}()
	}
	wg.Wait()
}

func TestHPColorTransformsDecompressDataSet(t *testing.T) {
	stream, want, meta := hpFixture(t, 16, 2, 1)
	obj, _ := jpeglsObjectWithFragment(t, meta, stream)
	defer obj.Close()
	registry := pixeldata.NewMemoryRegistry()
	if err := Register(registry); err != nil {
		t.Fatal(err)
	}
	if _, _, err := pixeldata.DecompressDataSetContext(context.Background(), obj, transfer.JPEGLSLossless, pixeldata.DecompressOptions{Registry: registry}); !errors.Is(err, ErrInvalidCodestream) {
		t.Fatalf("strict decompress: %v", err)
	}
	var report pixeldata.DecompressReport
	derived, _, err := pixeldata.DecompressDataSetContext(context.Background(), obj, transfer.JPEGLSLossless, pixeldata.DecompressOptions{Registry: registry, Recovery: pixeldata.DecodeRecovery{JPEGLSHPColorTransforms: true}, Report: &report})
	if err != nil {
		t.Fatal(err)
	}
	defer derived.Close()
	native, err := pixeldata.ExtractNativeFrames(derived)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(native.Data[0], want) || !slices.Equal(report.Diagnostics, []string{hpDiagnostic(2), pixeldata.DiagnosticJPEGLSHPBackendPureGo}) {
		t.Fatalf("RGB/report mismatch %v", report.Diagnostics)
	}
}

// TestHPColorTransformsGDCM independently decodes every fixture as a DICOM
// image through GDCM, in addition to the generator's native CharLS oracle.
func TestHPColorTransformsGDCM(t *testing.T) {
	if os.Getenv("DICOMGO_JPEGLS_HP_GDCM") != "1" {
		t.Skip("set DICOMGO_JPEGLS_HP_GDCM=1 for independent GDCM decoding")
	}
	tool, err := exec.LookPath("gdcmconv")
	if err != nil {
		t.Fatal(err)
	}
	versionCtx, versionCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer versionCancel()
	versionCommand := exec.CommandContext(versionCtx, tool, "--version")
	versionCommand.WaitDelay = time.Second
	version, err := versionCommand.CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("oracle %s: %s", tool, version)
	for _, bits := range []int{8, 16} {
		for _, hp := range []int{1, 2, 3} {
			for _, ilv := range []int{1, 2} {
				t.Run(fmt.Sprintf("rgb%d/hp%d/ilv%d", bits, hp, ilv), func(t *testing.T) {
					stream, want, meta := hpFixture(t, bits, hp, ilv)
					part10, err := encodedPart10(t, stream, meta)
					if err != nil {
						t.Fatal(err)
					}
					root := t.TempDir()
					input, output := filepath.Join(root, "input.dcm"), filepath.Join(root, "output.dcm")
					if err := os.WriteFile(input, part10, 0600); err != nil {
						t.Fatal(err)
					}
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					cmd := exec.CommandContext(ctx, tool, "--raw", input, output)
					cmd.WaitDelay = time.Second
					log, err := cmd.CombinedOutput()
					if err != nil {
						t.Fatalf("GDCM: %v %s", err, log)
					}
					file, err := object.OpenFile(output)
					if err != nil {
						t.Fatal(err)
					}
					defer file.Close()
					native, err := pixeldata.ExtractNativeFrames(file.Dataset)
					if err != nil {
						t.Fatal(err)
					}
					m := native.Metadata
					if m.Rows != 17 || m.Columns != 33 || m.SamplesPerPixel != 3 || m.BitsAllocated != uint16(bits) || m.BitsStored != uint16(bits) || m.PhotometricInterpretation != "RGB" || m.PlanarConfiguration != 0 || !bytes.Equal(native.Data[0], want) {
						t.Fatal("independent GDCM RGB/metadata mismatch")
					}
				})
			}
		}
	}
}
