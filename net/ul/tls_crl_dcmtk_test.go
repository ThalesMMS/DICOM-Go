package ul

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// This optional local interoperability profile is pinned to the audited
// DCMTK 3.7.0/OpenSSL build. It never runs against a configured PACS.
func tlsCRLDCMTKTool(t *testing.T, dir, name, want string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != want {
		t.Fatalf("%s binary digest differs from the qualified profile", name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	if err != nil || !bytes.Contains(out, []byte("v3.7.0 2025-12-15")) || !bytes.Contains(out, []byte("OpenSSL 3.6.4")) {
		t.Fatalf("%s version/profile: %v %s", name, err, out)
	}
	t.Logf("%s SHA256=%s DCMTK=3.7.0 OpenSSL=3.6.4", name, want)
	return path
}

func tlsCRLWritePEM(t *testing.T, path, kind string, der ...[]byte) {
	t.Helper()
	var data []byte
	for _, raw := range der {
		data = append(data, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: raw})...)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func tlsCRLInteropFiles(t *testing.T, f tlsCRLFixture) (dir, ca, serverCert, serverKey, clientCert, clientKey, good string) {
	t.Helper()
	dir = t.TempDir()
	ca = filepath.Join(dir, "root.pem")
	serverCert = filepath.Join(dir, "server.pem")
	serverKey = filepath.Join(dir, "server.key")
	clientCert = filepath.Join(dir, "client.pem")
	clientKey = filepath.Join(dir, "client.key")
	good = filepath.Join(dir, "good-crls.pem")
	tlsCRLWritePEM(t, ca, "CERTIFICATE", f.root.Raw)
	tlsCRLWritePEM(t, serverCert, "CERTIFICATE", f.server.Certificate...)
	tlsCRLWritePEM(t, clientCert, "CERTIFICATE", f.client.Certificate...)
	for path, key := range map[string]any{serverKey: f.server.PrivateKey, clientKey: f.client.PrivateKey} {
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		tlsCRLWritePEM(t, path, "PRIVATE KEY", der)
	}
	// OpenSSL full-chain checking can include the anchor, unlike this Go
	// profile. Supply its complete self-issued CRL rather than weaken checks.
	tlsCRLWritePEM(t, good, "X509 CRL", f.list(t, false).Raw, f.list(t, true).Raw)
	return
}

// boundedToolLog is safe for a running subprocess and caps captured output.
type boundedTLSCRLToolLog struct {
	mu   sync.Mutex
	data []byte
}

func (b *boundedTLSCRLToolLog) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.data) < 32768 {
		n := 32768 - len(b.data)
		if n > len(p) {
			n = len(p)
		}
		b.data = append(b.data, p[:n]...)
	}
	return len(p), nil
}
func (b *boundedTLSCRLToolLog) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(append([]byte(nil), b.data...))
}

func tlsCRLEchoCommand(response bool) []byte {
	var rest []byte
	appendElement := func(tag uint16, value []byte) {
		rest = binary.LittleEndian.AppendUint16(rest, 0)
		rest = binary.LittleEndian.AppendUint16(rest, tag)
		rest = binary.LittleEndian.AppendUint32(rest, uint32(len(value)))
		rest = append(rest, value...)
	}
	uid := []byte(verificationSOPClassUID)
	if len(uid)%2 != 0 {
		uid = append(uid, 0)
	}
	appendElement(2, uid)
	field, idTag := uint16(0x0030), uint16(0x0110)
	if response {
		field, idTag = 0x8030, 0x0120
	}
	appendElement(0x0100, binary.LittleEndian.AppendUint16(nil, field))
	appendElement(idTag, []byte{1, 0})
	appendElement(0x0800, []byte{1, 1})
	if response {
		appendElement(0x0900, []byte{0, 0})
	}
	group := binary.LittleEndian.AppendUint16(nil, 0)
	group = binary.LittleEndian.AppendUint16(group, 0)
	group = binary.LittleEndian.AppendUint32(group, 4)
	group = binary.LittleEndian.AppendUint32(group, uint32(len(rest)))
	return append(group, rest...)
}

