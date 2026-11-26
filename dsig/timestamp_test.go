package dsig

import (
	"bytes"
	"context"
	"crypto"
	"crypto/x509"
	"encoding/asn1"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/internal/testutil"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// timestamped signs a synthetic object with s, timestamps the signature with
// tsa and returns the object as read back from a Part 10 file.
func timestamped(t *testing.T, s signer, tsa Timestamper, opts TimestampRequestOptions) (*object.Object, TimestampInfo) {
	t.Helper()
	obj := newObject(testElements(false), transfer.ExplicitVRLittleEndian)
	sig, err := Sign(obj, s.options(MACSHA256))
	if err != nil {
		t.Fatal(err)
	}
	updated, info, err := TimestampSignature(context.Background(), obj, sig, tsa, opts)
	if err != nil {
		t.Fatal(err)
	}
	if updated.CertifiedTimestampType != CertifiedTimestampCMSTSP || len(updated.CertifiedTimestamp) == 0 {
		t.Fatalf("timestamp not stored: %+v", updated)
	}
	return roundTrip(t, obj, transfer.ExplicitVRLittleEndian), info
}

func onlyResult(t *testing.T, results []Result) Result {
	t.Helper()
	if len(results) != 1 {
		t.Fatalf("%d results, want 1", len(results))
	}
	return results[0]
}

// TestCertifiedTimestampRoundTrip covers #1174: a signature timestamped by
// an in-process RFC 3161 TSA (RSA or ECDSA signer and TSA, signing
// certificate v1 or v2, SHA-256/384/512 imprints) still verifies, its token
// verifies and certifies the signature value, and the TSA is trusted only
// with its root.
func TestCertifiedTimestampRoundTrip(t *testing.T) {
	pki := testutil.NewTestPKI(t)
	signers := map[string]signer{
		"rsa":   newSigner(t, pki, "ts-signer-rsa.test", "rsa2048"),
		"ecdsa": newSigner(t, pki, "ts-signer-ec.test", "p256"),
	}
	for _, tc := range []struct {
		name, signer, tsaKey string
		v1                   bool
		hash                 crypto.Hash
	}{
		{"rsa signer, ec tsa", "rsa", "p256", false, crypto.SHA256},
		{"ec signer, rsa tsa", "ecdsa", "rsa2048", false, crypto.SHA384},
		{"signing certificate v1", "rsa", "p256", true, crypto.SHA512},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tsa := testutil.NewTestTSA(t, testutil.TestTSAOptions{KeyKind: tc.tsaKey})
			tsa.SigningCertificateV1 = tc.v1
			obj, info := timestamped(t, signers[tc.signer], tsa, TimestampRequestOptions{Hash: tc.hash})
			if info.HashAlgorithm != tc.hash || info.Nonce == nil || !info.CertificateIncluded || !info.Policy.Equal(testutil.TestTSAPolicy) {
				t.Fatalf("info %+v", info)
			}
			result := onlyResult(t, Verify(obj))
			if !result.Valid() || !result.TimestampValid() || result.Timestamp.Trusted {
				t.Fatalf("Verify: valid=%v timestamp=%v (%v) trusted=%v", result.Valid(), result.TimestampValid(), result.TimestampErr, result.Timestamp != nil && result.Timestamp.Trusted)
			}
			if d := result.Timestamp.Time.Sub(time.Now()); d > time.Minute || d < -time.Minute {
				t.Fatalf("genTime %v", result.Timestamp.Time)
			}
			if result.Timestamp.Accuracy != 500*time.Millisecond {
				t.Fatalf("accuracy %v", result.Timestamp.Accuracy)
			}
			trusted, err := VerifyTimestamp(result.Signature, TimestampOptions{Roots: tsa.RootPool})
			if err != nil || !trusted.Trusted || len(trusted.Chains) == 0 {
				t.Fatalf("trusted TSA: %+v %v", trusted, err)
			}
			other := testutil.NewTestTSA(t, testutil.TestTSAOptions{})
			if _, err := VerifyTimestamp(result.Signature, TimestampOptions{Roots: other.RootPool}); !errors.Is(err, ErrTimestampUntrusted) {
				t.Fatalf("foreign root: %v", err)
			}
		})
	}
}

