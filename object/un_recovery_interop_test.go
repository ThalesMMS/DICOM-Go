package object_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
)

// TestUNRecoveryIndependentOracles checks the recovered interpretation of
// the UN fixture against DCMTK dcmdump +uc (Little and Big Endian) and, with
// DICOMGO_PYTHON, pydicom 3.0.2 (Little Endian; pydicom cannot read an
// undefined-length UN sequence in a Big Endian file). Enable with
// DICOMGO_DCMTK_INTEGRATION=1 and dcmdump on PATH; without the gate the test
// skips and qualifies nothing.
func TestUNRecoveryIndependentOracles(t *testing.T) {
	dcmdump := dcmtkoracle.Tool(t, "dcmdump")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	dir := t.TempDir()
	for _, syntax := range unSyntaxes() {
		meta, err := dicomtest.Part10File(syntax)
		if err != nil {
			t.Fatal(err)
		}
		name := "le"
		if syntax.ByteOrder.String() == "BigEndian" {
			name = "be"
		}
		path := filepath.Join(dir, name+".dcm")
		if err := os.WriteFile(path, append(meta, unFixture(syntax.ByteOrder, true)...), 0o600); err != nil {
			t.Fatal(err)
		}
		obj, _ := readUNFixture(t, syntax, true)
		recovered, _, err := obj.RecoverUN(ctx, object.UNRecoveryOptions{})
		if err != nil {
			t.Fatal(err)
		}
		series, _ := recovered.GetSequence(core.NewTag(0x0008, 0x1115))
		images, _ := recovered.GetSequence(core.NewTag(0x0008, 0x1140))
		modality, _ := recovered.GetString(core.NewTag(0x0008, 0x0060))
		frame, _ := images[0].GetInt(core.NewTag(0x0008, 0x1160))
		graphic, _ := recovered.GetRaw(core.NewTag(0x0070, 0x0022))
		want := []string{
			fmt.Sprintf("(0008,0060) CS [%s]", modality),
			fmt.Sprintf("(0028,0010) US %d", uint16In(t, recovered, core.NewTag(0x0028, 0x0010))),
			fmt.Sprintf("(0028,0103) US %d", uint16In(t, recovered, core.NewTag(0x0028, 0x0103))),
			// DCMTK prints the unresolved US or SS pattern; Go resolves SS.
			fmt.Sprintf("(0028,0106) xs %d", uint16In(t, recovered, core.NewTag(0x0028, 0x0106))),
			fmt.Sprintf("(0028,0011) US %d", uint16In(t, series[0], core.NewTag(0x0028, 0x0011))),
			fmt.Sprintf("(0008,1160) IS [%d]", frame),
			"(0070,0022) FL 0\\1\\2",
			fmt.Sprintf("# %d,%d GraphicData", len(graphic), len(graphic)/4),
			"(0008,1140) SQ (Sequence with explicit length #=2)",
			"(0008,1115) SQ (Sequence with undefined length #=1)",
			"(0031,1001) UN 08\\00",
		}
		output, err := dcmtkoracle.Command(ctx, t, "recovered VRs and values match dcmdump +uc (xs left unresolved by DCMTK)", dcmdump, "+uc", path).CombinedOutput()
		if err != nil {
			t.Fatalf("dcmdump %s: %v\n%s", name, err, output)
		}
		for _, line := range want {
			if !bytes.Contains(output, []byte(line)) {
				t.Fatalf("%s: DCMTK disagrees on %q:\n%s", name, line, output)
			}
		}
		if name != "le" {
			continue
		}
		python := strings.TrimSpace(os.Getenv("DICOMGO_PYTHON"))
		if python == "" {
			continue
		}
		script := filepath.Join(dir, "un_oracle.py")
		if err := os.WriteFile(script, []byte(unRecoveryPydicomScript), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.CommandContext(ctx, python, script, path).Output()
		if err != nil {
			t.Fatalf("pydicom: %v\n%s", err, out)
		}
		var got map[string]any
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("pydicom output: %v\n%s", err, out)
		}
		smallest := int16(uint16In(t, recovered, core.NewTag(0x0028, 0x0106)))
		uid, _ := images[1].GetString(core.NewTag(0x0008, 0x1150))
		seriesUID, _ := series[0].GetString(core.NewTag(0x0020, 0x000E))
		for key, value := range map[string]any{
			"modality": modality,
			"rows":     float64(uint16In(t, recovered, core.NewTag(0x0028, 0x0010))),
			"smallest": float64(smallest),
			"frame":    fmt.Sprint(frame),
			"uid":      uid,
			"series":   seriesUID,
			"columns":  float64(uint16In(t, series[0], core.NewTag(0x0028, 0x0011))),
		} {
			if got[key] != value {
				t.Fatalf("pydicom %s = %v, Go %v", key, got[key], value)
			}
		}
	}
}

const unRecoveryPydicomScript = `import json, sys, warnings
import pydicom
pydicom.config.convert_wrong_length_to_UN = True
warnings.simplefilter("ignore")
ds = pydicom.dcmread(sys.argv[1])
print(json.dumps({
    "modality": ds.Modality,
    "rows": ds.Rows,
    "smallest": ds.SmallestImagePixelValue,
    "frame": str(ds.ReferencedImageSequence[0].ReferencedFrameNumber),
    "uid": ds.ReferencedImageSequence[1].ReferencedSOPClassUID,
    "series": ds.ReferencedSeriesSequence[0].SeriesInstanceUID,
    "columns": ds.ReferencedSeriesSequence[0].Columns,
}))
`
