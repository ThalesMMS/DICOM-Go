package dcmdump

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/transfer"

	"github.com/ThalesMMS/dicom-go/internal/privatefs"
)

func dsExport(t *testing.T, input []byte, flags []string, want [][]byte, coordinates string) exportManifest {
	t.Helper()
	source := dsPath(t, input)
	dir := t.TempDir()
	args := append(append([]string{"-write-values", dir}, flags...), source)
	code, _, stderr := dsRun(t, args...)
	if code != 0 {
		t.Fatalf("export code=%d stderr=%s", code, stderr)
	}
	var manifest exportManifest
	data, err := os.ReadFile(filepath.Join(dir, "values.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Values) != len(want) {
		t.Fatalf("export count=%d", len(manifest.Values))
	}
	for i, e := range manifest.Values {
		v, err := os.ReadFile(filepath.Join(dir, e.Filename))
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(v)
		if !bytes.Equal(v, want[i]) || e.Bytes != int64(len(v)) || e.SHA256 != hex.EncodeToString(hash[:]) || e.Coordinates != coordinates {
			t.Fatalf("export %d span/hash/coordinates mismatch", i)
		}
		_, err = os.Stat(filepath.Join(dir, e.Filename))
		if err != nil {
			t.Fatal(err)
		}
		if privatefs.Check(filepath.Join(dir, e.Filename)) != nil {
			t.Fatal("export permissions widened")
		}
	}
	after, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(after, input) {
		t.Fatal("export changed source")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != len(want)+1 {
		t.Fatal("temporary or extra output")
	}
	return manifest
}

func TestDumpExportOriginalNativeValueBytes(t *testing.T) {
	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ExplicitVRBigEndian, transfer.ImplicitVRLittleEndian} {
		t.Run(syntax.Name, func(t *testing.T) {
			v := []byte{0x12, 0x34}
			body := dicomtest.EncodeElements(syntax, core.NewRawElement(core.NewTag(0x0028, 0x0010), core.VRUS, v),
				dicomtest.NewSequenceElement(core.NewTag(0x0008, 0x1140), core.DataSet{Elements: []core.Element{core.NewRawElement(core.NewTag(0x0028, 0x0010), core.VRUS, []byte{0x56, 0x78})}}))
			dsExport(t, dsPart10(syntax, body), []string{"-search-tag", "Rows"}, [][]byte{v, {0x56, 0x78}}, "physical")
		})
	}
	body := dsLong(transfer.ExplicitVRLittleEndian, 0x0008, 0x1140, "SQ", 0xffffffff, nil)
	body = append(body, dsControl(binary.BigEndian, 0xe000, 0xffffffff)...)
	body = append(body, dsShort(transfer.ExplicitVRBigEndian, 0x0028, 0x0010, "US", []byte{1, 2})...)
	body = append(body, dsControl(binary.BigEndian, 0xe00d, 0)...)
	body = append(body, dsControl(binary.BigEndian, 0xe0dd, 0)...)
	dsExport(t, dsPart10(transfer.ExplicitVRLittleEndian, body), []string{"-read-recover-delimiter-anomalies", "-search-path", "ReferencedImageSequence[0].Rows"}, [][]byte{{1, 2}}, "physical")
}

func TestDumpExportEncapsulatedFramingAndTextPadding(t *testing.T) {
	pixel := dicomtest.NewFragmentSequenceElement(core.TagPixelData, []byte{0, 0, 0, 0}, []byte{1, 2}, []byte{3, 4, 5, 6})
	encoded := dicomtest.EncodeElements(transfer.RLELossless, pixel)
	dsExport(t, dsPart10(transfer.RLELossless, encoded), []string{"-search-tag", "PixelData"}, [][]byte{encoded[12:]}, "physical")
	dsExport(t, dsPart10(transfer.ExplicitVRLittleEndian, dsShort(transfer.ExplicitVRLittleEndian, 0x0010, 0x0020, "LO", []byte("ABC "))), []string{"-search-tag", "PatientID"}, [][]byte{[]byte("ABC ")}, "physical")
}

func TestDumpExportRecoveredRawDataSet(t *testing.T) {
	body := dicomtest.EncodeElements(transfer.ExplicitVRLittleEndian, dicomtest.MinimalDataset()...)
	dsExport(t, body, []string{"-recover-transfer-syntax", "-search-tag", "PatientID"}, [][]byte{[]byte("TESTID001 ")}, "physical")
}

func TestDumpExportDeflateSpoolAndChecksum(t *testing.T) {
	body := dsLong(transfer.ExplicitVRLittleEndian, 0x7777, 0x1001, "OB", 262144, bytes.Repeat([]byte{1, 2}, 131072))
	for _, wrapped := range []bool{false, true} {
		t.Run(map[bool]string{false: "raw", true: "zlib"}[wrapped], func(t *testing.T) {
			var compressed bytes.Buffer
			if wrapped {
				w := zlib.NewWriter(&compressed)
				_, _ = w.Write(body)
				if err := w.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				w, err := flate.NewWriter(&compressed, flate.DefaultCompression)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = w.Write(body)
				if err := w.Close(); err != nil {
					t.Fatal(err)
				}
			}
			flags := []string{"-search-tag", "(7777,1001)", "-read-max-element-bytes", "524288"}
			if wrapped {
				flags = append(flags, "-read-allow-zlib-deflate")
			}
			m := dsExport(t, dsPart10(transfer.DeflatedExplicitVRLittleEndian, compressed.Bytes()), flags, [][]byte{body[12:]}, "inflated-dataset")
			if m.ContainerSyntax != transfer.DeflatedExplicitVRLittleEndian.UID {
				t.Fatal("compressed source syntax lost")
			}
			bad := append([]byte(nil), compressed.Bytes()...)
			if wrapped {
				bad[len(bad)-1] ^= 1
				dir := t.TempDir()
				code, _, _ := dsRun(t, append(append([]string{"-write-values", dir}, flags...), dsPath(t, dsPart10(transfer.DeflatedExplicitVRLittleEndian, bad)))...)
				entries, _ := os.ReadDir(dir)
				if code != 1 || len(entries) != 0 {
					t.Fatal("bad checksum published output")
				}
			}
			dir := t.TempDir()
			code, _, _ := dsRun(t, append(append([]string{"-write-values", dir, "-read-max-total-bytes", "1000"}, flags...), dsPath(t, dsPart10(transfer.DeflatedExplicitVRLittleEndian, compressed.Bytes())))...)
			entries, _ := os.ReadDir(dir)
			if code != 1 || len(entries) != 0 {
				t.Fatal("inflation limit published output")
			}
		})
	}
}

func TestDumpExportRefusalsBeforePublication(t *testing.T) {
	for _, c := range []struct {
		name  string
		body  []byte
		flags []string
	}{
		{"bytes", dsShort(transfer.ExplicitVRLittleEndian, 0x0010, 0x0010, "PN", []byte("ABCD")), []string{"-search-tag", "PatientName", "-max-export-bytes", "3"}},
		{"count", dsNested()[132+len(dicomtest.NewFileMetaBuilder().Encode()):], []string{"-search-tag", "PatientName", "-max-export-files", "2"}},
		{"sequence", dsNested()[132+len(dicomtest.NewFileMetaBuilder().Encode()):], []string{"-search-tag", "ReferencedImageSequence"}},
		{"duplicate", append(dsShort(transfer.ExplicitVRLittleEndian, 0x0010, 0x0010, "PN", []byte("ABCD")), dsShort(transfer.ExplicitVRLittleEndian, 0x0010, 0x0010, "PN", []byte("EFGH"))...), []string{"-search-tag", "PatientName"}},
		{"truncated", dsLong(transfer.ExplicitVRLittleEndian, 0x7777, 0x1001, "OB", 8, []byte{1, 2}), []string{"-search-tag", "(7777,1001)"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			code, _, _ := dsRun(t, append(append([]string{"-write-values", dir}, c.flags...), dsPath(t, dsPart10(transfer.ExplicitVRLittleEndian, c.body)))...)
			entries, _ := os.ReadDir(dir)
			if code != 1 || len(entries) != 0 {
				t.Fatalf("code=%d outputs=%d", code, len(entries))
			}
		})
	}
	source := dsPath(t, dsPart10(transfer.ExplicitVRLittleEndian, dsShort(transfer.ExplicitVRLittleEndian, 0x0010, 0x0010, "PN", []byte("ABCD"))))
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "values.json"), []byte("KEEP"), 0600); err != nil {
		t.Fatal(err)
	}
	code, _, _ := dsRun(t, "-write-values", dir, "-search-tag", "PatientName", source)
	entries, _ := os.ReadDir(dir)
	kept, _ := os.ReadFile(filepath.Join(dir, "values.json"))
	if code != 1 || len(entries) != 1 || string(kept) != "KEEP" {
		t.Fatal("existing destination changed")
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	code, _, _ = dsRun(t, "-write-values", link, "-search-tag", "PatientName", source)
	if code != 1 {
		t.Fatal("symlink export directory accepted")
	}
}
