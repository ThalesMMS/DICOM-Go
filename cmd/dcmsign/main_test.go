package main

import (
	"bytes"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dsig"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/internal/testutil"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func writeInput(t *testing.T, dir string) string {
	t.Helper()
	item := core.DataSet{Elements: []core.Element{
		dicomtest.StringElement(core.NewTag(0x0008, 0x1150), core.VRUI, "1.2.840.10008.3.1.2.3.1"),
		dicomtest.StringElement(core.NewTag(0x0008, 0x1155), core.VRUI, "1.2.826.0.1.3680043.10.543.1175.9"),
	}}
	data, err := dicomtest.Part10File(transfer.ExplicitVRLittleEndian,
		dicomtest.StringElement(core.NewTag(0x0008, 0x0016), core.VRUI, "1.2.840.10008.5.1.4.1.1.7"),
		dicomtest.StringElement(core.NewTag(0x0008, 0x0018), core.VRUI, "1.2.826.0.1.3680043.10.543.1175.1"),
		core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x1110), VR: core.VRSQ, Length: core.UndefinedLength, LengthSet: true}, Value: core.SequenceValue{Items: []core.DataSet{item}}},
		dicomtest.StringElement(core.NewTag(0x0010, 0x0010), core.VRPN, "TEST^DCMSIGN"),
		dicomtest.StringElement(core.NewTag(0x0020, 0x000D), core.VRUI, "1.2.826.0.1.3680043.10.543.1175.2"),
		dicomtest.StringElement(core.NewTag(0x0020, 0x000E), core.VRUI, "1.2.826.0.1.3680043.10.543.1175.3"),
	)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "in.dcm")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// dcmsign runs the command and returns its exit code and output.
func dcmsign(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String() + stderr.String()
}

func mustRun(t *testing.T, args ...string) string {
	t.Helper()
	code, out := dcmsign(t, args...)
	if code != 0 {
		t.Fatalf("dcmsign %s: exit %d\n%s", strings.Join(args, " "), code, out)
	}
	return out
}

func signatures(t *testing.T, path string) []dsig.Result {
	t.Helper()
	file, err := object.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	return dsig.Verify(file.Dataset)
}

var uidPattern = regexp.MustCompile(`UID (2\.25\.[0-9]+)`)

