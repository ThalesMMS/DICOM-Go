package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func oracleFindPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return p
}
func oracleWorklistRecord(n int) *object.Object {
	text := func(g, e uint16, vr core.VR, v string) core.Element {
		return core.Element{Header: core.ElementHeader{Tag: core.NewTag(g, e), VR: vr}, Value: core.StringValue{v}}
	}
	step := core.DataSet{Elements: []core.Element{text(0x0008, 0x0060, core.VRCS, "CT"), text(0x0040, 0x0001, core.VRAE, "SYN_MODALITY"), text(0x0040, 0x0002, core.VRDA, "20261009"), text(0x0040, 0x0003, core.VRTM, "093000"), text(0x0040, 0x0006, core.VRPN, ""), text(0x0040, 0x0007, core.VRLO, "SYNTHETIC PROCEDURE"), text(0x0040, 0x0009, core.VRSH, fmt.Sprintf("SPS-%d", n))}}
	return object.FromElements([]core.Element{text(0x0010, 0x0010, core.VRPN, fmt.Sprintf("SYNTHETIC^%d", n)), text(0x0010, 0x0020, core.VRLO, fmt.Sprintf("SYN-%d", n)), text(0x0010, 0x0030, core.VRDA, "20000101"), text(0x0010, 0x0040, core.VRCS, "O"), text(0x0008, 0x0050, core.VRSH, fmt.Sprintf("ACC-%d", n)), text(0x0020, 0x000d, core.VRUI, fmt.Sprintf("1.2.826.0.1.3680043.10.1355.8.%d", n)), text(0x0032, 0x1060, core.VRLO, "SYNTHETIC PROCEDURE"), text(0x0040, 0x1001, core.VRSH, fmt.Sprintf("RP-%d", n)), {Header: core.ElementHeader{Tag: core.NewTag(0x0040, 0x0100), VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{step}}}}, std.Dictionary)
}

// Real wlmscpfs with complete synthetic records. No directory policy from
// another owner, external PACS, clinical payload or copied upstream fixture.
func TestFindWorklistDCMTKKeysQueryFileCancelAndExtract(t *testing.T) {
	wlm := dcmtkoracle.Tool(t, "wlmscpfs")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	root := t.TempDir()
	db := filepath.Join(root, "db", "OFFIS")
	if err := os.MkdirAll(db, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(db, "lockfile"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= 3; n++ {
		var b bytes.Buffer
		if err := object.WriteDataSet(&b, oracleWorklistRecord(n), transfer.ExplicitVRLittleEndian); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(db, fmt.Sprintf("synthetic-%d.wl", n)), b.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
	port := oracleFindPort(t)
	cmd := dcmtkoracle.Command(ctx, t, dcmtkoracle.Protocol, wlm, "-s", "-dfp", filepath.Join(root, "db"), strconv.Itoa(port))
	var log bytes.Buffer
	cmd.Stdout, cmd.Stderr = &log, &log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Logf("wlmscpfs started pid=%d single-process", cmd.Process.Pid)
	t.Cleanup(func() {
		cmd.Process.Kill()
		waitErr := cmd.Wait()
		t.Logf("wlmscpfs direct process reaped pid=%d state=%v wait=%v\n%s", cmd.Process.Pid, cmd.ProcessState, waitErr, log.String())
	})
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", address, 50*time.Millisecond)
		if err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("DCMTK worklist did not listen")
		}
		time.Sleep(20 * time.Millisecond)
	}
	file := filepath.Join(root, "worklist-query.json")
	query := `{"00100010":{"vr":"PN","Value":[{"Alphabetic":"SYNTHETIC*"}]},"00100020":{"vr":"LO"},"00400100":{"vr":"SQ","Value":[{"00080060":{"vr":"CS","Value":["CT"]},"00400009":{"vr":"SH"}}]}}`
	if err := os.WriteFile(file, []byte(query), 0600); err != nil {
		t.Fatal(err)
	}
	base := []string{"-remote", address, "-called", "OFFIS", "-calling", "FINDSCU", "-worklist", "-output", "jsonl", "-timeout", "5s", "-k", "ScheduledProcedureStepSequence[0].ScheduledStationAETitle=SYN_MODALITY"}
	var stdout, stderr bytes.Buffer
	dir := filepath.Join(root, "extracted")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	args := append(append([]string{}, base...), "-extract", "-extract-dir", dir, file)
	if code := runWithContext(ctx, args, &stdout, &stderr); code != 0 {
		t.Fatalf("MWL exit=%d stderr=%s", code, stderr.String())
	}
	lines := bytes.Split(bytes.TrimSpace(stdout.Bytes()), []byte("\n"))
	if len(lines) != 3 {
		t.Fatalf("matches=%d stdout=%s stderr=%s", len(lines), stdout.String(), stderr.String())
	}
	for _, line := range lines {
		if !json.Valid(line) {
			t.Fatal("invalid JSONL response")
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 3 {
		t.Fatalf("extracted files=%d", len(entries))
	}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		id, err := object.ReadDataSet(bytes.NewReader(data), transfer.ExplicitVRLittleEndian)
		if err != nil {
			t.Fatal(err)
		}
		if id.Has(core.NewTag(0x0008, 0x0052)) || id.Has(core.NewTag(0x0008, 0x0018)) {
			t.Fatal("QR/SOP identity fabricated in worklist extract")
		}
		if got, _ := id.GetString(core.NewTag(0x0010, 0x0020)); got == "" {
			t.Fatal("PatientID missing from extract")
		}
	}
	stdout.Reset()
	stderr.Reset()
	args = append(append([]string{}, base...), "-cancel-after", "1", file)
	if code := runWithContext(ctx, args, &stdout, &stderr); code != 0 {
		t.Fatalf("MWL intentional cancel exit=%d stderr=%s", code, stderr.String())
	}
	if bytes.Count(stdout.Bytes(), []byte("\n")) != 1 {
		t.Fatalf("canceled delivery count stdout=%s", stdout.String())
	}
	t.Logf("MWL count-cancel stderr:\n%s", stderr.String())
}
