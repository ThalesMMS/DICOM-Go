package dimse

import (
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
)

// TestStoreSessionHeterogeneousBatchIndependentPeers is opt-in evidence for
// #944: DCMTK storescp (+xa) receives the CT/MR RLE/RT Dose/RT Plan/RTSTRUCT
// Deflated/SR/ECG batch with an unreadable entry in the middle. dcmdump parses
// every persisted file, dicom-go compares all attributes, items, pixels and
// waveform bytes with the sources, and pydicom 3.0.2 (DICOMGO_PYTHON)
// compares every element of each DCMTK file with its source. Enable with
// DICOMGO_DCMTK_INTEGRATION=1.
func TestStoreSessionHeterogeneousBatchIndependentPeers(t *testing.T) {
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
	storescp := dcmtkoracle.Command(ctx, t, dcmtkoracle.Protocol, storescpTool, "-aet", "BATCHSCP", "+xa", "-od", outDir, strconv.Itoa(port))
	if err := storescp.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storescp.Process.Kill(); _ = storescp.Wait() })
	time.Sleep(300 * time.Millisecond)

	objects := heterogeneousBatch(t)
	sources := make([]StoreSource, len(objects))
	for i, o := range objects {
		sources[i] = NewPathStoreSource(o.path)
	}
	session, err := NewStoreSession(net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), StoreSessionOptions{DialOptions: ul.DialOptions{CallingAETitle: "STORESCU", CalledAETitle: "BATCHSCP"}, ContinueOnError: true})
	if err != nil {
		t.Fatal(err)
	}
	result, _ := session.StoreBatch(ctx, sources)
	_ = session.Close(ctx)
	for i, item := range result.Items {
		if objects[i].file == nil {
			if item.Outcome == StoreOutcomeSuccess {
				t.Fatal("unreadable source reported as stored")
			}
			continue
		}
		if item.Outcome != StoreOutcomeSuccess {
			t.Fatalf("%s: %s %v", objects[i].name, item.Outcome, item.Err)
		}
	}

	// DCMTK names files itself; index them by SOP Instance UID.
	peer := &batchPeer{dir: t.TempDir()}
	received := map[string]string{}
	for deadline := time.Now().Add(5 * time.Second); len(received) < len(objects)-1 && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		entries, _ := os.ReadDir(outDir)
		for _, entry := range entries {
			path := filepath.Join(outDir, entry.Name())
			file, err := object.OpenFile(path)
			if err != nil {
				continue
			}
			if uid, ok := file.Dataset.GetUID(core.NewTag(0x0008, 0x0018)); ok {
				received[uid] = path
			}
			_ = file.Close()
		}
	}
	var manifest strings.Builder
	for _, o := range objects {
		if o.file == nil {
			continue
		}
		path, ok := received[o.uid]
		if !ok {
			t.Fatalf("%s not persisted by DCMTK", o.name)
		}
		if out, err := dcmtkoracle.Command(ctx, t, dcmtkoracle.Accepted, dcmdump, "-q", path).CombinedOutput(); err != nil {
			t.Fatalf("dcmdump %s: %v\n%s", o.name, err, out)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(peer.dir, o.uid+".dcm"), data, 0o600); err != nil {
			t.Fatal(err)
		}
		verifyPersisted(t, peer, o)
		manifest.WriteString(o.path + "\t" + path + "\n")
	}
	python := strings.TrimSpace(os.Getenv("DICOMGO_PYTHON"))
	if python == "" {
		t.Log("DICOMGO_PYTHON unset: pydicom portion not executed")
		return
	}
	pairs := filepath.Join(t.TempDir(), "pairs.tsv")
	if err := os.WriteFile(pairs, []byte(manifest.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `
import sys
import pydicom
assert pydicom.__version__ == "3.0.2"
count = 0
for line in open(sys.argv[1], encoding="utf-8"):
    source_path, received_path = line.rstrip("\n").split("\t")
    source = pydicom.dcmread(source_path)
    received = pydicom.dcmread(received_path)
    for elem in source:
        other = received[elem.tag]
        assert elem.VR == other.VR or {elem.VR, other.VR} <= {"OB", "OW"}, (received_path, elem.tag)
        assert elem.value == other.value, (received_path, elem.tag)
    assert len(source) == len(received), (received_path, len(source), len(received))
    count += 1
print("pydicom compared %d DCMTK-persisted objects with their sources" % count)
`
	out, err := exec.CommandContext(ctx, python, "-c", script, pairs).CombinedOutput()
	if err != nil {
		t.Fatalf("pydicom: %v\n%s", err, out)
	}
	t.Log(strings.TrimSpace(string(out)))
}
