package dimse

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// TestDeflatedStoreIndependentDCMTKPeer is opt-in evidence for #940: DCMTK
// storescp preferring Deflated (+xd) selects it from the StoreSession proposal,
// writes the file, and dcmdump, dicom-go and (with DICOMGO_PYTHON) pydicom
// 3.0.2 read every sample back. Without the gate (DICOMGO_DCMTK_INTEGRATION=1)
// the test skips.
func TestDeflatedStoreIndependentDCMTKPeer(t *testing.T) {
	storescpTool := dcmtkoracle.Tool(t, "storescp")
	dcmdump := dcmtkoracle.Tool(t, "dcmdump")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()
	outDir := t.TempDir()
	storescp := dcmtkoracle.Command(ctx, t, dcmtkoracle.Protocol, storescpTool, "-aet", "DEFLATESCP", "+xd", "-od", outDir, strconv.Itoa(port))
	if err := storescp.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storescp.Process.Kill(); _ = storescp.Wait() })
	time.Sleep(300 * time.Millisecond)

	source := deflatedStoreDataSet()
	wantPixels, _ := source.GetRaw(core.TagPixelData)
	var buf bytes.Buffer
	if err := object.WriteFile(&buf, &object.File{Dataset: source, TransferSyntax: transfer.DeflatedExplicitVRLittleEndian}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "deflated.dcm")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := NewStoreSession(net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), StoreSessionOptions{DialOptions: ul.DialOptions{CallingAETitle: "STORESCU", CalledAETitle: "DEFLATESCP"}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := session.StoreBatch(ctx, []StoreSource{NewPathStoreSource(path)})
	_ = session.Close(ctx)
	if err != nil || len(result.Items) != 1 || result.Items[0].Outcome != StoreOutcomeSuccess {
		t.Fatalf("store to DCMTK: %+v %v", result.Items, err)
	}
	if got := result.Items[0].NegotiatedTransferSyntaxUID; got != transfer.DeflatedExplicitVRLittleEndian.UID {
		t.Fatalf("DCMTK selected %s, want Deflated", got)
	}
	var received string
	for deadline := time.Now().Add(5 * time.Second); received == "" && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		entries, _ := os.ReadDir(outDir)
		for _, entry := range entries {
			received = filepath.Join(outDir, entry.Name())
		}
	}
	if received == "" {
		t.Fatal("DCMTK stored no file")
	}
	time.Sleep(200 * time.Millisecond)
	if out, err := dcmtkoracle.Command(ctx, t, dcmtkoracle.Accepted, dcmdump, "-q", received).CombinedOutput(); err != nil {
		t.Fatalf("dcmdump: %v\n%s", err, out)
	}
	file, err := object.OpenFile(received)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if file.TransferSyntax.UID != transfer.DeflatedExplicitVRLittleEndian.UID {
		t.Fatalf("DCMTK file syntax %s", file.TransferSyntax.UID)
	}
	if pixels, _ := file.Dataset.GetRaw(core.TagPixelData); !bytes.Equal(pixels, wantPixels) {
		t.Fatal("DCMTK file samples differ")
	}
	python := strings.TrimSpace(os.Getenv("DICOMGO_PYTHON"))
	if python == "" {
		t.Log("DICOMGO_PYTHON unset: pydicom portion not executed")
		return
	}
	script := `
import sys
import numpy as np
import pydicom
assert pydicom.__version__ == "3.0.2"
ds = pydicom.dcmread(sys.argv[1])
assert ds.file_meta.TransferSyntaxUID == "1.2.840.10008.1.2.1.99", ds.file_meta.TransferSyntaxUID
want = (np.arange(64 * 64) * 7 % 256).astype(np.uint8).reshape(64, 64)
assert np.array_equal(ds.pixel_array, want)
assert str(ds.PatientName) == "SYNTHETIC^DEFLATE"
print("pydicom Deflated peer file verified")
`
	out, err := exec.CommandContext(ctx, python, "-c", script, received).CombinedOutput()
	if err != nil {
		t.Fatalf("pydicom: %v\n%s", err, out)
	}
	t.Log(strings.TrimSpace(string(out)))
}
