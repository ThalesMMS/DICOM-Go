package dsig

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/internal/testutil"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// timestampFixtures holds a file signed here whose Certified Timestamp an
// OpenSSL TSA (`openssl ts -reply`, the time stamping engine DCMTK dcmsign
// relies on) issued, with the synthetic roots of the signer and the TSA (no
// keys). TestOpenSSLTimestampSamples rewrites it with
// DICOMGO_OPENSSL_INTEGRATION=1 DICOMGO_UPDATE_GOLDEN=1.
const timestampFixtures = "testdata/timestamp"

const (
	timestampSampleFile     = "openssl-tsa.dcm"
	timestampSignerRootFile = "signer-root.pem"
	timestampTSARootFile    = "tsa-root.pem"
	timestampTimeFile       = "verification-time.txt"
	envOpenSSLIntegration   = "DICOMGO_OPENSSL_INTEGRATION"
)

func readFixtureTime(t *testing.T, path string) time.Time {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	at, err := time.Parse(time.RFC3339, strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	return at
}

// checkOpenSSLSample verifies the OpenSSL-timestamped sample: the
// signature, its token and both chains, and that a changed token is refused.
func checkOpenSSLSample(t *testing.T, obj *object.Object, signerRoot, tsaRoot string, at time.Time) {
	t.Helper()
	opts := TrustOptions{
		Roots:           certPool(readCertFile(t, signerRoot)),
		Time:            at,
		TimestampRoots:  certPool(readCertFile(t, tsaRoot)),
		AtTimestampTime: true,
	}
	result := onlyResult(t, VerifyWithTrust(obj, opts))
	if !result.Valid() || !result.Trusted() || !result.TimestampValid() || !result.Timestamp.Trusted {
		t.Fatalf("valid %v trusted %v timestamp %v", result.Err, result.TrustErr, result.TimestampErr)
	}
	if !result.Timestamp.CertificateIncluded || result.Timestamp.Nonce == nil {
		t.Fatalf("timestamp info %+v", result.Timestamp)
	}
	tampered := result.Signature
	tampered.CertifiedTimestamp = append([]byte(nil), tampered.CertifiedTimestamp...)
	tampered.CertifiedTimestamp[len(tampered.CertifiedTimestamp)-8] ^= 0x10
	if _, err := VerifyTimestamp(tampered, opts.timestampOptions()); !errors.Is(err, ErrTimestampInvalid) {
		t.Fatalf("tampered OpenSSL token: %v", err)
	}
}

// TestOpenSSLTimestampFixture checks the committed sample (#1174): a token
// from an independent RFC 3161 implementation verifies and certifies the
// signature, and a changed token is refused.
func TestOpenSSLTimestampFixture(t *testing.T) {
	at := readFixtureTime(t, filepath.Join(timestampFixtures, timestampTimeFile))
	obj := readTestFile(t, filepath.Join(timestampFixtures, timestampSampleFile))
	checkOpenSSLSample(t, obj, filepath.Join(timestampFixtures, timestampSignerRootFile), filepath.Join(timestampFixtures, timestampTSARootFile), at)
}

// requireOpenSSL returns the openssl command, skipping unless
// DICOMGO_OPENSSL_INTEGRATION=1 and openssl is installed.
func requireOpenSSL(t *testing.T) string {
	t.Helper()
	if os.Getenv(envOpenSSLIntegration) != "1" {
		t.Skipf("opt-in: %s=1 with openssl on PATH", envOpenSSLIntegration)
	}
	path, err := exec.LookPath("openssl")
	if err != nil {
		t.Skipf("openssl not found: %v", err)
	}
	version, _ := exec.Command(path, "version").Output()
	t.Logf("openssl: %s", strings.TrimSpace(string(version)))
	return path
}

func runTool(t *testing.T, tool string, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	cmd := exec.Command(tool, args...)
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %s: %v\n%s", filepath.Base(tool), strings.Join(args, " "), err, out.String())
	}
	return out.String()
}

