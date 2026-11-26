package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
)

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	return port
}

func qrscpPort(t *testing.T, address string) string {
	t.Helper()
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// TestPynetdicomRequestorAgainstQRSCP: pynetdicom queries, gets and moves
// from qrscp. Opt-in with DICOMGO_PYNETDICOM_INTEGRATION=1 and
// DICOMGO_PYTHON (pydicom 3.0.2, pynetdicom 3.0.4).
func TestPynetdicomRequestorAgainstQRSCP(t *testing.T) {
	if os.Getenv("DICOMGO_PYNETDICOM_INTEGRATION") == "" {
		t.Skip("opt-in: DICOMGO_PYNETDICOM_INTEGRATION=1 and DICOMGO_PYTHON (pydicom 3.0.2, pynetdicom 3.0.4)")
	}
	python := os.Getenv("DICOMGO_PYTHON")
	if python == "" {
		python = "python3"
	}
	movePort := freePort(t)
	address, _ := startQRSCP(t, writeArchive(t), "-move-destination", "PYDEST=127.0.0.1:"+strconv.Itoa(movePort))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, python, "testdata/qr_peer.py", qrscpPort(t, address), strconv.Itoa(movePort), studyA, studyB).CombinedOutput()
	if err != nil {
		t.Fatalf("peer: %v\n%s", err, output)
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	var summary struct {
		StudyMatches   []string `json:"study_matches"`
		StudyInstances []string `json:"study_instances"`
		StudyFinal     int      `json:"study_final"`
		Patients       []string `json:"patients"`
		GetFinal       int      `json:"get_final"`
		GetReceived    []string `json:"get_received"`
		MoveFinal      int      `json:"move_final"`
		MoveReceived   []string `json:"move_received"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &summary); err != nil {
		t.Fatalf("summary: %v\n%s", err, output)
	}
	if strings.Join(summary.StudyMatches, ",") != studyA+","+studyB || strings.Join(summary.StudyInstances, ",") != "1,3" || summary.StudyFinal != 0 {
		t.Errorf("study find: %+v", summary)
	}
	if strings.Join(summary.Patients, ",") != "SYNTH-A,SYNTH-B" {
		t.Errorf("patients: %v", summary.Patients)
	}
	if summary.GetFinal != 0 || strings.Join(summary.GetReceived, ",") != seriesA+".1,"+seriesA+".2,"+srA+".1" {
		t.Errorf("C-GET: final 0x%04X received %v", summary.GetFinal, summary.GetReceived)
	}
	if summary.MoveFinal != 0 || strings.Join(summary.MoveReceived, ",") != seriesB+".1" {
		t.Errorf("C-MOVE: final 0x%04X received %v", summary.MoveFinal, summary.MoveReceived)
	}
}

func countFiles(t *testing.T, dir, prefix string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), prefix) {
			n++
		}
	}
	return n
}

// TestDCMTKRequestorsAgainstQRSCP: DCMTK findscu, getscu and movescu
// against qrscp. Enable with DICOMGO_DCMTK_INTEGRATION=1 and the tools on
// PATH or in DICOMGO_DCMTK_BIN.
func TestDCMTKRequestorsAgainstQRSCP(t *testing.T) {
	findscu := dcmtkoracle.Tool(t, "findscu")
	getscu := dcmtkoracle.Tool(t, "getscu")
	movescu := dcmtkoracle.Tool(t, "movescu")
	movePort := freePort(t)
	address, _ := startQRSCP(t, writeArchive(t), "-move-destination", "MOVESCU=127.0.0.1:"+strconv.Itoa(movePort))
	port := qrscpPort(t, address)
	ctx := context.Background()

	findDir := t.TempDir()
	out, err := dcmtkoracle.Command(ctx, t, dcmtkoracle.Protocol, findscu, "-S", "-aet", "FINDSCU", "-aec", "QRSCP",
		"-k", "QueryRetrieveLevel=STUDY", "-k", "StudyInstanceUID", "-k", "PatientID=SYNTH-*", "-k", "NumberOfStudyRelatedInstances",
		"-X", "-od", findDir, "127.0.0.1", port).CombinedOutput()
	if err != nil {
		t.Fatalf("findscu: %v\n%s", err, out)
	}
	if n := countFiles(t, findDir, "rsp"); n != 2 {
		t.Fatalf("findscu wrote %d responses, want 2\n%s", n, out)
	}

	getDir := t.TempDir()
	out, err = dcmtkoracle.Command(ctx, t, dcmtkoracle.Protocol, getscu, "-S", "-aet", "GETSCU", "-aec", "QRSCP",
		"-k", "QueryRetrieveLevel=STUDY", "-k", "StudyInstanceUID="+studyA, "-od", getDir, "127.0.0.1", port).CombinedOutput()
	if err != nil {
		t.Fatalf("getscu: %v\n%s", err, out)
	}
	if n := countFiles(t, getDir, ""); n != 3 {
		t.Fatalf("getscu stored %d files, want 3\n%s", n, out)
	}

	moveDir := t.TempDir()
	out, err = dcmtkoracle.Command(ctx, t, dcmtkoracle.Protocol, movescu, "-S", "-aet", "MOVESCU", "-aec", "QRSCP", "-aem", "MOVESCU",
		"+P", strconv.Itoa(movePort), "-od", moveDir,
		"-k", "QueryRetrieveLevel=STUDY", "-k", "StudyInstanceUID="+studyB, "127.0.0.1", port).CombinedOutput()
	if err != nil {
		t.Fatalf("movescu: %v\n%s", err, out)
	}
	if n := countFiles(t, moveDir, ""); n != 1 {
		t.Fatalf("movescu stored %d files, want 1\n%s", n, out)
	}
	if matches, _ := filepath.Glob(filepath.Join(moveDir, "*")); len(matches) != 1 {
		t.Fatalf("movescu output %v", matches)
	}
}
