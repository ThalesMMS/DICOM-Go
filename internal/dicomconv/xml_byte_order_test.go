package dicomconv

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// TestXMLOfBigEndianFileIsLittleEndian checks that dcm2xml of an Explicit VR
// Big Endian file writes OW and OF values in Little Endian, inline and in
// bulk data files, and that xml2dcm restores the original data set bytes.
func TestXMLOfBigEndianFileIsLittleEndian(t *testing.T) {
	ctx := context.Background()
	little := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	pointCoordinates := core.NewTag(0x0066, 0x0016)
	source := richFile(t)
	source.Dataset.Put(raw(pointCoordinates, core.VROF, little))
	source.TransferSyntax = transfer.ExplicitVRBigEndian
	if err := source.RebuildFileMeta(); err != nil {
		t.Fatal(err)
	}
	file := reread(t, source)
	if file.TransferSyntax.UID != transfer.ExplicitVRBigEndian.UID {
		t.Fatalf("source transfer syntax %s", file.TransferSyntax.UID)
	}
	pixels := le16(0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 4095)
	for _, bulk := range []bool{false, true} {
		dir := t.TempDir()
		opts := EncodeOptions{WithMeta: true, KeepCharacterSet: true}
		if bulk {
			opts.BulkDir, opts.BulkThreshold, opts.BaseDir = filepath.Join(dir, "bulk"), 8, dir
		}
		encoded, err := Encode(ctx, file, XML, opts)
		if err != nil {
			t.Fatalf("bulk=%t: %v", bulk, err)
		}
		if bulk {
			entries, err := os.ReadDir(opts.BulkDir)
			if err != nil {
				t.Fatal(err)
			}
			found := map[string][]byte{}
			for _, entry := range entries {
				data, err := os.ReadFile(filepath.Join(opts.BulkDir, entry.Name()))
				if err != nil {
					t.Fatal(err)
				}
				found[entry.Name()[:8]] = data
			}
			if got := found["7FE00010"]; !bytes.Equal(got, pixels) {
				t.Errorf("Pixel Data bulk file % X, want % X", got, pixels)
			}
			if got := found["00660016"]; !bytes.Equal(got, little) {
				t.Errorf("OF bulk file % X, want % X", got, little)
			}
		} else {
			for _, want := range [][]byte{pixels, little} {
				if inline := "<InlineBinary>" + base64.StdEncoding.EncodeToString(want) + "</InlineBinary>"; !strings.Contains(string(encoded), inline) {
					t.Errorf("no Little Endian %s in the XML", inline)
				}
			}
		}
		back, err := Decode(ctx, encoded, XML, DecodeOptions{BaseDir: dir})
		if err != nil {
			t.Fatalf("bulk=%t: decode: %v", bulk, err)
		}
		if back.TransferSyntax.UID != transfer.ExplicitVRBigEndian.UID {
			t.Fatalf("bulk=%t: transfer syntax %s", bulk, back.TransferSyntax.UID)
		}
		target := filepath.Join(dir, "out.dcm")
		if err := WritePart10(target, back); err != nil {
			t.Fatal(err)
		}
		written, err := ReadPart10(target)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(datasetBytes(t, written), datasetBytes(t, file)) {
			t.Errorf("bulk=%t: data set bytes changed", bulk)
		}
		_ = written.Close()
	}
}
