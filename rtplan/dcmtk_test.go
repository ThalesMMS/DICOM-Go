package rtplan

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
)

// TestSyntheticPlansAreReadByDCMTKDRTDump has DCMTK's dcmrt (drtdump, which
// reads the data set through DRTPlanIOD) read the synthetic plans used by
// the tests. dcmrt checks attribute presence per module, not the relations
// between beams, control points and fraction groups that Read validates, so
// the oracle confirms the fixtures are well-formed RT Plans; a control
// without the Beam Number of a beam shows the oracle sees what it checks.
// Enable with DICOMGO_DCMTK_INTEGRATION=1 and drtdump on PATH or in
// DICOMGO_DCMTK_BIN.
func TestSyntheticPlansAreReadByDCMTKDRTDump(t *testing.T) {
	drtdump := dcmtkoracle.Tool(t, "drtdump")
	dir := t.TempDir()
	write := func(name string, fixture *planFixture) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, fixture.bytes(t), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	diagnostics := func(out []byte) []string {
		var lines []string
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, "W:") || strings.HasPrefix(line, "E:") {
				lines = append(lines, line)
			}
		}
		return lines
	}

	plan := write("plan.dcm", newPlanFixture())
	out := dcmtkoracle.Run(t, dcmtkoracle.Accepted, drtdump, "-v", plan)
	if !strings.Contains(string(out), "RT Plan object") {
		t.Fatalf("drtdump did not read an RT Plan:\n%s", out)
	}
	if lines := diagnostics(out); len(lines) > 0 {
		t.Fatalf("drtdump reports diagnostics for the synthetic plan:\n%s", strings.Join(lines, "\n"))
	}

	control := newPlanFixture()
	control.beams[1].attrs.del(tagBeamNumber)
	cmd := dcmtkoracle.Command(context.Background(), t, dcmtkoracle.Accepted, drtdump, "-v", write("control.dcm", control))
	controlOut, _ := cmd.CombinedOutput()
	if len(diagnostics(controlOut)) == 0 && strings.Contains(string(controlOut), "RT Plan object") {
		t.Fatalf("drtdump accepts a beam without Beam Number silently; the oracle would not detect a missing Type 1 attribute:\n%s", controlOut)
	}
	if _, err := Read(control.file(t)); err == nil {
		t.Fatal("Read accepts the control without Beam Number")
	}
}
