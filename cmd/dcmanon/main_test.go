package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var (
	tagPatientName      = core.NewTag(0x0010, 0x0010)
	tagStudyDate        = core.NewTag(0x0008, 0x0020)
	tagStudyInstanceUID = core.NewTag(0x0020, 0x000D)
	tagSOPInstanceUID   = core.NewTag(0x0008, 0x0018)
)

// writeInstance writes a synthetic instance of the fixture study.
func writeInstance(t *testing.T, path, instance string) {
	t.Helper()
	elements := []core.Element{
		dicomtest.NewUIElement(core.NewTag(0x0008, 0x0016), dicomtest.TestSOPClassUID),
		dicomtest.NewUIElement(tagSOPInstanceUID, instance),
		dicomtest.NewDAElement(tagStudyDate, "20240115"),
		dicomtest.NewPNElement(tagPatientName, "SYNTHETIC^PERSON"),
		dicomtest.NewStringElement(core.NewTag(0x0010, 0x0020), core.VRLO, "SYN0001"),
		dicomtest.NewUIElement(tagStudyInstanceUID, dicomtest.TestStudyInstanceUID),
	}
	data, err := dicomtest.Part10File(transfer.ExplicitVRLittleEndian, elements...)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func value(t *testing.T, path string, tag core.Tag) string {
	t.Helper()
	file, err := object.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	v, _ := file.GetString(tag)
	return strings.TrimRight(v, " \x00")
}

func runAnon(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// TestAnonFileRemovesIdentityAndReportsWithoutValues: the Basic Profile
// replaces the name and the UIDs, and the JSON report names the actions
// without the values.
func TestAnonFileRemovesIdentityAndReportsWithoutValues(t *testing.T) {
	dir := t.TempDir()
	input, output, report := filepath.Join(dir, "in.dcm"), filepath.Join(dir, "out.dcm"), filepath.Join(dir, "report.json")
	writeInstance(t, input, "1.2.826.0.1.3680043.10.1055.1")
	code, stdout, stderr := runAnon("-report", report, "-o", output, input)
	if code != 0 || !strings.Contains(stdout, "1 file(s) de-identified, 0 failed") {
		t.Fatalf("code %d stdout %q stderr %q", code, stdout, stderr)
	}
	if name := value(t, output, tagPatientName); strings.Contains(name, "SYNTHETIC") {
		t.Fatalf("patient name kept: %q", name)
	}
	if uid := value(t, output, tagSOPInstanceUID); uid == "1.2.826.0.1.3680043.10.1055.1" || uid == "" {
		t.Fatalf("SOP Instance UID %q not remapped", uid)
	}
	data, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	var entries []fileReport
	if err := json.Unmarshal(data, &entries); err != nil || len(entries) != 1 || entries[0].Report == nil || len(entries[0].Report.Actions) == 0 {
		t.Fatalf("report %s, %v", data, err)
	}
	for _, secret := range []string{"SYNTHETIC", "SYN0001", "20240115", "1.2.826.0.1.3680043.10.1055.1"} {
		if bytes.Contains(data, []byte(secret)) {
			t.Fatalf("report contains the value %q", secret)
		}
	}
}

// TestAnonDirectoryKeepsReferencesConsistent: files of one study keep one
// remapped Study Instance UID; a keyed run is repeatable; a file that is not
// DICOM fails without stopping the others; options apply.
func TestAnonDirectoryKeepsReferencesConsistent(t *testing.T) {
	root := t.TempDir()
	input := filepath.Join(root, "in")
	writeInstance(t, filepath.Join(input, "a.dcm"), "1.2.826.0.1.3680043.10.1055.2")
	writeInstance(t, filepath.Join(input, "series", "b.dcm"), "1.2.826.0.1.3680043.10.1055.3")
	key := filepath.Join(root, "key")
	if err := os.WriteFile(key, []byte("synthetic test key"), 0o600); err != nil {
		t.Fatal(err)
	}
	var studies []string
	for _, out := range []string{"out1", "out2"} {
		output := filepath.Join(root, out)
		if code, _, stderr := runAnon("-uid-key-file", key, "-date-shift-days", "-10", "-o", output, input); code != 0 {
			t.Fatalf("code %d stderr %q", code, stderr)
		}
		a, b := value(t, filepath.Join(output, "a.dcm"), tagStudyInstanceUID), value(t, filepath.Join(output, "series", "b.dcm"), tagStudyInstanceUID)
		if a != b || a == dicomtest.TestStudyInstanceUID {
			t.Fatalf("study UIDs %q and %q", a, b)
		}
		if date := value(t, filepath.Join(output, "a.dcm"), tagStudyDate); date != "20240105" {
			t.Fatalf("shifted study date %q", date)
		}
		studies = append(studies, a)
	}
	if studies[0] != studies[1] {
		t.Fatal("the keyed remapping differs between runs")
	}
	if err := os.WriteFile(filepath.Join(input, "notes.txt"), []byte("not DICOM"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, _ := runAnon("-o", filepath.Join(root, "out3"), input)
	if code != 1 || !strings.Contains(stdout, "2 file(s) de-identified, 1 failed") {
		t.Fatalf("mixed directory: code %d stdout %q", code, stdout)
	}
}

func TestAnonUsageErrors(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "in.dcm")
	writeInstance(t, input, "1.2.826.0.1.3680043.10.1055.4")
	out := filepath.Join(dir, "out.dcm")
	for _, args := range [][]string{
		{"-option", "clean-pixel-data", "-o", out, input},
		{"-option", "no-such-option", "-o", out, input},
		{"-option", "retain-full-dates", "-date-shift-days", "3", "-o", out, input},
		{input},
	} {
		if code, _, _ := runAnon(args...); code != 2 {
			t.Fatalf("%v: code %d, want 2", args, code)
		}
	}
	if code, _, _ := runAnon("-o", filepath.Join(dir, "nested"), dir); code != 1 {
		t.Fatalf("output inside input: code %d", code)
	}
	if code, _, _ := runAnon("-option", "retain-device-identity,clean-descriptors", "-o", out, input); code != 0 {
		t.Fatalf("valid options: code %d", code)
	}
	if code, _, stderr := runAnon("-h"); code != 0 || !strings.Contains(stderr, "Usage: dcmanon") {
		t.Fatalf("-h: %d %q", code, stderr)
	}
}

// writeSyntheticRecipient writes a generated self-signed RSA recipient
// certificate and its PKCS #8 key as PEM files.
func writeSyntheticRecipient(t *testing.T, dir string) (string, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1177),
		Subject:      pkix.Name{CommonName: "Synthetic Research Recipient"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certificatePath, keyPath := filepath.Join(dir, "recipient.crt"), filepath.Join(dir, "recipient.key")
	if err := os.WriteFile(certificatePath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certificatePath, keyPath
}

// TestAnonEncryptForAndReidentifyRoundTrip: -encrypt-for stores the original
// values for the recipient and -reidentify restores them.
func TestAnonEncryptForAndReidentifyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	certificate, key := writeSyntheticRecipient(t, dir)
	input, anonymized, restored := filepath.Join(dir, "in.dcm"), filepath.Join(dir, "anon.dcm"), filepath.Join(dir, "restored.dcm")
	writeInstance(t, input, "1.2.826.0.1.3680043.10.1177.9")
	if code, stdout, stderr := runAnon("-encrypt-for", certificate, "-o", anonymized, input); code != 0 || !strings.Contains(stdout, "1 file(s) de-identified") {
		t.Fatalf("encrypt: code %d stdout %q stderr %q", code, stdout, stderr)
	}
	if name := value(t, anonymized, tagPatientName); strings.Contains(name, "SYNTHETIC") {
		t.Fatalf("patient name kept: %q", name)
	}
	report := filepath.Join(dir, "report.json")
	code, stdout, stderr := runAnon("-reidentify", "-recipient-cert", certificate, "-recipient-key", key, "-report", report, "-o", restored, anonymized)
	if code != 0 || !strings.Contains(stdout, "1 file(s) re-identified, 0 failed") {
		t.Fatalf("reidentify: code %d stdout %q stderr %q", code, stdout, stderr)
	}
	if name := value(t, restored, tagPatientName); name != "SYNTHETIC^PERSON" {
		t.Fatalf("restored patient name %q", name)
	}
	if uid := value(t, restored, tagSOPInstanceUID); uid != "1.2.826.0.1.3680043.10.1177.9" {
		t.Fatalf("restored SOP Instance UID %q", uid)
	}
	data, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("SYNTHETIC")) || !bytes.Contains(data, []byte(`"restored"`)) {
		t.Fatalf("re-identification report %s", data)
	}
	// A file without Encrypted Attributes fails.
	if code, _, _ := runAnon("-reidentify", "-recipient-cert", certificate, "-recipient-key", key, "-o", filepath.Join(dir, "x.dcm"), input); code != 1 {
		t.Fatalf("file without Encrypted Attributes: code %d", code)
	}
	for _, args := range [][]string{
		{"-reidentify", "-recipient-cert", certificate, "-o", restored, anonymized},
		{"-reidentify", "-recipient-cert", certificate, "-recipient-key", key, "-option", "retain-uids", "-o", restored, anonymized},
		{"-recipient-cert", certificate, "-recipient-key", key, "-o", restored, anonymized},
		{"-reidentify", "-recipient-cert", key, "-recipient-key", key, "-o", restored, anonymized},
		{"-encrypt-for", key, "-o", restored, input},
		{"-key-transport", "rsaes-pkcs1-v1_5", "-o", restored, input},
		{"-encrypt-for", certificate, "-key-transport", "rsaes-oaep-sha1", "-o", restored, input},
	} {
		if code, _, _ := runAnon(args...); code != 2 {
			t.Fatalf("%v: code %d, want 2", args, code)
		}
	}
}
