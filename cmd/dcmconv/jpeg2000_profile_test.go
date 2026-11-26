//go:build (jpeg2000_openjpeg || codecfull) && dcmconv_interop

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/examples/codec-adapters/jpeg2000"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
)

func j2kCLIImage(t *testing.T) (string, []byte) {
	t.Helper()
	input, _ := writeImage(t)
	file, err := object.OpenFile(input)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	const side = 144
	pixels := make([]byte, 2*side*side)
	for f := 0; f < 2; f++ {
		for y := 0; y < side; y++ {
			for x := 0; x < side; x++ {
				pixels[f*side*side+y*side+x] = byte(128 + 60*math.Sin(float64(x+f*13)/17)*math.Cos(float64(y)/23))
			}
		}
	}
	for tag, value := range map[core.Tag]uint16{core.NewTag(0x0028, 0x0010): side, core.NewTag(0x0028, 0x0011): side, core.NewTag(0x0028, 0x0100): 8, core.NewTag(0x0028, 0x0101): 8, core.NewTag(0x0028, 0x0102): 7} {
		file.Dataset.Put(core.NewRawElement(tag, core.VRUS, binary.LittleEndian.AppendUint16(nil, value)))
	}
	file.Dataset.Put(core.NewRawElement(core.NewTag(0x0028, 0x0008), core.VRIS, []byte("2 ")))
	file.Dataset.Put(core.NewRawElement(core.TagPixelData, core.VROB, pixels))
	var data bytes.Buffer
	if err := object.WriteFile(&data, file); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(input, data.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return input, pixels
}

func j2kCLIMarker(t *testing.T, stream []byte, marker byte) []byte {
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

type j2kCLILog struct{ data []byte }

func (w *j2kCLILog) Write(p []byte) (int, error) {
	n := len(p)
	if len(w.data) < 64<<10 {
		keep := min(len(p), (64<<10)-len(w.data))
		w.data = append(w.data, p[:keep]...)
	}
	return n, nil
}

func TestConvJPEG2000LosslessTileControlsRealFile(t *testing.T) {
	input, pixels := j2kCLIImage(t)
	before, err := os.ReadFile(input)
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "lossless.dcm")
	code, _, stderr := runConv(t, "-ts", "1.2.840.10008.1.2.4.90", "-j2k-backend", "openjpeg", "-j2k-tile", "64,64", "-j2k-resolutions", "4", "-split", "1025", "-offset-table", "basic", "-generate-icon", "-o", output, input)
	if code != 0 {
		t.Fatalf("lossless tile controls refused real source: code=%d stderr=%s", code, stderr)
	}
	checkJ2KCLIOutput(t, output, pixels, true, 1)
	file, err := object.OpenFile(output)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	pixel, err := pixeldata.Extract(file.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	if len(pixel.Sequence.Fragments) <= 2 || len(pixel.Sequence.OffsetTable) != 8 {
		t.Fatal("split alias/BOT not encoded")
	}
	for _, fragment := range pixel.Sequence.Fragments {
		if len(fragment) > 1024 || len(fragment)%2 != 0 {
			t.Fatal("effective fragment cap/padding not respected")
		}
	}
	icons, ok := file.Dataset.GetSequence(core.NewTag(0x0088, 0x0200))
	if !ok || len(icons) != 1 {
		t.Fatal("generated icon missing")
	}
	after, err := os.ReadFile(input)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("source changed during optional encoding")
	}
}

func TestConvJPEG2000RateLayersAndIndependentReadback(t *testing.T) {
	input, pixels := j2kCLIImage(t)
	output := filepath.Join(t.TempDir(), "lossy.dcm")
	code, _, stderr := runConv(t, "-ts", "1.2.840.10008.1.2.4.91", "-j2k-backend", "openjpeg", "-allow-lossy", "-j2k-rate", "20,5", "-j2k-tile", "64,64", "-j2k-resolutions", "4", "-root-uid", "1.2.826.0.1.3680043.10.1352", "-o", output, input)
	if code != 0 {
		t.Fatalf("rate controls=%d %s", code, stderr)
	}
	checkJ2KCLIOutput(t, output, pixels, false, 2)
	file, err := object.OpenFile(output)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	method, _ := file.Dataset.GetString(core.NewTag(0x0028, 0x2114))
	lossy, _ := file.Dataset.GetString(core.NewTag(0x0028, 0x2110))
	uid, _ := file.Dataset.GetString(core.NewTag(0x0008, 0x0018))
	if method != "ISO_15444_1" || lossy != "01" || !strings.HasPrefix(uid, "1.2.826.0.1.3680043.10.1352.") {
		t.Fatalf("lossy metadata/root wrong: method=%s lossy=%s uid=%s", method, lossy, uid)
	}
}

func checkJ2KCLIOutput(t *testing.T, path string, want []byte, lossless bool, layers uint16) {
	t.Helper()
	decompress := os.Getenv("DICOM_GO_OPENJPEG_DECOMPRESS")
	if decompress == "" {
		t.Fatal("dcmconv_interop requires explicit DICOM_GO_OPENJPEG_DECOMPRESS")
	}
	registry := pixeldata.NewMemoryRegistry()
	if err := jpeg2000.Register(registry); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	reader, err := pixeldata.OpenFrameReader(ctx, path, pixeldata.FrameReaderOptions{Registry: registry})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	file, err := object.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	pixel, err := pixeldata.Extract(file.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	// Every frame's SOC establishes a boundary, including fragmented output;
	// strip only the legal final even-fragment padding after EOC.
	joined := bytes.Join(pixel.Sequence.Fragments, nil)
	var streams [][]byte
	for len(joined) > 0 {
		pos := bytes.Index(joined, []byte{0xff, 0x4f})
		if pos < 0 {
			t.Fatal("frame SOC missing")
		}
		joined = joined[pos:]
		end := bytes.Index(joined, []byte{0xff, 0xd9})
		if end < 0 {
			t.Fatal("frame EOC missing")
		}
		streams = append(streams, joined[:end+2])
		joined = joined[end+2:]
		if len(joined) > 0 && joined[0] == 0 {
			joined = joined[1:]
		}
	}
	if len(streams) != 2 {
		t.Fatalf("codestream frames=%d", len(streams))
	}
	var squared float64
	for f, stream := range streams {
		siz, cod := j2kCLIMarker(t, stream, 0x51), j2kCLIMarker(t, stream, 0x52)
		transform := byte(0)
		if lossless {
			transform = 1
		}
		if len(siz) < 36 || binary.BigEndian.Uint32(siz[18:]) != 64 || binary.BigEndian.Uint32(siz[22:]) != 64 || len(cod) < 10 || cod[5] != 3 || cod[9] != transform || binary.BigEndian.Uint16(cod[2:]) != layers {
			t.Fatalf("SIZ/COD controls absent: %x %x", siz, cod)
		}
		dir := t.TempDir()
		in, out := filepath.Join(dir, "frame.j2k"), filepath.Join(dir, "frame.rawl")
		if err := os.WriteFile(in, stream, 0600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(ctx, decompress, "-i", in, "-o", out)
		var log j2kCLILog
		cmd.Stdout = &log
		cmd.Stderr = &log
		if err := cmd.Run(); err != nil {
			t.Fatalf("independent decode=%v %s", err, log.data)
		}
		raw, err := os.ReadFile(out)
		if err != nil || len(raw) != 144*144 {
			t.Fatalf("independent frame bytes=%d %v", len(raw), err)
		}
		coreFrame, _, err := reader.Frame(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		for i, v := range raw {
			cross := int(v) - int(coreFrame[i])
			if cross < -1 || cross > 1 {
				t.Fatalf("independent/core disagree at%d: %d", i, cross)
			}
			delta := int(v) - int(want[f*144*144+i])
			if lossless && delta != 0 {
				t.Fatalf("lossless pixels differ at%d delta%d", i, delta)
			}
			squared += float64(delta * delta)
		}
	}
	if !lossless && squared > 0 {
		psnr := 10 * math.Log10(255*255/(squared/float64(len(want))))
		if psnr < 25 {
			t.Fatalf("lossy PSNR %.2f dB", psnr)
		}
		t.Log(fmt.Sprintf("rate controls encoded with %d layers; PSNR %.2f dB (no exact byte-rate claim)", layers, psnr))
	}
}

func TestConvJPEG2000SameSyntaxCodingControlsReencode(t *testing.T) {
	for _, lossless := range []bool{true, false} {
		name, syntax := "lossless", "1.2.840.10008.1.2.4.90"
		if !lossless {
			name, syntax = "lossy", "1.2.840.10008.1.2.4.91"
		}
		t.Run(name, func(t *testing.T) {
			input, pixels := j2kCLIImage(t)
			encoded := filepath.Join(t.TempDir(), "encoded.dcm")
			args := []string{"-ts", syntax, "-j2k-backend", "openjpeg", "-j2k-tile", "128,128", "-j2k-resolutions", "3"}
			if !lossless {
				args = append(args, "-allow-lossy", "-j2k-rate", "20,5")
			}
			args = append(args, "-o", encoded, input)
			if code, _, err := runConv(t, args...); code != 0 {
				t.Fatalf("initial encode %d %s", code, err)
			}
			beforeBytes, err := os.ReadFile(encoded)
			if err != nil {
				t.Fatal(err)
			}
			before, err := object.OpenFile(encoded)
			if err != nil {
				t.Fatal(err)
			}
			oldUID, _ := before.Dataset.GetString(core.NewTag(0x0008, 0x0018))
			oldHistory, _ := before.Dataset.GetStrings(core.NewTag(0x0028, 0x2112))
			before.Close()
			output := filepath.Join(t.TempDir(), "reencoded.dcm")
			args = []string{"-ts", syntax, "-j2k-backend", "openjpeg", "-j2k-tile", "64,64", "-j2k-resolutions", "4"}
			if !lossless {
				args = append(args, "-allow-lossy", "-j2k-rate", "10,3", "-root-uid", "1.2.826.0.1.3680043.10.1352")
			}
			args = append(args, "-o", output, encoded)
			if code, _, err := runConv(t, args...); code != 0 {
				t.Fatalf("same syntax control %d %s", code, err)
			}
			// Different SIZ/COD metadata proves actual encoding, not an equivalent copy.
			layers := uint16(1)
			if !lossless {
				layers = 2
			}
			checkJ2KCLIOutput(t, output, pixels, lossless, layers)
			afterBytes, err := os.ReadFile(encoded)
			if err != nil || !bytes.Equal(beforeBytes, afterBytes) {
				t.Fatal("compressed source mutated")
			}
			after, err := object.OpenFile(output)
			if err != nil {
				t.Fatal(err)
			}
			defer after.Close()
			if !lossless {
				uid, _ := after.Dataset.GetString(core.NewTag(0x0008, 0x0018))
				history, _ := after.Dataset.GetStrings(core.NewTag(0x0028, 0x2112))
				if uid == oldUID || !strings.HasPrefix(uid, "1.2.826.0.1.3680043.10.1352.") || len(history) != len(oldHistory)+1 {
					t.Fatal("lossy generation UID/history not advanced")
				}
			}
		})
	}
}

func TestConvJPEG2000InvalidNamedOptionBeforeRuntimeProbe(t *testing.T) {
	for _, flag := range []string{"-recover", "-rewrite"} {
		code, _, stderr := runConv(t, "-ts", "1.2.840.10008.1.2.4.90", "-j2k-backend", "openjpeg", "-j2k-executable", filepath.Join(t.TempDir(), "missing-runtime"), flag, "bogus", "-o", filepath.Join(t.TempDir(), "out.dcm"), "missing-input.dcm")
		want := "unknown recovery"
		if flag == "-rewrite" {
			want = "unknown rewrite"
		}
		if code != 2 || !strings.Contains(stderr, want) {
			t.Fatalf("invalid named option reached codec probe: code=%d stderr=%s", code, stderr)
		}
	}
}
