package object_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// TestPydicomCharsetConversionOracle has pydicom 3.0.2 decode the original
// mixed-charset file (ISO_IR 100, ISO 2022 IR 87 and ISO_IR 192 scopes) and
// reopen the UTF-8 conversion; both must give the characters Go expects.
// Enable with DICOM_GO_PYDICOM_CHARSET=1 and DICOM_GO_PYTHON; without the gate
// the test skips and qualifies nothing.
func TestPydicomCharsetConversionOracle(t *testing.T) {
	if os.Getenv("DICOM_GO_PYDICOM_CHARSET") != "1" {
		t.Skip("set DICOM_GO_PYDICOM_CHARSET=1 and DICOM_GO_PYTHON to run the pydicom charset oracle")
	}
	python := strings.TrimSpace(os.Getenv("DICOM_GO_PYTHON"))
	if python == "" {
		t.Fatal("DICOM_GO_PYTHON is required when the pydicom charset oracle is enabled")
	}
	fixture := append([]core.Element{
		text(core.NewTag(0x0008, 0x0016), core.VRUI, "1.2.840.10008.5.1.4.1.1.7"),
		text(core.NewTag(0x0008, 0x0018), core.VRUI, "2.25.941"),
	}, preservingFixture()...)
	source := charsetObject(object.TextOptions{}, fixture...)
	converted, _, err := source.ConvertTextToUTF8(context.Background(), object.CharsetConversionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for name, obj := range map[string]*object.Object{"original": source, "converted": converted} {
		var buf bytes.Buffer
		if err := object.WriteFile(&buf, &object.File{Dataset: obj, TransferSyntax: transfer.ExplicitVRLittleEndian}); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name+".dcm"), buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	script := filepath.Join(dir, "charset_oracle.py")
	if err := os.WriteFile(script, []byte(charsetPydicomScript), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, python, script, dir)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	command.Env = append(os.Environ(), "PYTHONIOENCODING=utf-8")
	out, err := command.Output()
	if err != nil {
		t.Fatalf("pydicom: %v\n%s", err, stderr.Bytes())
	}
	var got map[string][]string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("pydicom output: %v\n%s", err, out)
	}
	want := []string{latinName, latinPlace, latinName, japaneseName, polishName}
	for _, name := range []string{"original", "converted"} {
		if strings.Join(got[name], "|") != strings.Join(want, "|") {
			t.Fatalf("pydicom %s = %q, want %q", name, got[name], want)
		}
	}
	if got["charset"][0] != "ISO_IR 192" {
		t.Fatalf("converted declaration %q", got["charset"])
	}
}

const charsetPydicomScript = `import json, pathlib, sys
import pydicom
root = pathlib.Path(sys.argv[1])
out = {}
for name in ("original", "converted"):
    ds = pydicom.dcmread(root / (name + ".dcm"))
    values = [str(ds.PatientName), str(ds.InstitutionName)]
    values += [str(item.PatientName) for item in ds.ReferencedSeriesSequence]
    out[name] = values
    if name == "converted":
        out["charset"] = [str(ds.SpecificCharacterSet)]
print(json.dumps(out, ensure_ascii=False))
`