// TestCertifiedTimestampRejectsTampering: a changed token, a changed
// signature value, a token for another signature and a TSA certificate
// without the critical time stamping usage are refused.
func TestCertifiedTimestampRejectsTampering(t *testing.T) {
	pki := testutil.NewTestPKI(t)
	s := newSigner(t, pki, "ts-signer.test", "rsa2048")
	tsa := testutil.NewTestTSA(t, testutil.TestTSAOptions{})
	obj, _ := timestamped(t, s, tsa, TimestampRequestOptions{})
	sig := onlyResult(t, Verify(obj)).Signature

	expectInvalid := func(t *testing.T, sig Signature) {
		t.Helper()
		if _, err := VerifyTimestamp(sig, TimestampOptions{Roots: tsa.RootPool}); !errors.Is(err, ErrTimestampInvalid) {
			t.Fatalf("tampered timestamp: %v", err)
		}
	}
	t.Run("token bytes", func(t *testing.T) {
		// The certified imprint (in the TSTInfo), the middle of the token
		// and the end of the TSA's signature.
		imprint := bytes.Index(sig.CertifiedTimestamp, digest(crypto.SHA256, sig.Signature))
		if imprint < 0 {
			t.Fatal("imprint not found in the token")
		}
		for _, offset := range []int{imprint + 5, len(sig.CertifiedTimestamp) / 2, len(sig.CertifiedTimestamp) - 10} {
			tampered := sig
			tampered.CertifiedTimestamp = append([]byte(nil), sig.CertifiedTimestamp...)
			tampered.CertifiedTimestamp[offset] ^= 0x01
			expectInvalid(t, tampered)
		}
	})
	t.Run("signature value", func(t *testing.T) {
		tampered := sig
		tampered.Signature = append([]byte(nil), sig.Signature...)
		tampered.Signature[3] ^= 0x01
		expectInvalid(t, tampered)
	})
	t.Run("token of another signature", func(t *testing.T) {
		other, _ := timestamped(t, s, tsa, TimestampRequestOptions{})
		swapped := sig
		swapped.CertifiedTimestamp = onlyResult(t, Verify(other)).Signature.CertifiedTimestamp
		expectInvalid(t, swapped)
		// Through the object, the stored token is refused as well.
		stored := onlyResult(t, Verify(other))
		obj := newObject(testElements(false), transfer.ExplicitVRLittleEndian)
		if _, err := AddTimestamp(obj, sig, stored.Signature.CertifiedTimestamp); !errors.Is(err, ErrTimestampInvalid) {
			t.Fatalf("AddTimestamp with a foreign token: %v", err)
		}
	})
	t.Run("changed signature in the object", func(t *testing.T) {
		tampered := roundTrip(t, obj, transfer.ExplicitVRLittleEndian)
		tampered.Put(dicomtest.StringElement(tagPatientName, core.VRPN, "TEST^TSTAMP"))
		result := onlyResult(t, Verify(tampered))
		// The data no longer matches the signature; the timestamp still
		// certifies the (unchanged) signature value.
		if result.Valid() || !result.TimestampValid() {
			t.Fatalf("valid=%v timestamp=%v", result.Valid(), result.TimestampErr)
		}
	})
	t.Run("TSA usage not critical", func(t *testing.T) {
		weak := testutil.NewTestTSA(t, testutil.TestTSAOptions{NonCriticalUsage: true})
		req, err := NewTimestampRequest(sig, TimestampRequestOptions{})
		if err != nil {
			t.Fatal(err)
		}
		response, err := weak.Respond(req.DER)
		if err != nil {
			t.Fatal(err)
		}
		token, err := TokenFromResponse(response)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := req.Check(token); !errors.Is(err, ErrTimestampInvalid) {
			t.Fatalf("non-critical TSA usage: %v", err)
		}
	})
	t.Run("wrong type", func(t *testing.T) {
		other := sig
		other.CertifiedTimestampType = "OTHER"
		if _, err := VerifyTimestamp(other, TimestampOptions{}); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("type: %v", err)
		}
		other.CertifiedTimestamp = nil
		if _, err := VerifyTimestamp(other, TimestampOptions{}); !errors.Is(err, ErrNoTimestamp) {
			t.Fatalf("missing: %v", err)
		}
	})
}