// TestDCMSignWorkflow covers #1175: sign the data set and a sequence item,
// list and verify them (signature, trust, profile), timestamp with an
// online and an offline TSA, detect tampering, then remove the signatures.
func TestDCMSignWorkflow(t *testing.T) {
	dir := t.TempDir()
	input := writeInput(t, dir)
	pki := testutil.NewTestPKI(t)
	cred := pki.Issue(t, "dcmsign-cli.test", "rsa2048")
	tsa := testutil.NewTestTSA(t, testutil.TestTSAOptions{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		response, err := tsa.Respond(body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/timestamp-reply")
		w.Write(response)
	}))
	defer server.Close()

	signed := filepath.Join(dir, "signed.dcm")
	out := mustRun(t, "sign", "-key", cred.KeyFile, "-cert", cred.CertFile, "-profile", "creator", "-purpose", "1", "-tsa-url", server.URL, "-o", signed, input)
	if !strings.Contains(out, "signed main data set with SHA256") || !strings.Contains(out, "timestamped") {
		t.Fatalf("sign output:\n%s", out)
	}
	mainUID := uidPattern.FindStringSubmatch(out)[1]
	out = mustRun(t, "sign", "-key", cred.KeyFile, "-cert", cred.CertFile, "-item", "ReferencedStudySequence[0]", "-mac", "sha512", "-in-place", signed)
	itemUID := uidPattern.FindStringSubmatch(out)[1]
	if !strings.Contains(out, "signed (0008,1110)[0] with SHA512") {
		t.Fatalf("item sign output:\n%s", out)
	}

	list := mustRun(t, "list", signed)
	for _, want := range []string{"signature 1: main data set", "signature 2: (0008,1110)[0]", "purpose:             (1, ASTM-sigpurpose, \"Author's Signature\")", "certified timestamp: CMS_TSP", "Creator RSA"} {
		if !strings.Contains(list, want) {
			t.Fatalf("list lacks %q:\n%s", want, list)
		}
	}
	verified := mustRun(t, "verify", "-ca", pki.CAFile, "-tsa-ca", tsa.RootFile, "-require-signature", "-require-profile", "creator", signed)
	if strings.Count(verified, "signature:   valid") != 2 || strings.Count(verified, "certificate: trusted") != 2 || !strings.Contains(verified, "TSA trusted") {
		t.Fatalf("verify output:\n%s", verified)
	}
	// The item signature has no timestamp yet.
	if code, out := dcmsign(t, "verify", "-require-timestamp", signed); code != 1 || !strings.Contains(out, "timestamp:   none") {
		t.Fatalf("require timestamp: exit %d\n%s", code, out)
	}

	// Offline TSA: write the request, answer it, insert the response.
	query, response := filepath.Join(dir, "q.tsq"), filepath.Join(dir, "r.tsr")
	mustRun(t, "timestamp", "-uid", itemUID, "-write-query", query, signed)
	request, err := os.ReadFile(query)
	if err != nil {
		t.Fatal(err)
	}
	answer, err := tsa.Respond(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(response, answer, 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out := dcmsign(t, "timestamp", "-uid", mainUID, "-query", query, "-response", response, "-in-place", signed); code != 1 || !strings.Contains(out, "not a request for signature") {
		t.Fatalf("response for another signature: exit %d\n%s", code, out)
	}
	mustRun(t, "timestamp", "-uid", itemUID, "-query", query, "-response", response, "-in-place", signed)
	mustRun(t, "verify", "-ca", pki.CAFile, "-tsa-ca", tsa.RootFile, "-require-timestamp", "-at-timestamp-time", signed)
	for _, result := range signatures(t, signed) {
		if !result.Valid() || !result.TimestampValid() {
			t.Fatalf("library verification: %v %v", result.Err, result.TimestampErr)
		}
	}

	// Untrusted anchors and tampering fail with exit status 1.
	other := testutil.NewTestPKI(t)
	if code, out := dcmsign(t, "verify", "-ca", other.CAFile, signed); code != 1 || !strings.Contains(out, "UNTRUSTED") {
		t.Fatalf("foreign CA: exit %d\n%s", code, out)
	}
	file, err := object.OpenFile(signed)
	if err != nil {
		t.Fatal(err)
	}
	file.Dataset.Put(dicomtest.StringElement(core.NewTag(0x0010, 0x0010), core.VRPN, "TEST^CHANGED"))
	tampered := filepath.Join(dir, "tampered.dcm")
	if err := writeFile(file, signed, output{path: tampered}); err != nil {
		t.Fatal(err)
	}
	file.Close()
	if code, out := dcmsign(t, "verify", tampered); code != 1 || strings.Count(out, "INVALID") != 1 {
		t.Fatalf("tampered: exit %d\n%s", code, out)
	}

	// Remove one, then all.
	unsigned := filepath.Join(dir, "unsigned.dcm")
	mustRun(t, "remove", "-uid", mainUID, "-o", unsigned, signed)
	if results := signatures(t, unsigned); len(results) != 1 || results[0].Signature.UID != itemUID {
		t.Fatalf("after remove -uid: %+v", results)
	}
	mustRun(t, "remove", "-all", "-in-place", unsigned)
	if code, out := dcmsign(t, "verify", "-require-signature", unsigned); code != 1 || !strings.Contains(out, "no signatures") {
		t.Fatalf("unsigned: exit %d\n%s", code, out)
	}
	if code, _ := dcmsign(t, "verify", unsigned); code != 0 {
		t.Fatal("verify without -require-signature fails on an unsigned file")
	}
}

func TestDCMSignRejectsUnsafeInput(t *testing.T) {
	dir := t.TempDir()
	input := writeInput(t, dir)
	pki := testutil.NewTestPKI(t)
	cred := pki.Issue(t, "dcmsign-cli.test", "p256")

	encrypted := filepath.Join(dir, "encrypted.key")
	block := &pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: []byte{0x30, 0x00}}
	if err := os.WriteFile(encrypted, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out := dcmsign(t, "sign", "-key", encrypted, "-cert", cred.CertFile, "-o", filepath.Join(dir, "x.dcm"), input); code != 1 || !strings.Contains(out, "encrypted keys are not read") {
		t.Fatalf("encrypted key: exit %d\n%s", code, out)
	}
	shared := filepath.Join(dir, "shared.key")
	data, err := os.ReadFile(cred.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shared, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, out := dcmsign(t, "sign", "-key", shared, "-cert", cred.CertFile, "-o", filepath.Join(dir, "y.dcm"), input); !strings.Contains(out, "accessible to other users") {
		t.Fatalf("no warning for a shared key file:\n%s", out)
	}
	other := pki.Issue(t, "other.test", "p256")
	if code, out := dcmsign(t, "sign", "-key", other.KeyFile, "-cert", cred.CertFile, "-o", filepath.Join(dir, "z.dcm"), input); code != 1 || !strings.Contains(out, "does not match") {
		t.Fatalf("mismatched key: exit %d\n%s", code, out)
	}
	if code, out := dcmsign(t, "sign", "-key", cred.KeyFile, "-cert", cred.CertFile, "-profile", "creator", "-item", "ReferencedStudySequence[0]", "-o", filepath.Join(dir, "p.dcm"), input); code != 1 || !strings.Contains(out, "profile violated") {
		t.Fatalf("profile violation: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "p.dcm")); !os.IsNotExist(err) {
		t.Fatal("a refused signature wrote an output")
	}
}

func TestDCMSignUsage(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"bogus"},
		{"sign", "in.dcm"},
		{"sign", "-key", "k", "-cert", "c", "in.dcm"},
		{"sign", "-key", "k", "-cert", "c", "-o", "a", "-in-place", "in.dcm"},
		{"remove", "-o", "out.dcm", "in.dcm"},
		{"remove", "-all", "-uid", "1", "-o", "out.dcm", "in.dcm"},
		{"timestamp", "-uid", "1", "in.dcm"},
		{"timestamp", "-uid", "1", "-query", "q", "-in-place", "in.dcm"},
		{"verify", "-require-profile", "bogus", "in.dcm"},
		{"verify", "-at-timestamp-time", "in.dcm"},
		{"verify", "a.dcm", "b.dcm"},
	} {
		if code, out := dcmsign(t, args...); code != 2 {
			t.Fatalf("%v: exit %d, want 2\n%s", args, code, out)
		}
	}
	if code, out := dcmsign(t, "-h"); code != 0 || !strings.Contains(out, "Usage: dcmsign") {
		t.Fatalf("-h: exit %d\n%s", code, out)
	}
	if code, _ := dcmsign(t, "verify", "-h"); code != 0 {
		t.Fatal("verify -h failed")
	}
}