func tlsCRLReadEcho(assoc *Association, response bool) (byte, error) {
	var command []byte
	var pc byte
	for {
		pdu, err := assoc.ReadPDU()
		if err != nil {
			return 0, err
		}
		data, ok := pdu.(*PDataTF)
		if !ok {
			return 0, errors.New("expected C-ECHO P-DATA")
		}
		for _, pdv := range data.Values {
			if !pdv.IsCommand || len(command)+len(pdv.Data) > 65536 || (pc != 0 && pc != pdv.PresentationContextID) {
				return 0, errors.New("invalid bounded C-ECHO command")
			}
			pc = pdv.PresentationContextID
			command = append(command, pdv.Data...)
			if pdv.IsLast {
				goto parsed
			}
		}
	}
parsed:
	field, mid, status := uint16(0xffff), uint16(0), uint16(0xffff)
	for len(command) > 0 {
		if len(command) < 8 {
			return 0, io.ErrUnexpectedEOF
		}
		tag, n := binary.LittleEndian.Uint16(command[2:4]), int(binary.LittleEndian.Uint32(command[4:8]))
		if n > len(command)-8 {
			return 0, io.ErrUnexpectedEOF
		}
		value := command[8 : 8+n]
		if n == 2 {
			v := binary.LittleEndian.Uint16(value)
			switch tag {
			case 0x0100:
				field = v
			case 0x0110, 0x0120:
				mid = v
			case 0x0900:
				status = v
			}
		}
		command = command[8+n:]
	}
	want := uint16(0x0030)
	if response {
		want = 0x8030
	}
	if field != want || mid != 1 || (response && status != 0) {
		return 0, errors.New("unexpected C-ECHO response/correlation")
	}
	return pc, nil
}

