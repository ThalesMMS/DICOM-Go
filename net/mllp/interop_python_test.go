package mllp

import (
	"bufio"
	"context"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/hl7"
)

// pythonPeer is an independent MLLP peer written against the framing rules
// alone, with the Python standard library: as a client it sends two
// messages in one write split at odd offsets and checks both
// acknowledgments; as a server it acknowledges what it receives.
const pythonPeer = `
import socket, sys

VT, FS, CR = b"\x0b", b"\x1c", b"\r"

def read_frame(sock, buf):
    while FS + CR not in buf:
        chunk = sock.recv(7)
        if not chunk:
            raise SystemExit("connection closed inside a frame")
        buf += chunk
    end = buf.index(FS + CR)
    if not buf.startswith(VT):
        raise SystemExit("no start block")
    return buf[1:end], buf[end + 2:]

def field(message, segment, n):
    for line in message.split(b"\r"):
        parts = line.split(b"|")
        if parts[0] == segment:
            index = n - 1 if segment == b"MSH" else n
            return parts[index].decode() if index < len(parts) else ""
    return ""

mode, port = sys.argv[1], int(sys.argv[2])
if mode == "client":
    messages = [
        b"MSH|^~\\&|PY|LAB|TWIN|RAD|20261007120000||ADT^A08^ADT_A01|PY1|P|2.5.1\rEVN|A08\rPID|1||PY100^^^HOSP^MR||SYNTH^PY\r",
        b"MSH|^~\\&|PY|LAB|TWIN|RAD|20261007120000||ADT^A08^ADT_A01|PY2|P|2.5.1\rEVN|A08\rPID|1||||SYNTH^PY\r",
    ]
    stream = b"".join(VT + m + FS + CR for m in messages)
    sock = socket.create_connection(("127.0.0.1", port), timeout=10)
    for i in range(0, len(stream), 13):
        sock.sendall(stream[i:i + 13])
    buf = b""
    for control, code in (("PY1", "AA"), ("PY2", "AE")):
        ack, buf = read_frame(sock, buf)
        if field(ack, b"MSA", 1) != code or field(ack, b"MSA", 2) != control:
            raise SystemExit("unexpected acknowledgment for %s: %r" % (control, ack))
    print("client ok")
else:
    server = socket.socket()
    server.bind(("127.0.0.1", 0))
    server.listen(1)
    print(server.getsockname()[1], flush=True)
    conn, _ = server.accept()
    conn.settimeout(10)
    buf = b""
    message, buf = read_frame(conn, buf)
    control = field(message, b"MSH", 10)
    ack = b"MSH|^~\\&|PY|LAB|TWIN|RAD|20261007120001||ACK^A08^ACK|PYACK|P|2.5.1\rMSA|AA|" + control.encode() + b"\r"
    conn.sendall(VT + ack + FS + CR)
    conn.close()
`

// TestMLLPInteropPythonPeer exchanges frames with an independent peer. Enable with
// DICOMGO_HL7_INTEGRATION=1 and DICOMGO_PYTHON (Python 3, standard library
// only).
func TestMLLPInteropPythonPeer(t *testing.T) {
	if os.Getenv("DICOMGO_HL7_INTEGRATION") != "1" {
		t.Skip("opt-in: DICOMGO_HL7_INTEGRATION=1 with DICOMGO_PYTHON")
	}
	python := os.Getenv("DICOMGO_PYTHON")
	if python == "" {
		python = "python3"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	address, _, received := startServer(t, ServerOptions{})
	_, port, _ := net.SplitHostPort(address)
	out, err := exec.CommandContext(ctx, python, "-c", pythonPeer, "client", port).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "client ok") {
		t.Fatalf("Python client: %v\n%s", err, out)
	}
	if len(*received) != 1 || (*received)[0].Patient.Identifiers[0].ID != "PY100" {
		t.Fatalf("server received %+v", *received)
	}

	peer := exec.CommandContext(ctx, python, "-c", pythonPeer, "server", "0")
	stdout, err := peer.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := peer.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = peer.Wait() }()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	client, err := Dial(ctx, net.JoinHostPort("127.0.0.1", strings.TrimSpace(line)), ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	message, err := hl7.Parse(adt("GO1", "MRN1^^^HOSP^MR"))
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Send(ctx, message)
	if err != nil {
		t.Fatal(err)
	}
	if ack := ackOf(t, response); ack.Code != hl7.AckAccept || ack.ControlID != "GO1" {
		t.Fatalf("Python server acknowledgment %+v", ack)
	}
}
