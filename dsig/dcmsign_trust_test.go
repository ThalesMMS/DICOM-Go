package dsig

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/transfer"
)

// dcmsignTrustFixtures holds files DCMTK dcmsign signed with certificates of
// a synthetic test PKI (no private keys), written by TestDCMSignTrustSamples
// with DICOMGO_UPDATE_GOLDEN=1.
const dcmsignTrustFixtures = "testdata/dcmsign-trust"

// dcmsignTrustSamples are the signed samples and the trust outcome each
// must have at the fixture's verification time: nil for trusted.
var dcmsignTrustSamples = []struct {
	file string
	want error
}{
	{"trusted.dcm", nil},
	{"untrusted.dcm", ErrCertificateUntrusted},
	{"expired.dcm", ErrCertificateExpired},
	{"revoked.dcm", ErrCertificateRevoked},
}

// Fixture files besides the samples.
const (
	trustRootFile         = "root.pem"
	trustIntermediateFile = "intermediate.pem"
	trustRootCRLFile      = "root.crl.pem"
	trustInterCRLFile     = "intermediate.crl.pem"
	trustTimeFile         = "verification-time.txt"
)

func readPEMBlocks(t *testing.T, path, blockType string) [][]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var blocks [][]byte
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			break
		}
		if block.Type == blockType {
			blocks = append(blocks, block.Bytes)
		}
	}
	if len(blocks) == 0 {
		t.Fatalf("%s: no %s", path, blockType)
	}
	return blocks
}