// TestTimestampResponseChecks: a rejection, a response to another request
// (imprint, nonce), a missing requested certificate and a policy other than
// the requested one are refused before anything is stored.
func TestTimestampResponseChecks(t *testing.T) {
	pki := testutil.NewTestPKI(t)
	s := newSigner(t, pki, "ts-signer.test", "rsa2048")
	tsa := testutil.NewTestTSA(t, testutil.TestTSAOptions{})
	obj := newObject(testElements(false), transfer.ExplicitVRLittleEndian)
	sig, err := Sign(obj, s.options(MACSHA256))
	if err != nil {
		t.Fatal(err)
	}
	req, err := NewTimestampRequest(sig, TimestampRequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	respond := func(t *testing.T, tsa *testutil.TestTSA, der []byte) []byte {
		t.Helper()
		response, err := tsa.Respond(der)
		if err != nil {
			t.Fatal(err)
		}
		token, err := TokenFromResponse(response)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	if _, err := req.Check(respond(t, tsa, req.DER)); err != nil {
		t.Fatalf("matching response: %v", err)
	}
	t.Run("rejection", func(t *testing.T) {
		rejecting := testutil.NewTestTSA(t, testutil.TestTSAOptions{})
		rejecting.Reject = true
		_, _, err := TimestampSignature(context.Background(), obj, sig, rejecting, TimestampRequestOptions{})
		if !errors.Is(err, ErrTimestampResponse) || !strings.Contains(err.Error(), "rejection: test TSA rejects every request") {
			t.Fatalf("rejection: %v", err)
		}
		if got := Signatures(obj.Elements())[0]; len(got.CertifiedTimestamp) != 0 {
			t.Fatal("a rejected request stored a timestamp")
		}
	})
	t.Run("another request", func(t *testing.T) {
		other, err := NewTimestampRequest(sig, TimestampRequestOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := req.Check(respond(t, tsa, other.DER)); !errors.Is(err, ErrTimestampResponse) {
			t.Fatalf("nonce of another request: %v", err)
		}
		sha512, err := NewTimestampRequest(sig, TimestampRequestOptions{Hash: crypto.SHA512, NoNonce: true})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := req.Check(respond(t, tsa, sha512.DER)); !errors.Is(err, ErrTimestampResponse) {
			t.Fatalf("imprint of another request: %v", err)
		}
	})
	t.Run("certificate", func(t *testing.T) {
		omitting := testutil.NewTestTSA(t, testutil.TestTSAOptions{})
		omitting.OmitCertificate = true
		token := respond(t, omitting, req.DER)
		if _, err := req.Check(token); !errors.Is(err, ErrTimestampInvalid) {
			t.Fatalf("no certificate to verify with: %v", err)
		}
		if _, err := req.Check(token, omitting.Certificate); !errors.Is(err, ErrTimestampResponse) {
			t.Fatalf("requested certificate missing: %v", err)
		}
		noCert, err := NewTimestampRequest(sig, TimestampRequestOptions{NoCertificate: true})
		if err != nil {
			t.Fatal(err)
		}
		token = respond(t, tsa, noCert.DER)
		if _, err := noCert.Check(token, tsa.Certificate); err != nil {
			t.Fatalf("certificate supplied by the verifier: %v", err)
		}
		withToken := sig
		withToken.CertifiedTimestampType, withToken.CertifiedTimestamp = CertifiedTimestampCMSTSP, token
		if _, err := VerifyTimestamp(withToken, TimestampOptions{Certificates: []*x509.Certificate{tsa.Certificate}, Roots: tsa.RootPool}); err != nil {
			t.Fatalf("VerifyTimestamp with a supplied certificate: %v", err)
		}
	})
	t.Run("policy", func(t *testing.T) {
		policy := asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 32473, 1, 2}
		withPolicy, err := NewTimestampRequest(sig, TimestampRequestOptions{Policy: policy})
		if err != nil {
			t.Fatal(err)
		}
		info, err := withPolicy.Check(respond(t, tsa, withPolicy.DER))
		if err != nil || !info.Policy.Equal(policy) {
			t.Fatalf("policy %v: %v", info.Policy, err)
		}
		if _, err := withPolicy.Check(respond(t, tsa, req.DER)); !errors.Is(err, ErrTimestampResponse) {
			t.Fatalf("policy mismatch: %v", err)
		}
	})
}

// TestTimestampExtendsTrust: a signer certificate that has since expired is
// trusted at the time a trusted TSA certified, not at the current time,
// and not when the TSA is unknown or the time comes from an untrusted token.
func TestTimestampExtendsTrust(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().Truncate(time.Second)
	year := 365 * 24 * time.Hour
	root := newTrustCA(t, dir, "ts-root", nil, now.Add(-year), now.Add(10*year))
	s := root.issue(t, "ts-signer", leafSpec{notBefore: now.Add(-time.Hour), notAfter: now.Add(24 * time.Hour), keyUsage: signingUsage()})
	tsa := testutil.NewTestTSA(t, testutil.TestTSAOptions{})
	obj, _ := timestamped(t, s, tsa, TimestampRequestOptions{})
	later := now.Add(30 * 24 * time.Hour)

	opts := TrustOptions{Roots: certPool(root.Cert), Time: later}
	if result := onlyResult(t, VerifyWithTrust(obj, opts)); !errors.Is(result.TrustErr, ErrCertificateExpired) {
		t.Fatalf("at a later time: %v", result.TrustErr)
	}
	opts.AtTimestampTime = true
	if result := onlyResult(t, VerifyWithTrust(obj, opts)); !errors.Is(result.TrustErr, ErrTimestampUntrusted) {
		t.Fatalf("AtTimestampTime without TSA anchors: %v", result.TrustErr)
	}
	opts.TimestampRoots = tsa.RootPool
	result := onlyResult(t, VerifyWithTrust(obj, opts))
	if !result.Trusted() || !result.TimestampValid() || !result.Timestamp.Trusted {
		t.Fatalf("at the certified time: trust %v timestamp %v", result.TrustErr, result.TimestampErr)
	}
	opts.TimestampRoots = testutil.NewTestTSA(t, testutil.TestTSAOptions{}).RootPool
	result = onlyResult(t, VerifyWithTrust(obj, opts))
	if result.Trusted() || !errors.Is(result.TrustErr, ErrTimestampUntrusted) || !errors.Is(result.TimestampErr, ErrTimestampUntrusted) {
		t.Fatalf("unknown TSA: trust %v timestamp %v", result.TrustErr, result.TimestampErr)
	}

	// A signature without a timestamp is not trusted at a timestamp time.
	plain := signedObject(t, s, s.options(MACSHA256), false)
	opts.TimestampRoots = tsa.RootPool
	if result := onlyResult(t, VerifyWithTrust(plain, opts)); !errors.Is(result.TrustErr, ErrNoTimestamp) || result.HasTimestamp() {
		t.Fatalf("no timestamp: %v", result.TrustErr)
	}

	// A token certifying a time outside the signer certificate is refused.
	early := testutil.NewTestTSA(t, testutil.TestTSAOptions{NotBefore: now.Add(-year)})
	early.Now = func() time.Time { return now.Add(-48 * time.Hour) }
	obj = newObject(testElements(false), transfer.ExplicitVRLittleEndian)
	sig, err := Sign(obj, s.options(MACSHA256))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := TimestampSignature(context.Background(), obj, sig, early, TimestampRequestOptions{}); !errors.Is(err, ErrTimestampInvalid) {
		t.Fatalf("time before the signer certificate: %v", err)
	}
}

// TestTimestampNestedSignature timestamps a signature held in a sequence
// item and keeps the item's other content.
func TestTimestampNestedSignature(t *testing.T) {
	pki := testutil.NewTestPKI(t)
	s := newSigner(t, pki, "ts-signer.test", "p256")
	tsa := testutil.NewTestTSA(t, testutil.TestTSAOptions{})
	inner := newObject([]core.Element{
		uiElement(core.NewTag(0x0008, 0x1150), "1.2.840.10008.3.1.2.3.1"),
		uiElement(core.NewTag(0x0008, 0x1155), "1.2.826.0.1.3680043.10.543.998.9"),
	}, transfer.ExplicitVRLittleEndian)
	if _, err := Sign(inner, s.options(MACSHA256)); err != nil {
		t.Fatal(err)
	}
	obj := newObject(append(testElements(false)[:3], sequenceElement(tagReferencedStudy, core.DataSet{Elements: inner.Elements()})), transfer.ExplicitVRLittleEndian)
	sig := onlyResult(t, Verify(obj)).Signature
	if _, _, err := TimestampSignature(context.Background(), obj, sig, tsa, TimestampRequestOptions{}); err != nil {
		t.Fatal(err)
	}
	result := onlyResult(t, Verify(roundTrip(t, obj, transfer.ExplicitVRLittleEndian)))
	if result.Signature.Path != "(0008,1110)[0]" || !result.Valid() || !result.TimestampValid() {
		t.Fatalf("path %q valid %v timestamp %v", result.Signature.Path, result.Err, result.TimestampErr)
	}
}

// TestHTTPTimestamper posts to an in-process HTTP TSA (loopback only).
func TestHTTPTimestamper(t *testing.T) {
	tsa := testutil.NewTestTSA(t, testutil.TestTSAOptions{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/timestamp-query" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
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
	pki := testutil.NewTestPKI(t)
	obj, _ := timestamped(t, newSigner(t, pki, "ts-http.test", "rsa2048"), HTTPTimestamper{URL: server.URL, Client: server.Client()}, TimestampRequestOptions{})
	if result := onlyResult(t, Verify(obj)); !result.TimestampValid() {
		t.Fatal(result.TimestampErr)
	}
	// A server that is not a TSA is refused.
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("hello"))
	}))
	defer plain.Close()
	if _, err := (HTTPTimestamper{URL: plain.URL, Client: plain.Client()}).Timestamp(context.Background(), []byte{0x30, 0}); !errors.Is(err, ErrTimestampResponse) {
		t.Fatalf("non-TSA server: %v", err)
	}
}

