//go:build (jpeg2000_openjpeg || codecfull) && dcmconv_interop

package jpeg2000

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func losslessMarker(t *testing.T, stream []byte, marker byte) []byte {
	t.Helper()
	if len(stream) < 2 || !bytes.Equal(stream[:2], []byte{0xff, 0x4f}) {
		t.Fatal("missing SOC")
	}
	for pos := 2; pos+4 <= len(stream); {
		if stream[pos] != 0xff {
			t.Fatal("invalid marker header")
		}
		code := stream[pos+1]
		if code == 0x90 || code == 0x93 || code == 0xd9 {
			break
		}
		n := int(binary.BigEndian.Uint16(stream[pos+2:]))
		if n < 2 || pos+2+n > len(stream) {
			t.Fatal("invalid marker length")
		}
		if code == marker {
			return stream[pos+4 : pos+2+n]
		}
		pos += 2 + n
	}
	t.Fatalf("marker %x missing", marker)
	return nil
}

func TestOpenJPEGLosslessTilesMarkersAndIndependentReadback(t *testing.T) {
	decompress := os.Getenv("DICOM_GO_OPENJPEG_DECOMPRESS")
	if decompress == "" {
		t.Fatal("explicit dcmconv_interop qualification requires DICOM_GO_OPENJPEG_DECOMPRESS")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	for _, tc := range []struct {
		name             string
		components, bits int
	}{{"mono8", 1, 8}, {"mono12", 1, 12}, {"rgb8", 3, 8}, {"rgb16", 3, 16}} {
		t.Run(tc.name, func(t *testing.T) {
			source, want := lossySource(144, 144, tc.components, tc.bits)
			pixels, _ := source.GetRaw(core.TagPixelData)
			before := append([]byte(nil), pixels...)
			encoders := pixeldata.NewMemoryEncoderRegistry()
			if err := RegisterOpenJPEGLosslessEncoder(ctx, encoders, OpenJPEGEncoderOptions{TileWidth: 64, TileHeight: 64, Resolutions: 4}); err != nil {
				t.Fatal(err)
			}
			converted, report, err := pixeldata.TranscodeDataSet(ctx, source, transfer.ExplicitVRLittleEndian, transfer.JPEG2000LosslessOnly, pixeldata.TranscodeOptions{EncoderRegistry: encoders})
			if err != nil || report.Lossy || report.Frames != 2 {
				t.Fatalf("lossless transcode: %+v %v", report, err)
			}
			defer converted.Close()
			after, _ := source.GetRaw(core.TagPixelData)
			if !bytes.Equal(before, after) {
				t.Fatal("source pixels mutated")
			}
			pixel, err := pixeldata.Extract(converted)
			if err != nil {
				t.Fatal(err)
			}
			payloads, err := framePayloads(pixel, 2)
			if err != nil {
				t.Fatal(err)
			}
			for f, payload := range payloads {
				siz, cod := losslessMarker(t, payload, 0x51), losslessMarker(t, payload, 0x52)
				if len(siz) < 36 || binary.BigEndian.Uint32(siz[18:]) != 64 || binary.BigEndian.Uint32(siz[22:]) != 64 || len(cod) < 10 || cod[5] != 3 || cod[9] != 1 || cod[4] != 0 {
					t.Fatalf("coding not encoded: SIZ=%x COD=%x", siz, cod)
				}
				dir := t.TempDir()
				input, output := filepath.Join(dir, "frame.j2k"), filepath.Join(dir, "frame.rawl")
				if err := os.WriteFile(input, payload, 0600); err != nil {
					t.Fatal(err)
				}
				externalCtx, stop := context.WithTimeout(ctx, 10*time.Second)
				cmd := exec.CommandContext(externalCtx, decompress, "-i", input, "-o", output)
				var log limitedProcessOutput
				cmd.Stdout = &log
				cmd.Stderr = &log
				err = cmd.Run()
				stop()
				if err != nil {
					t.Fatalf("independent decode: %v %s", err, log.String())
				}
				raw, err := os.ReadFile(output)
				if err != nil {
					t.Fatal(err)
				}
				decoded, err := DecodeCoreReduced(ctx, payload, 0, CoreOptions{})
				if err != nil {
					t.Fatal(err)
				}
				for c := 0; c < tc.components; c++ {
					plane, tail, err := readPlane(raw, 144*144, tc.bits, false)
					if err != nil {
						t.Fatal(err)
					}
					raw = tail
					for i, v := range plane {
						if v != want[f][i*tc.components+c] || decoded.Components[c][i] != v {
							t.Fatalf("frame%d component%d sample%d opj=%d core=%d want=%d", f, c, i, v, decoded.Components[c][i], want[f][i*tc.components+c])
						}
					}
				}
				if len(raw) != 0 {
					t.Fatal("extra independently decoded bytes")
				}
			}
		})
	}
}