func readCertFile(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	cert, err := x509.ParseCertificate(readPEMBlocks(t, path, "CERTIFICATE")[0])
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func readCRLFile(t *testing.T, path string) *x509.RevocationList {
	t.Helper()
	crl, err := x509.ParseRevocationList(readPEMBlocks(t, path, "X509 CRL")[0])
	if err != nil {
		t.Fatal(err)
	}
	return crl
}

// trustOptionsFrom loads the anchors, intermediates and CRLs of dir.
func trustOptionsFrom(t *testing.T, dir string, at time.Time) TrustOptions {
	t.Helper()
	return TrustOptions{
		Roots:         certPool(readCertFile(t, filepath.Join(dir, trustRootFile))),
		Intermediates: certPool(readCertFile(t, filepath.Join(dir, trustIntermediateFile))),
		CRLs:          []*x509.RevocationList{readCRLFile(t, filepath.Join(dir, trustRootCRLFile)), readCRLFile(t, filepath.Join(dir, trustInterCRLFile))},
		RequireCRL:    true,
		Time:          at,
	}
}

// checkTrustSample verifies a dcmsign sample: the signature always
// verifies, and the certificate has the expected trust outcome.
func checkTrustSample(t *testing.T, path string, opts TrustOptions, want error) Result {
	t.Helper()
	results := VerifyWithTrust(readTestFile(t, path), opts)
	if len(results) != 1 {
		t.Fatalf("%s: %d signatures, want 1", path, len(results))
	}
	result := results[0]
	if !result.Valid() {
		t.Fatalf("%s: signature invalid: %v", path, result.Err)
	}
	if want == nil {
		if !result.Trusted() {
			t.Fatalf("%s: not trusted: %v", path, result.TrustErr)
		}
		return result
	}
	if result.Trusted() || !errors.Is(result.TrustErr, want) {
		t.Fatalf("%s: trust error %v, want %v", path, result.TrustErr, want)
	}
	// Each sample fails for its own reason only.
	for _, other := range []error{ErrCertificateExpired, ErrCertificateRevoked, ErrRevocationUnknown, ErrCertificateUsage} {
		if other != want && errors.Is(result.TrustErr, other) {
			t.Fatalf("%s: trust error %v also matches %v", path, result.TrustErr, other)
		}
	}
	return result
}

// TestDCMSignTrustFixtures checks the committed dcmsign samples (#1173):
// signed with a trusted, an untrusted, an expired and a revoked
// certificate, all four signatures verify and only the first certificate
// is trusted at the recorded verification time.
func TestDCMSignTrustFixtures(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(dcmsignTrustFixtures, trustTimeFile))
	if err != nil {
		t.Fatal(err)
	}
	at, err := time.Parse(time.RFC3339, strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	opts := trustOptionsFrom(t, dcmsignTrustFixtures, at)
	for _, sample := range dcmsignTrustSamples {
		t.Run(sample.file, func(t *testing.T) {
			checkTrustSample(t, filepath.Join(dcmsignTrustFixtures, sample.file), opts, sample.want)
		})
	}
	// The expired certificate had already expired when dcmsign signed.
	atSignature := opts
	atSignature.AtSignatureTime = true
	checkTrustSample(t, filepath.Join(dcmsignTrustFixtures, "expired.dcm"), atSignature, ErrCertificateExpired)
	// Without the intermediate CRL the revoked certificate is trusted,
	// and with RequireCRL it is refused as unknown.
	noCRL := opts
	noCRL.CRLs = opts.CRLs[:1]
	noCRL.RequireCRL = false
	checkTrustSample(t, filepath.Join(dcmsignTrustFixtures, "revoked.dcm"), noCRL, nil)
	noCRL.RequireCRL = true
	checkTrustSample(t, filepath.Join(dcmsignTrustFixtures, "revoked.dcm"), noCRL, ErrRevocationUnknown)
}

// TestDCMSignTrustSamples is opt-in evidence for #1173: DCMTK dcmsign signs
// with a trusted (through an intermediate), an untrusted, an expired and a
// revoked certificate; every signature verifies here, VerifyWithTrust gives
// each certificate its outcome, and dcmsign --verify with the same root,
// intermediate and CRLs (+cl) accepts only the trusted one. With
// DICOMGO_UPDATE_GOLDEN=1 the samples, certificates and CRLs (no keys)
// replace testdata/dcmsign-trust. Enable with DICOMGO_DCMTK_INTEGRATION=1.
func TestDCMSignTrustSamples(t *testing.T) {
	tool := requireDCMSign(t)
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	year := 365 * 24 * time.Hour
	// Validity starts two days back: dcmsign reads the Digital Signature
	// DateTime it writes in local time without its offset, so a certificate
	// issued an hour ago looks "not yet valid" east or west of UTC.
	start := now.Add(-48 * time.Hour)
	root := newTrustCA(t, dir, "root", nil, start, now.Add(30*year))
	inter := newTrustCA(t, dir, "intermediate", &root, start, now.Add(25*year))
	other := newTrustCA(t, dir, "other-root", nil, start, now.Add(30*year))
	valid := leafSpec{notBefore: start, notAfter: now.Add(20 * year), keyUsage: signingUsage()}
	signers := map[string]signer{
		"trusted.dcm":   inter.issue(t, "trusted", valid),
		"untrusted.dcm": other.issue(t, "untrusted", valid),
		"expired.dcm":   inter.issue(t, "expired", leafSpec{notBefore: now.Add(-96 * time.Hour), notAfter: start, keyUsage: signingUsage()}),
		"revoked.dcm":   inter.issue(t, "revoked", valid),
	}
	_, rootCRL := root.crl(t, "root", start, now.Add(20*year))
	_, interCRL := inter.crl(t, "intermediate", start, now.Add(20*year), signers["revoked.dcm"].leaf)
	files := map[string]string{
		trustRootFile:         root.File,
		trustIntermediateFile: inter.File,
		trustRootCRLFile:      rootCRL,
		trustInterCRLFile:     interCRL,
	}
	in := writeTestFile(t, "in.dcm", transfer.ExplicitVRLittleEndian, testElements(false))
	opts := TrustOptions{
		Roots:         certPool(root.Cert),
		Intermediates: certPool(inter.Cert),
		RequireCRL:    true,
		Time:          now,
	}
	opts.CRLs = []*x509.RevocationList{readCRLFile(t, rootCRL), readCRLFile(t, interCRL)}
	for _, sample := range dcmsignTrustSamples {
		t.Run(sample.file, func(t *testing.T) {
			s := signers[sample.file]
			out := filepath.Join(dir, sample.file)
			if log, err := dcmsign(t, tool, signedVerifies, "-pw", "+s", s.cred.KeyFile, s.cred.CertFile, "+m2", in, out); err != nil {
				t.Fatalf("dcmsign: %v\n%s", err, log)
			}
			checkTrustSample(t, out, opts, sample.want)
			args := []string{"--verify", "+rg", "+cf", root.File, "+uf", inter.File, "+cr", rootCRL, "+cr", interCRL, "+cl", out}
			log, err := dcmsign(t, tool, "dcmsign --verify accepts the trusted sample only", args...)
			if accepted := err == nil; accepted != (sample.want == nil) {
				t.Fatalf("dcmsign --verify accepted=%v, want %v:\n%s", accepted, sample.want == nil, log)
			}
			t.Logf("dcmsign --verify %s:\n%s", sample.file, strings.TrimSpace(log))
			files[sample.file] = out
		})
	}
	if os.Getenv("DICOMGO_UPDATE_GOLDEN") != "1" || t.Failed() {
		return
	}
	if err := os.MkdirAll(dcmsignTrustFixtures, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, source := range files {
		data, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dcmsignTrustFixtures, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dcmsignTrustFixtures, trustTimeFile), []byte(now.Format(time.RFC3339)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}
