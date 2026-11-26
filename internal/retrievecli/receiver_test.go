package retrievecli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

const receiverCTClass = "1.2.840.10008.5.1.4.1.1.2"

func TestMoveReceiverStopsBeforeOutgoingReleaseCompletes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	remote, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	localAddress := reserved.Addr().String()
	localPort := reserved.Addr().(*net.TCPAddr).Port
	reserved.Close()
	releaseSeen := make(chan struct{})
	allowReply := make(chan struct{}, 1)
	defer close(allowReply)
	peerDone := make(chan error, 1)
	go func() {
		peerDone <- func() error {
			a, err := remote.AcceptAssociation(ul.AcceptOptions{AETitle: "QRSCP", Context: ctx, SupportedAbstractSyntaxes: []string{dimse.StudyRootMoveSOPClassUID}, SupportedTransferSyntaxes: []string{transfer.ExplicitVRLittleEndian.UID}})
			if err != nil {
				return err
			}
			defer a.Close()
			pc, err := dimse.AcceptedContextForSOPClass(a, dimse.StudyRootMoveSOPClassUID)
			if err != nil {
				return err
			}
			req, err := dimse.ReceiveCMoveRequest(a, pc.ID)
			if err != nil {
				return err
			}
			if _, err := dimse.ReceiveDataSet(a, pc.ID, transfer.ExplicitVRLittleEndian); err != nil {
				return err
			}
			if err := dimse.SendCMoveResponse(a, pc.ID, dimse.CMoveResponse{AffectedSOPClassUID: dimse.StudyRootMoveSOPClassUID, MessageIDBeingRespondedTo: req.MessageID, Status: 0}); err != nil {
				return err
			}
			pdu, err := a.ReadPDU()
			if err != nil {
				return err
			}
			if _, ok := pdu.(*ul.ReleaseRQ); !ok {
				return fmt.Errorf("release PDU=%T", pdu)
			}
			close(releaseSeen)
			select {
			case <-allowReply:
			case <-ctx.Done():
				return ctx.Err()
			}
			return a.WritePDU(&ul.ReleaseRP{})
		}()
	}()
	outputDir := t.TempDir()
	cliDone := make(chan int, 1)
	go func() {
		cliDone <- RunWithContext(ctx, []string{"-remote", remote.Addr().String(), "-called", "QRSCP", "-listen-port", strconv.Itoa(localPort), "-move-destination", "DESTAE", "-study-uid", "1.2.3", "-output", outputDir, "-timeout", "2s"}, io.Discard, io.Discard)
	}()
	select {
	case <-releaseSeen:
	case err := <-peerDone:
		t.Fatalf("peer before ReleaseRQ: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	conn, err := net.DialTimeout("tcp", localAddress, 100*time.Millisecond)
	if err == nil {
		conn.Close()
		t.Error("MOVE receiver still accepts after terminal response while outgoing ReleaseRP is withheld")
	}
	allowReply <- struct{}{}
	if code := <-cliDone; code != 0 {
		t.Errorf("retrieve exit=%d", code)
	}
	if err := <-peerDone; err != nil {
		t.Errorf("peer: %v", err)
	}
}

func receiverDataSet(study string) *object.Object {
	return object.FromElements([]core.Element{
		{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0016), VR: core.VRUI}, Value: core.StringValue{receiverCTClass}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0018), VR: core.VRUI}, Value: core.StringValue{"1.2.826.0.1.3680043.10.1355.1.1"}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0020, 0x000d), VR: core.VRUI}, Value: core.StringValue{study}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0020, 0x000e), VR: core.VRUI}, Value: core.StringValue{"1.2.826.0.1.3680043.10.1355.1.2"}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0010), VR: core.VRUS}, Value: core.Uint16Value{1}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0011), VR: core.VRUS}, Value: core.Uint16Value{2}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0100), VR: core.VRUS}, Value: core.Uint16Value{16}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0101), VR: core.VRUS}, Value: core.Uint16Value{16}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0102), VR: core.VRUS}, Value: core.Uint16Value{15}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0103), VR: core.VRUS}, Value: core.Uint16Value{0}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0002), VR: core.VRUS}, Value: core.Uint16Value{1}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0004), VR: core.VRCS}, Value: core.StringValue{"MONOCHROME2"}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x7fe0, 0x0010), VR: core.VROW}, Value: core.RawValue{1, 0, 2, 0}},
	}, std.Dictionary)
}
func receiverOptionsForTest(t *testing.T, extra ...string) retrieveOptions {
	t.Helper()
	args := []string{"-listen-port", "0", "-called", "STORESCU", "-move-destination", "DESTAE", "-study-uid", "1.2.3", "-output", t.TempDir()}
	args = append(args, extra...)
	opts, err := parseRetrieveArgs(args, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	return opts
}
func dialReceiver(t *testing.T, ctx context.Context, r *moveReceiver, calling, called string) (*ul.Association, error) {
	t.Helper()
	return ul.DialContext(ctx, r.address, ul.DialOptions{CallingAETitle: calling, CalledAETitle: called, Contexts: []ul.PresentationContext{{AbstractSyntaxUID: receiverCTClass, TransferSyntaxUIDs: []string{transfer.ExplicitVRLittleEndian.UID}}}})
}
func TestMoveReceiverStorageBindingAndLimits(t *testing.T) {
	for _, tt := range []struct {
		name, study string
		limit       string
		want        uint16
	}{{"accepted", "1.2.3", "", 0}, {"foreign-study", "1.2.4", "", dimse.StatusCStoreDataSetDoesNotMatch}, {"byte-cap", "1.2.3", "64", dimse.StatusCStoreOutOfResources}} {
		t.Run(tt.name, func(t *testing.T) {
			extra := []string{}
			if tt.limit != "" {
				extra = append(extra, "-receiver-max-dataset-bytes", tt.limit)
			}
			opts := receiverOptionsForTest(t, extra...)
			id, err := buildFixedRetrieveIdentifier(opts)
			if err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			r, err := startMoveReceiver(opts, []*object.Object{id}, &synchronizedWriter{writer: &stdout}, &synchronizedWriter{writer: &stderr})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			a, err := dialReceiver(t, ctx, r, "STORESCU", "DESTAE")
			if err != nil {
				r.shutdown()
				t.Fatal(err)
			}
			rsp, err := dimse.NewStoreClient(a).Store(ctx, receiverDataSet(tt.study))
			if err != nil {
				a.Close()
				r.shutdown()
				t.Fatal(err)
			}
			if rsp == nil || rsp.Status != tt.want {
				t.Fatalf("store response=%#v", rsp)
			}
			if err := a.Release(ctx); err != nil {
				t.Fatal(err)
			}
			a.Close()
			if err := r.shutdown(); err != nil {
				t.Fatal(err)
			}
			files, err := os.ReadDir(opts.outputDir)
			if err != nil {
				t.Fatal(err)
			}
			stats := r.snapshot()
			if tt.want == 0 {
				if len(files) != 1 || stats.committed != 1 {
					t.Fatalf("commit count/files=%+v/%v", stats, files)
				}
				data, err := os.ReadFile(filepath.Join(opts.outputDir, files[0].Name()))
				if err != nil {
					t.Fatal(err)
				}
				f, err := object.ReadFile(bytes.NewReader(data))
				if err != nil {
					t.Fatal(err)
				}
				for tag, want := range map[core.Tag]string{core.NewTag(0x0002, 0x0016): "DESTAE", core.NewTag(0x0002, 0x0017): "STORESCU", core.NewTag(0x0002, 0x0018): "DESTAE"} {
					if got, _ := f.GetString(tag); got != want {
						t.Fatalf("AE tag %s=%q", tag, got)
					}
				}
				if data, _ := f.GetRaw(core.NewTag(0x7fe0, 0x0010)); !bytes.Equal(data, []byte{1, 0, 2, 0}) {
					t.Fatal("pixels changed")
				}
			} else {
				if len(files) != 0 || stats.committed != 0 || stats.rejected != 1 {
					t.Fatalf("rejected store published or count wrong: %+v/%v", stats, files)
				}
			}
		})
	}
}
func TestMoveReceiverAdmissionDrainAndPrefixConflicts(t *testing.T) {
	for _, args := range [][]string{{"-listen-host", "127.0.0.1"}, {"-listen-port", "1", "-listen-port", "2"}, {"-listen-port", "0", "-listen-ae", "OTHER"}, {"-listen-port", "0", "-method", "get"}, {"-listen-port", "0", "-listen-cert", "missing"}, {"-listen-port", "0", "-listen-tls-profile", "bcp195-rfc8996"}} {
		if _, err := parseRetrieveArgs(args, io.Discard); err == nil {
			t.Fatalf("invalid receiver flags=%v", args)
		}
	}
	opts := receiverOptionsForTest(t, "-receiver-drain-timeout", "20ms")
	id, _ := buildFixedRetrieveIdentifier(opts)
	r, err := startMoveReceiver(opts, []*object.Object{id}, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if a, err := dialReceiver(t, ctx, r, "INTRUDER", "DESTAE"); err == nil {
		a.Close()
		t.Fatal("wrong Calling AE accepted")
	}
	if a, err := dialReceiver(t, ctx, r, "STORESCU", "WRONG"); err == nil {
		a.Close()
		t.Fatal("wrong Called AE accepted")
	}
	a, err := dialReceiver(t, ctx, r, "STORESCU", "DESTAE")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err := r.shutdown(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("active-association drain=%v", err)
	}
	if conn, err := net.DialTimeout("tcp", r.address, 50*time.Millisecond); err == nil {
		conn.Close()
		t.Fatal("receiver listener still alive")
	}
}
func TestOccupiedReceiverPortRefusesBeforeOutgoingMOVE(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	remote, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	var stderr bytes.Buffer
	code := Run([]string{"-remote", remote.Addr().String(), "-listen-port", strconv.Itoa(ln.Addr().(*net.TCPAddr).Port), "-study-uid", "1.2.3", "-output", t.TempDir()}, io.Discard, &stderr)
	if code != 1 {
		t.Fatalf("bind failure exit=%d", code)
	}
	remote.(*net.TCPListener).SetDeadline(time.Now().Add(20 * time.Millisecond))
	if c, err := remote.Accept(); err == nil {
		c.Close()
		t.Fatal("outgoing connection preceded receiver bind")
	}
}

func TestMoveReceiverIndependentMutualTLS(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1355), Subject: pkix.Name{CommonName: "Synthetic test only"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	encodedKey, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "synthetic-cert.pem"), filepath.Join(dir, "synthetic-key.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encodedKey})
	if err := os.WriteFile(certPath, certPEM, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		t.Fatal(err)
	}
	opts := receiverOptionsForTest(t, "-listen-tls-profile", "bcp195-rfc8996", "-listen-cert", certPath, "-listen-key", keyPath, "-listen-ca", certPath)
	if opts.tls.Enabled() {
		t.Fatal("listener TLS enabled requestor TLS")
	}
	id, _ := buildFixedRetrieveIdentifier(opts)
	r, err := startMoveReceiver(opts, []*object.Object{id}, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(certPEM)
	a, err := ul.DialContext(ctx, r.address, ul.DialOptions{CallingAETitle: "STORESCU", CalledAETitle: "DESTAE", TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool, ServerName: "127.0.0.1", Certificates: []tls.Certificate{cert}}, Contexts: []ul.PresentationContext{{AbstractSyntaxUID: receiverCTClass, TransferSyntaxUIDs: []string{transfer.ExplicitVRLittleEndian.UID}}}})
	if err != nil {
		r.shutdown()
		t.Fatal(err)
	}
	rsp, err := dimse.NewStoreClient(a).Store(ctx, receiverDataSet("1.2.3"))
	if err != nil || rsp.Status != 0 {
		t.Fatalf("TLS store=%#v/%v", rsp, err)
	}
	if err := a.Release(ctx); err != nil {
		t.Fatal(err)
	}
	a.Close()
	if err := r.shutdown(); err != nil {
		t.Fatal(err)
	}
	if r.snapshot().committed != 1 {
		t.Fatal("TLS store not committed")
	}
}