func tlsCRLServeEcho(t *testing.T, ctx context.Context, config *tls.Config) (string, <-chan error) {
	t.Helper()
	listener, err := Listen(ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	done := make(chan error, 1)
	go func() {
		defer listener.Close()
		assoc, err := listener.AcceptAssociation(AcceptOptions{AETitle: "CRLSCP", TLSConfig: config, Context: ctx, SupportedAbstractSyntaxes: []string{verificationSOPClassUID}, SupportedTransferSyntaxes: []string{ImplicitVRLittleEndian, ExplicitVRLittleEndian}})
		if err != nil {
			done <- err
			return
		}
		defer assoc.Close()
		pc, err := tlsCRLReadEcho(assoc, false)
		if err == nil {
			err = assoc.WritePDU(&PDataTF{Values: []PDataValue{{PresentationContextID: pc, IsCommand: true, IsLast: true, Data: tlsCRLEchoCommand(true)}}})
		}
		if err == nil {
			pdu, e := assoc.ReadPDU()
			err = e
			if _, ok := pdu.(*ReleaseRQ); err == nil && ok {
				err = assoc.WritePDU(&ReleaseRP{})
			} else if err == nil {
				err = errors.New("expected association release")
			}
		}
		done <- err
	}()
	return listener.Addr().String(), done
}

func TestTLSCRLDCMTKLocalInterop(t *testing.T) {
	dir := os.Getenv("DICOMGO_TLS_CRL_DCMTK_DIR")
	if dir == "" {
		t.Skip("set DICOMGO_TLS_CRL_DCMTK_DIR to the pinned local DCMTK 3.7.0 bin directory")
	}
	echo := tlsCRLDCMTKTool(t, dir, "echoscu", "bf9f155ecc1e6c7714053efda257e22e97d12ef841df36b910aad34e879c54b2")
	store := tlsCRLDCMTKTool(t, dir, "storescp", "23c6167c3669fe62174b9bab5f45bfd4386ae8154570cf1a22e3d4405b45ce86")
	dictionary := os.Getenv("DICOMGO_TLS_CRL_DCMTK_DICTIONARY")
	if dictionary == "" {
		t.Fatal("set DICOMGO_TLS_CRL_DCMTK_DICTIONARY to the qualified dicom.dic")
	}
	data, err := os.ReadFile(dictionary)
	if err != nil {
		t.Fatal(err)
	}
	dictionaryDigest := sha256.Sum256(data)
	if hex.EncodeToString(dictionaryDigest[:]) != "235128eca117534eb418487280f560285eb3322f1b5e773ffb9ece648fdf4634" {
		t.Fatal("DCMTK dictionary digest differs from the qualified profile")
	}
	t.Logf("DCMTK dictionary SHA256=%x", dictionaryDigest)
	toolEnvironment := func() []string {
		var env []string
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(entry, "DCMDICTPATH=") {
				env = append(env, entry)
			}
		}
		return append(env, "DCMDICTPATH="+dictionary)
	}
	for _, mode := range []TLSCRLMode{TLSCRLLeaf, TLSCRLChain} {
		for _, scenario := range []string{"good", "leaf-revoked", "intermediate-revoked"} {
			modeName := map[TLSCRLMode]string{TLSCRLLeaf: "leaf", TLSCRLChain: "chain"}[mode]
			shouldPass := scenario == "good" || (scenario == "intermediate-revoked" && mode == TLSCRLLeaf)
			t.Run("DCMTK-SCU/"+modeName+"/"+scenario, func(t *testing.T) {
				f := newTLSCRLFixture(t)
				work, ca, _, _, clientCert, clientKey, good := tlsCRLInteropFiles(t, f)
				leaf, root := f.list(t, false), f.list(t, true)
				if scenario == "leaf-revoked" {
					leaf = f.list(t, false, f.server.Leaf)
				}
				if scenario == "intermediate-revoked" {
					root = f.list(t, true, f.intermediate)
				}
				selected := filepath.Join(work, "selected.pem")
				tlsCRLWritePEM(t, selected, "X509 CRL", leaf.Raw, root.Raw)
				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				defer cancel()
				address, done := tlsCRLServeEcho(t, ctx, f.config(t, true, TLSCRLChain, f.list(t, false), f.list(t, true)))
				host, port, _ := net.SplitHostPort(address)
				flag := "+crv"
				if mode == TLSCRLChain {
					flag = "+cra"
				}
				// Go requires a good client chain; DCMTK independently checks the
				// server with the selected leaf/full-chain policy.
				_ = good
				command := exec.CommandContext(ctx, echo, "-ll", "error", "-to", "3", "-ta", "3", "-td", "3", "+tls", clientKey, clientCert, "+cf", ca, "+crl", selected, flag, "-aec", "CRLSCP", host, port)
				command.Env = toolEnvironment()
				out, err := command.CombinedOutput()
				if (err == nil) != shouldPass {
					t.Fatalf("DCMTK outcome=%v want success=%v log=%s", err, shouldPass, out)
				}
				if !shouldPass && !bytes.Contains(out, []byte("certificate verify failed")) {
					t.Fatalf("negative control did not fail certificate verification: %s", out)
				}
				serverErr := <-done
				if shouldPass && serverErr != nil {
					t.Fatalf("Go SCP: %v", serverErr)
				}
				t.Logf("mode=%s scenario=%s tool-success=%v Go-SCP-success=%v", modeName, scenario, err == nil, serverErr == nil)
			})
			t.Run("Go-SCU/"+modeName+"/"+scenario, func(t *testing.T) {
				f := newTLSCRLFixture(t)
				work, ca, serverCert, serverKey, _, _, good := tlsCRLInteropFiles(t, f)
				reserve, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				port := reserve.Addr().(*net.TCPAddr).Port
				reserve.Close()
				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, store, "-ll", "error", "-i4", "-aet", "CRLSCP", "-od", work, "+tls", serverKey, serverCert, "+cf", ca, "+crl", good, "+cra", strconv.Itoa(port))
				cmd.Env = toolEnvironment()
				var log boundedTLSCRLToolLog
				cmd.Stdout, cmd.Stderr = &log, &log
				if err = cmd.Start(); err != nil {
					t.Fatal(err)
				}
				processDone := make(chan error, 1)
				go func() { processDone <- cmd.Wait() }()
				defer func() { cancel(); <-processDone }()
				address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
				deadline := time.Now().Add(3 * time.Second)
				ready := false
				for time.Now().Before(deadline) {
					probe, e := net.DialTimeout("tcp", address, 50*time.Millisecond)
					if e == nil {
						probe.Close()
						ready = true
						break
					}
					time.Sleep(15 * time.Millisecond)
				}
				if !ready {
					t.Fatalf("storescp not ready: %s", log.String())
				}
				lists := []*x509.RevocationList{f.list(t, false), f.list(t, true)}
				if scenario == "leaf-revoked" {
					lists[0] = f.list(t, false, f.server.Leaf)
				}
				if scenario == "intermediate-revoked" {
					lists[1] = f.list(t, true, f.intermediate)
				}
				assoc, err := Dial(address, DialOptions{Context: ctx, CalledAETitle: "CRLSCP", CallingAETitle: "CRLSCU", TLSConfig: f.config(t, false, mode, lists...), Contexts: []PresentationContext{{AbstractSyntaxUID: verificationSOPClassUID, TransferSyntaxUIDs: []string{ImplicitVRLittleEndian}}}})
				if (err == nil) != shouldPass {
					t.Fatalf("Go outcome=%v want success=%v log=%s", err, shouldPass, log.String())
				}
				if !shouldPass {
					tlsCRLKind(t, err, TLSRevocationRevoked)
					return
				}
				defer assoc.Close()
				if len(assoc.AcceptedContexts) != 1 || assoc.AcceptedContexts[0].AbstractSyntaxUID != verificationSOPClassUID {
					t.Fatal("verification context refused")
				}
				pc := assoc.AcceptedContexts[0]
				if err = assoc.WritePDU(&PDataTF{Values: []PDataValue{{PresentationContextID: pc.ID, IsCommand: true, IsLast: true, Data: tlsCRLEchoCommand(false)}}}); err != nil {
					t.Fatal(err)
				}
				if _, err = tlsCRLReadEcho(assoc, true); err != nil {
					t.Fatal(err)
				}
				if err = assoc.Release(ctx); err != nil {
					t.Fatal(err)
				}
				t.Logf("mode=%s scenario=%s correlated C-ECHO and release succeeded", modeName, scenario)
			})
		}
	}
}
