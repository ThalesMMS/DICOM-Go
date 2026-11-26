package rle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// Explicitly enabled independent decoder/encoder comparison with synthetic
// frames only. An explicitly requested but unusable tool fails rather than skips.
func TestEncoderKernelAgainstGDCM(t *testing.T) {
	tool := os.Getenv("DICOMGO_RLE_GDCM_TOOL")
	if tool == "" {
		t.Skip("set DICOMGO_RLE_GDCM_TOOL to gdcmconv for independent RLE samples")
	}
	tool, err := filepath.Abs(tool)
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(tool)
	if err != nil {
		t.Fatal(err)
	}
	run := func(t *testing.T, args ...string) []byte {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, tool, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("synthetic GDCM oracle %v: %v: %.2048s", args, err, out)
		}
		return out
	}
	t.Logf("oracle %s SHA256=%x version=%.512s", tool, sha256.Sum256(binary), run(t, "--version"))
	opts := oracleRegistries(t)
	for _, c := range oracleCases() {
		t.Run(c.name, func(t *testing.T) {
			pixels := oraclePixels(c)
			encoded, _, err := pixeldata.TranscodeDataSet(context.Background(), c.object(pixels), transfer.ExplicitVRLittleEndian, transfer.RLELossless, opts)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			rlePath, nativePath := filepath.Join(dir, "go-rle.dcm"), filepath.Join(dir, "gdcm-native.dcm")
			writeOracleFile(t, rlePath, encoded, transfer.RLELossless)
			run(t, "--raw", rlePath, nativePath)
			if !bytes.Equal(nativePixels(t, nativePath, c), pixels) {
				t.Fatal("GDCM decoded different native samples")
			}
			writeOracleFile(t, nativePath, c.object(pixels), transfer.ExplicitVRLittleEndian)
			run(t, "--rle", nativePath, rlePath)
			file, err := object.OpenFile(rlePath)
			if err != nil {
				t.Fatal(err)
			}
			native, _, err := pixeldata.TranscodeDataSet(context.Background(), file.Dataset, file.TransferSyntax, transfer.ExplicitVRLittleEndian, opts)
			if err != nil {
				t.Fatal(err)
			}
			goPath := filepath.Join(dir, "go-native.dcm")
			writeOracleFile(t, goPath, native, transfer.ExplicitVRLittleEndian)
			if !bytes.Equal(nativePixels(t, goPath, c), pixels) {
				t.Fatalf("Go decoded different GDCM samples for %s", c.name)
			}
		})
	}
}