// TestTimestampRequestEncoding checks the TimeStampReq DER against the
// RFC 3161 structure.
func TestTimestampRequestEncoding(t *testing.T) {
	sig := Signature{Signature: bytes.Repeat([]byte{7}, 256)}
	req, err := NewTimestampRequest(sig, TimestampRequestOptions{Policy: asn1.ObjectIdentifier{1, 2, 3}})
	if err != nil {
		t.Fatal(err)
	}
	var decoded timeStampReq
	if err := unmarshalDER(req.DER, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Version != 1 || !decoded.CertReq || decoded.Nonce.Cmp(req.Nonce) != 0 || !decoded.ReqPolicy.Equal(asn1.ObjectIdentifier{1, 2, 3}) ||
		!decoded.MessageImprint.HashAlgorithm.Algorithm.Equal(oidDigestSHA256) || !bytes.Equal(decoded.MessageImprint.HashedMessage, digest(crypto.SHA256, sig.Signature)) {
		t.Fatalf("request %+v", decoded)
	}
	parsed, err := ParseTimestampRequest(req.DER)
	if err != nil || parsed.Hash != req.Hash || parsed.Nonce.Cmp(req.Nonce) != 0 || !parsed.CertReq || !parsed.ForSignature(sig) {
		t.Fatalf("parsed request %+v: %v", parsed, err)
	}
	if parsed.ForSignature(Signature{Signature: []byte{1, 2}}) {
		t.Fatal("request matches another signature")
	}
	if _, err := ParseTimestampRequest(req.DER[:10]); err == nil {
		t.Fatal("truncated request parsed")
	}
	if _, err := NewTimestampRequest(sig, TimestampRequestOptions{Hash: crypto.MD5}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("MD5 imprint: %v", err)
	}
	if _, err := NewTimestampRequest(Signature{}, TimestampRequestOptions{}); !errors.Is(err, ErrMalformedSignature) {
		t.Fatalf("empty signature: %v", err)
	}
}
