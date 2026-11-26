package retrievecli

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func oracleMovePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return p
}

// Opt-in offline acceptance: actual DCMTK archive, no external PACS or PHI.
func TestRetrieveEmbeddedMoveDCMTK(t *testing.T) {
	qr := dcmtkoracle.Tool(t, "dcmqrscp")
	idx := dcmtkoracle.Tool(t, "dcmqridx")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dir := t.TempDir()
	storage := filepath.Join(dir, "archive")
	if err := os.Mkdir(storage, 0700); err != nil {
		t.Fatal(err)
	}
	study := "1.2.826.0.1.3680043.10.1355.7"
	ds := receiverDataSet(study)
	for _, e := range []core.Element{
		{Header: core.ElementHeader{Tag: core.NewTag(0x0010, 0x0010), VR: core.VRPN}, Value: core.StringValue{"SYNTHETIC^MOVE"}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0010, 0x0020), VR: core.VRLO}, Value: core.StringValue{"SYNTHETIC-1355"}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0020), VR: core.VRDA}, Value: core.StringValue{"20261009"}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0060), VR: core.VRCS}, Value: core.StringValue{"CT"}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0020, 0x0011), VR: core.VRIS}, Value: core.StringValue{"1"}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0020, 0x0013), VR: core.VRIS}, Value: core.StringValue{"1"}},
	} {
		ds.Put(e)
	}
	var instance bytes.Buffer
	if err := object.WriteFile(&instance, &object.File{Dataset: ds, TransferSyntax: transfer.ExplicitVRLittleEndian}); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(storage, "synthetic.dcm")
	if err := os.WriteFile(source, instance.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	index := dcmtkoracle.Command(ctx, t, dcmtkoracle.Accepted, idx, storage, source)
	if out, err := index.CombinedOutput(); err != nil {
		t.Fatalf("index failed: %v\n%s", err, out)
	}
	port, listenPort := oracleMovePort(t), oracleMovePort(t)
	config := filepath.Join(dir, "qr.cfg")
	cfg := fmt.Sprintf("NetworkTCPPort = %d\nMaxPDUSize = 16384\nMaxAssociations = 4\nHostTable BEGIN\nreceiver = (DESTAE, localhost, %d)\nHostTable END\nVendorTable BEGIN\nVendorTable END\nAETable BEGIN\nQRSCP %s RW (10, 64mb) ANY\nAETable END\n", port, listenPort, storage)
	if err := os.WriteFile(config, []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}
	args := append([]string{"-c", config}, dcmtkoracle.QRSCPProcessOptions(ctx, t, qr)...)
	cmd := dcmtkoracle.Command(ctx, t, dcmtkoracle.Protocol, qr, append(args, strconv.Itoa(port))...)
	var log bytes.Buffer
	cmd.Stdout, cmd.Stderr = &log, &log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Logf("dcmqrscp started pid=%d single-process", cmd.Process.Pid)
	t.Cleanup(func() {
		cmd.Process.Kill()
		waitErr := cmd.Wait()
		t.Logf("dcmqrscp direct process reaped pid=%d state=%v wait=%v\n%s", cmd.Process.Pid, cmd.ProcessState, waitErr, log.String())
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
			t.Fatal("DCMTK archive did not listen")
		}
		time.Sleep(20 * time.Millisecond)
	}
	q := object.New(nil)
	q.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0052), VR: core.VRCS}, Value: core.StringValue{"STUDY"}})
	q.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0020, 0x000d), VR: core.VRUI}, Value: core.StringValue{study}})
	var query bytes.Buffer
	if err := object.WriteDataSet(&query, q, transfer.ExplicitVRLittleEndian); err != nil {
		t.Fatal(err)
	}
	queryFile := filepath.Join(dir, "move-query.ds")
	if err := os.WriteFile(queryFile, query.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "received")
	var stdout, stderr bytes.Buffer
	code := RunMoveWithContext(ctx, []string{"-remote", address, "-called", "QRSCP", "-calling", "MOVESCU", "-move-destination", "DESTAE", "-listen-port", strconv.Itoa(listenPort), "-output", out, "-timeout", "10s", "-k", "StudyInstanceUID=" + study, queryFile}, &stdout, &stderr)
	t.Logf("Go movescu stdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	if code != 0 {
		t.Fatalf("movescu exit=%d", code)
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("received files=%v", entries)
	}
	raw, err := os.ReadFile(filepath.Join(out, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	file, err := object.ReadFile(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := file.GetString(core.NewTag(0x0020, 0x000d)); got != study {
		t.Fatal("wrong study")
	}
	if data, _ := file.GetRaw(core.NewTag(0x7fe0, 0x0010)); !bytes.Equal(data, []byte{1, 0, 2, 0}) {
		t.Fatal("received pixels differ")
	}
	if got, _ := file.Meta.GetString(core.NewTag(0x0002, 0x0017)); got != "QRSCP" {
		t.Fatalf("sending AE=%q", got)
	}
}