// openSSLTSAConfig writes an `openssl ts` configuration for tsa.
func openSSLTSAConfig(t *testing.T, dir string, tsa *testutil.TestTSA) string {
	t.Helper()
	serial := filepath.Join(dir, "tsa-serial")
	if err := os.WriteFile(serial, []byte("01\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "tsa.cnf")
	text := strings.Join([]string{
		"[ tsa ]",
		"default_tsa = test_tsa",
		"[ test_tsa ]",
		"serial = " + serial,
		"signer_cert = " + tsa.CertFile,
		"signer_key = " + tsa.KeyFile,
		"signer_digest = sha256",
		"default_policy = " + testutil.TestTSAPolicy.String(),
		"digests = sha256, sha384, sha512",
		"accuracy = secs:1",
		"ess_cert_id_alg = sha256",
		"",
	}, "\n")
	if err := os.WriteFile(config, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return config
}

// TestOpenSSLTimestampSamples is opt-in evidence for #1174 with an
// independent RFC 3161 implementation: `openssl ts -reply` answers the
// request built here and the token verifies here; `openssl ts -verify`
// accepts the tokens of the in-process test TSA the other tests use. With
// DICOMGO_UPDATE_GOLDEN=1 the sample replaces testdata/timestamp. Enable
// with DICOMGO_OPENSSL_INTEGRATION=1.
func TestOpenSSLTimestampSamples(t *testing.T) {
	openssl := requireOpenSSL(t)
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	year := 365 * 24 * time.Hour
	root := newTrustCA(t, dir, "signer-root", nil, now.Add(-48*time.Hour), now.Add(30*year))
	s := root.issue(t, "timestamped-signer", leafSpec{notBefore: now.Add(-48 * time.Hour), notAfter: now.Add(20 * year), keyUsage: signingUsage()})
	tsa := testutil.NewTestTSA(t, testutil.TestTSAOptions{KeyKind: "rsa2048", NotBefore: now.Add(-48 * time.Hour), NotAfter: now.Add(30 * year)})
	config := openSSLTSAConfig(t, dir, tsa)

	obj := newObject(testElements(false), transfer.ExplicitVRLittleEndian)
	sig, err := Sign(obj, s.options(MACSHA256))
	if err != nil {
		t.Fatal(err)
	}
	openSSLTSA := TimestamperFunc(func(_ context.Context, request []byte) ([]byte, error) {
		query := filepath.Join(dir, "query.tsq")
		reply := filepath.Join(dir, "reply.tsr")
		if err := os.WriteFile(query, request, 0o600); err != nil {
			return nil, err
		}
		runTool(t, openssl, "ts", "-reply", "-config", config, "-queryfile", query, "-out", reply)
		return os.ReadFile(reply)
	})
	if _, _, err := TimestampSignature(context.Background(), obj, sig, openSSLTSA, TimestampRequestOptions{}); err != nil {
		t.Fatalf("OpenSSL token: %v", err)
	}
	sample := writeTestFile(t, timestampSampleFile, transfer.ExplicitVRLittleEndian, obj.Elements())
	checkOpenSSLSample(t, readTestFile(t, sample), root.File, tsa.RootFile, now)

	// The other direction: OpenSSL verifies the test TSA's response.
	req, err := NewTimestampRequest(sig, TimestampRequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	response, err := tsa.Respond(req.DER)
	if err != nil {
		t.Fatal(err)
	}
	query, reply := filepath.Join(dir, "own.tsq"), filepath.Join(dir, "own.tsr")
	if err := os.WriteFile(query, req.DER, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reply, response, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("openssl ts -verify: %s", strings.TrimSpace(runTool(t, openssl, "ts", "-verify", "-queryfile", query, "-in", reply, "-CAfile", tsa.RootFile)))

	if os.Getenv("DICOMGO_UPDATE_GOLDEN") != "1" || t.Failed() {
		return
	}
	if err := os.MkdirAll(timestampFixtures, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, source := range map[string]string{timestampSampleFile: sample, timestampSignerRootFile: root.File, timestampTSARootFile: tsa.RootFile} {
		data, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(timestampFixtures, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(timestampFixtures, timestampTimeFile), []byte(now.Format(time.RFC3339)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestDCMSignTimestamps is opt-in evidence for #1174 against DCMTK
// dcmsign: a timestamp query dcmsign writes (+ts), answered by `openssl ts
// -reply` and inserted by dcmsign (+t), verifies here; a file timestamped
// here passes dcmsign --verify +tr. Enable with DICOMGO_DCMTK_INTEGRATION=1
// and DICOMGO_OPENSSL_INTEGRATION=1.
func TestDCMSignTimestamps(t *testing.T) {
	tool := requireDCMSign(t)
	openssl := requireOpenSSL(t)
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	year := 365 * 24 * time.Hour
	root := newTrustCA(t, dir, "signer-root", nil, now.Add(-48*time.Hour), now.Add(30*year))
	s := root.issue(t, "dcmsign-ts-signer", leafSpec{notBefore: now.Add(-48 * time.Hour), notAfter: now.Add(20 * year), keyUsage: signingUsage()})
	tsa := testutil.NewTestTSA(t, testutil.TestTSAOptions{NotBefore: now.Add(-48 * time.Hour), NotAfter: now.Add(30 * year)})
	config := openSSLTSAConfig(t, dir, tsa)

	in := writeTestFile(t, "in.dcm", transfer.ExplicitVRLittleEndian, testElements(false))
	signed := filepath.Join(dir, "signed.dcm")
	query, uidFile, reply := filepath.Join(dir, "q.tsq"), filepath.Join(dir, "uid.txt"), filepath.Join(dir, "r.tsr")
	if log, err := dcmsign(t, tool, signedVerifies, "-pw", "+s", s.cred.KeyFile, s.cred.CertFile, "+m2", "+ts", query, uidFile, in, signed); err != nil {
		t.Fatalf("dcmsign +ts: %v\n%s", err, log)
	}
	runTool(t, openssl, "ts", "-reply", "-config", config, "-queryfile", query, "-out", reply)
	stamped := filepath.Join(dir, "stamped.dcm")
	if log, err := dcmsign(t, tool, dcmtkoracle.Accepted, "+t", query, reply, uidFile, signed, stamped); err != nil {
		t.Fatalf("dcmsign +t: %v\n%s", err, log)
	}
	checkOpenSSLSample(t, readTestFile(t, stamped), root.File, tsa.RootFile, now)

	obj, _ := timestamped(t, s, tsa, TimestampRequestOptions{})
	path := writeTestFile(t, "go-stamped.dcm", transfer.ExplicitVRLittleEndian, obj.Elements())
	if log, err := dcmsign(t, tool, dcmtkoracle.Accepted, "--verify", "+rg", "+tr", "+cf", root.File, "+cf", tsa.RootFile, path); err != nil {
		t.Fatalf("dcmsign --verify +tr: %v\n%s", err, log)
	}
}
