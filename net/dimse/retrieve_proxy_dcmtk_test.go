package dimse

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
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

func freeTCPPort(t *testing.T) int {
	t.Helper()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()
	return port
}

// TestRetrieveProxyDCMTKPeers is opt-in evidence for #1171 with DCMTK on
// both sides of the proxy: dcmqrscp is the upstream (its C-FIND does not
// return SOP Class UID, so the classes come from the fetched instances),
// movescu the C-MOVE requester and its own Move Destination, getscu the
// C-GET requester. Enable with DICOMGO_DCMTK_INTEGRATION=1 and the DCMTK
// tools in DICOMGO_DCMTK_BIN.
func TestRetrieveProxyDCMTKPeers(t *testing.T) {
	dcmqrscp := dcmtkoracle.Tool(t, "dcmqrscp")
	dcmqridx := dcmtkoracle.Tool(t, "dcmqridx")
	movescu := dcmtkoracle.Tool(t, "movescu")
	getscu := dcmtkoracle.Tool(t, "getscu")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	dir := t.TempDir()
	storage := filepath.Join(dir, "QRSCP")
	if err := os.Mkdir(storage, 0o700); err != nil {
		t.Fatal(err)
	}
	archive := newProxyTestArchive()
	ct := archive.instances[:3]
	for i, instance := range ct {
		dataset := proxyTestDataSet(instance)
		for _, element := range []core.Element{
			{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0020), VR: core.VRDA}, Value: core.StringValue{"20261007"}},
			{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0060), VR: core.VRCS}, Value: core.StringValue{"CT"}},
			{Header: core.ElementHeader{Tag: core.NewTag(0x0010, 0x0010), VR: core.VRPN}, Value: core.StringValue{"SYNTHETIC^PROXY"}},
			{Header: core.ElementHeader{Tag: core.NewTag(0x0020, 0x0013), VR: core.VRIS}, Value: core.StringValue{strconv.Itoa(i + 1)}},
		} {
			dataset.Put(element)
		}
		var buf bytes.Buffer
		if err := object.WriteFile(&buf, &object.File{Dataset: dataset, TransferSyntax: transfer.ExplicitVRLittleEndian}); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(storage, fmt.Sprintf("ct%d.dcm", i))
		if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		dcmtkoracle.Run(t, dcmtkoracle.Accepted, dcmqridx, storage, path)
	}

	qrPort := freeTCPPort(t)
	config := filepath.Join(dir, "dcmqrscp.cfg")
	if err := os.WriteFile(config, []byte(fmt.Sprintf(`NetworkTCPPort = %d
MaxPDUSize = 16384
MaxAssociations = 4

HostTable BEGIN
HostTable END

VendorTable BEGIN
VendorTable END

AETable BEGIN
QRSCP %s RW (10, 64mb) ANY
AETable END
`, qrPort, storage)), 0o600); err != nil {
		t.Fatal(err)
	}
	serverArgs := append([]string{"-c", config}, dcmtkoracle.QRSCPProcessOptions(ctx, t, dcmqrscp)...)
	server := dcmtkoracle.Command(ctx, t, dcmtkoracle.Protocol, dcmqrscp, append(serverArgs, strconv.Itoa(qrPort))...)
	var serverLog bytes.Buffer
	server.Stdout, server.Stderr = &serverLog, &serverLog
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = server.Process.Kill()
		_ = server.Wait()
		if t.Failed() {
			t.Logf("dcmqrscp output:\n%s", serverLog.String())
		}
	})
	upstream := net.JoinHostPort("127.0.0.1", strconv.Itoa(qrPort))
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		conn, err := net.Dial("tcp", upstream)
		if err == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("dcmqrscp did not listen: %v", err)
		}
	}

	movePort := freeTCPPort(t)
	source, err := NewDIMSERetrieveSource(DIMSERetrieveSourceOptions{
		Address: upstream, CalledAETitle: "QRSCP", CallingAETitle: "PROXY",
		DialOptions: ul.DialOptions{NegotiationTimeout: 10 * time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := NewRetrieveProxy(RetrieveProxyOptions{
		Source: source, AETitle: "PROXY",
		Destinations: MoveDestinationPolicy{
			Destinations: map[string]MoveDestinationAddress{"MOVESCU": {Address: net.JoinHostPort("127.0.0.1", strconv.Itoa(movePort))}},
			CallerOnly:   true,
		},
		DestinationDialOptions: ul.DialOptions{NegotiationTimeout: 10 * time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	address := startRetrieveProxy(t, ctx, proxy)
	_, proxyPort, _ := net.SplitHostPort(address)
	keys := []string{"-k", "QueryRetrieveLevel=STUDY", "-k", "StudyInstanceUID=" + proxyStudy}

	countFiles := func(dir string) int {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		return len(entries)
	}

	moveDir := filepath.Join(dir, "move")
	if err := os.Mkdir(moveDir, 0o700); err != nil {
		t.Fatal(err)
	}
	args := append([]string{"-v", "-S", "-aet", "MOVESCU", "-aec", "PROXY", "-aem", "MOVESCU", "--port", strconv.Itoa(movePort), "-od", moveDir}, keys...)
	out := dcmtkoracle.Run(t, dcmtkoracle.Protocol, movescu, append(args, "127.0.0.1", proxyPort)...)
	if !strings.Contains(string(out), "Final Move Response (Success)") && !strings.Contains(string(out), "Status                   : Success") {
		t.Fatalf("movescu did not report success:\n%s", out)
	}
	if got := countFiles(moveDir); got != len(ct) {
		t.Fatalf("movescu received %d files, want %d:\n%s", got, len(ct), out)
	}

	getDir := filepath.Join(dir, "get")
	if err := os.Mkdir(getDir, 0o700); err != nil {
		t.Fatal(err)
	}
	args = append([]string{"-v", "-S", "-aet", "GETSCU", "-aec", "PROXY", "-od", getDir}, keys...)
	out = dcmtkoracle.Run(t, dcmtkoracle.Protocol, getscu, append(args, "127.0.0.1", proxyPort)...)
	if got := countFiles(getDir); got != len(ct) {
		t.Fatalf("getscu received %d files, want %d:\n%s", got, len(ct), out)
	}

	// The policy only lets a caller move to itself.
	refusedDir := filepath.Join(dir, "refused")
	if err := os.Mkdir(refusedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	args = append([]string{"-v", "-S", "-aet", "INTRUDER", "-aec", "PROXY", "-aem", "MOVESCU", "--port", strconv.Itoa(movePort), "-od", refusedDir}, keys...)
	out, _ = dcmtkoracle.Command(ctx, t, dcmtkoracle.Protocol, movescu, append(args, "127.0.0.1", proxyPort)...).CombinedOutput()
	if !strings.Contains(string(out), "MoveDestinationUnknown") {
		t.Fatalf("movescu as another caller was not refused with A801H:\n%s", out)
	}
	if got := countFiles(refusedDir); got != 0 {
		t.Fatalf("refused move delivered %d files", got)
	}
}
