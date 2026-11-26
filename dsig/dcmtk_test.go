package dsig

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/internal/testutil"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// requireDCMSign returns the path of an OpenSSL-enabled DCMTK dcmsign,
// skipping unless DICOMGO_DCMTK_INTEGRATION=1 and dcmsign is installed.
func requireDCMSign(t *testing.T) string {
	t.Helper()
	return dcmtkoracle.Tool(t, "dcmsign")
}

// Tolerances of the dcmsign oracle runs.
const (
	signedVerifies   = "dcmsign signature verifies here with the requested MAC and profile"
	tamperedRejected = "dcmsign --verify rejects the tampered file (non-zero exit)"
)

func writeTestFile(t *testing.T, name string, syntax transfer.Syntax, elements []core.Element) string {
	t.Helper()
	data, err := dicomtest.Part10File(syntax, sortedElements(elements)...)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readTestFile(t *testing.T, path string) *object.Object {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	file, err := object.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	return file.Dataset
}

func dcmsign(t *testing.T, tool, tolerance string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd := dcmtkoracle.Command(context.Background(), t, tolerance, tool, args...)
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

// TestDCMSignSignaturesVerify is opt-in evidence for #998: signatures DCMTK
// dcmsign creates (every MAC algorithm, tag subsets, a sequence item, the
// Creator profile, encapsulated Pixel Data, Implicit VR files) verify here and
// meet the profile they were made for. Enable with
// DICOMGO_DCMTK_INTEGRATION=1 and an OpenSSL-enabled DCMTK.
func TestDCMSignSignaturesVerify(t *testing.T) {
	tool := requireDCMSign(t)
	pki := testutil.NewTestPKI(t)
	cred := pki.Issue(t, "dcmsign.test", "rsa2048")
	sign := func(t *testing.T, in string, args ...string) *object.Object {
		t.Helper()
		out := filepath.Join(t.TempDir(), "signed.dcm")
		all := append([]string{"-pw"}, args...)
		all = append(all, in, out)
		if log, err := dcmsign(t, tool, signedVerifies, all...); err != nil {
			t.Fatalf("dcmsign %v: %v\n%s", args, err, log)
		}
		return readTestFile(t, out)
	}
	for _, tc := range []struct {
		name         string
		option       string
		algorithm    string
		encapsulated bool
		syntax       transfer.Syntax
	}{
		{"RIPEMD160", "+mr", MACRIPEMD160, false, transfer.ExplicitVRLittleEndian},
		{"MD5", "+mm", MACMD5, false, transfer.ExplicitVRLittleEndian},
		{"SHA1", "+ms", MACSHA1, false, transfer.ExplicitVRLittleEndian},
		{"SHA256", "+m2", MACSHA256, false, transfer.ExplicitVRLittleEndian},
		{"SHA384", "+m3", MACSHA384, true, transfer.EncapsulatedUncompressedExplicitVRLittleEndian},
		{"SHA512", "+m5", MACSHA512, false, transfer.ImplicitVRLittleEndian},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := writeTestFile(t, "in.dcm", tc.syntax, testElements(tc.encapsulated))
			obj := sign(t, in, "+s", cred.KeyFile, cred.CertFile, tc.option)
			results := requireValid(t, obj, 1)
			if got := results[0].Signature.MACAlgorithm; got != tc.algorithm {
				t.Fatalf("MAC Algorithm %q, want %q", got, tc.algorithm)
			}
			// dcmsign must see a change, too: the oracle is not vacuous.
			obj.Put(dicomtest.StringElement(tagPatientName, core.VRPN, "TEST^SIGM"))
			tampered := writeTestFile(t, "tampered.dcm", tc.syntax, obj.Elements())
			if log, err := dcmsign(t, tool, tamperedRejected, "--verify", tampered); err == nil {
				t.Fatalf("dcmsign accepted a tampered file:\n%s", log)
			}
			if results := Verify(readTestFile(t, tampered)); len(results) != 1 || results[0].Valid() {
				t.Fatalf("tampered file verified: %+v", results)
			}
		})
	}

	t.Run("tag subset", func(t *testing.T) {
		in := writeTestFile(t, "in.dcm", transfer.ExplicitVRLittleEndian, testElements(false))
		obj := sign(t, in, "+s", cred.KeyFile, cred.CertFile, "+m2", "-t", "0010,0010", "-t", "0008,1110", "-t", "7fe0,0010")
		results := requireValid(t, obj, 1)
		if got := results[0].Signature.DataElementsSigned; len(got) != 3 {
			t.Fatalf("Data Elements Signed %v", got)
		}
		obj.Put(dicomtest.StringElement(core.NewTag(0x0010, 0x0020), core.VRLO, "UNSIGNED"))
		requireValid(t, obj, 1)
	})

	t.Run("sequence item", func(t *testing.T) {
		in := writeTestFile(t, "in.dcm", transfer.ExplicitVRLittleEndian, testElements(false))
		obj := sign(t, in, "+si", cred.KeyFile, cred.CertFile, "ReferencedStudySequence[0]", "+m2")
		results := requireValid(t, obj, 1)
		if results[0].Signature.Path != "(0008,1110)[0]" {
			t.Fatalf("path %q", results[0].Signature.Path)
		}
	})

	t.Run("creator profile and purpose", func(t *testing.T) {
		in := writeTestFile(t, "in.dcm", transfer.ExplicitVRLittleEndian, testElements(false))
		obj := sign(t, in, "+s", cred.KeyFile, cred.CertFile, "+m2", "+pc", "+sp", "1")
		sig := requireValid(t, obj, 1)[0].Signature
		if err := CheckProfile(sig, ProfileCreatorRSA); err != nil {
			t.Fatal(err)
		}
		if len(sig.Purposes) != 1 || sig.Purposes[0].Value != "1" {
			t.Fatalf("purposes %+v", sig.Purposes)
		}
	})

	t.Run("two signatures", func(t *testing.T) {
		in := writeTestFile(t, "in.dcm", transfer.ExplicitVRLittleEndian, testElements(false))
		first := filepath.Join(t.TempDir(), "first.dcm")
		if log, err := dcmsign(t, tool, signedVerifies, "-pw", "+s", cred.KeyFile, cred.CertFile, "+m2", in, first); err != nil {
			t.Fatalf("%v\n%s", err, log)
		}
		requireValid(t, sign(t, first, "+s", cred.KeyFile, cred.CertFile, "+mr", "-t", "0010,0010"), 2)
	})
}

// TestSignaturesVerifyWithDCMSign is the other direction: signatures made
// here pass dcmsign --verify with the test CA trusted, including the Creator
// profile check (+rc). Enable with DICOMGO_DCMTK_INTEGRATION=1.
func TestSignaturesVerifyWithDCMSign(t *testing.T) {
	tool := requireDCMSign(t)
	pki := testutil.NewTestPKI(t)
	s := newSigner(t, pki, "go-signer.test", "rsa2048")
	ec := newSigner(t, pki, "go-signer-ec.test", "p256")
	verify := func(t *testing.T, obj *object.Object, syntax transfer.Syntax, extra ...string) {
		t.Helper()
		path := writeTestFile(t, "signed.dcm", syntax, obj.Elements())
		args := append([]string{"--verify", "+rg", "+cf", pki.CAFile}, extra...)
		log, err := dcmsign(t, tool, dcmtkoracle.Accepted, append(args, path)...)
		if err != nil {
			t.Fatalf("dcmsign --verify: %v\n%s", err, log)
		}
	}
	for _, algorithm := range []string{MACRIPEMD160, MACMD5, MACSHA1, MACSHA256, MACSHA384, MACSHA512} {
		for _, encapsulated := range []bool{false, true} {
			t.Run(algorithm, func(t *testing.T) {
				verify(t, signedObject(t, s, s.optionsFor(algorithm, encapsulated), encapsulated), testSyntax(encapsulated))
			})
		}
	}
	t.Run("implicit MAC syntax and tag subset", func(t *testing.T) {
		opts := s.options(MACSHA256)
		opts.MACTransferSyntaxUID = transfer.ImplicitVRLittleEndian.UID
		opts.Tags = []core.Tag{tagReferencedStudy, tagPatientName, tagPixelData}
		verify(t, signedObject(t, s, opts, false), transfer.ExplicitVRLittleEndian)
	})
	t.Run("implicit VR file", func(t *testing.T) {
		// Implicit VR files carry no VR: sign the object as read, so Pixel
		// Data has the OW VR every reader gives it.
		obj := roundTrip(t, newObject(testElements(false), transfer.ImplicitVRLittleEndian), transfer.ImplicitVRLittleEndian)
		if _, err := Sign(obj, s.options(MACSHA256)); err != nil {
			t.Fatal(err)
		}
		verify(t, roundTrip(t, obj, transfer.ImplicitVRLittleEndian), transfer.ImplicitVRLittleEndian)
	})
	t.Run("creator profile", func(t *testing.T) {
		opts := s.options(MACSHA256)
		opts.Profile = ProfileCreatorRSA
		opts.Purpose = &Code{Value: "1", Scheme: "ASTM-sigpurpose", Meaning: "Author's Signature"}
		verify(t, signedObject(t, s, opts, false), transfer.ExplicitVRLittleEndian, "+rc")
	})
	t.Run("sequence item", func(t *testing.T) {
		// #1175: signatures placed in items, one and two levels down.
		obj := newObject(nestedElements(), transfer.ExplicitVRLittleEndian)
		for _, item := range []string{"ReferencedStudySequence[0]", "ContentSequence[0].ContentSequence[1]"} {
			opts := s.options(MACSHA256)
			opts.Item = item
			if _, err := Sign(obj, opts); err != nil {
				t.Fatal(err)
			}
		}
		verify(t, roundTrip(t, obj, transfer.ExplicitVRLittleEndian), transfer.ExplicitVRLittleEndian)
	})
	t.Run("ECDSA", func(t *testing.T) {
		verify(t, signedObject(t, ec, ec.options(MACSHA256), false), transfer.ExplicitVRLittleEndian)
	})
	t.Run("tampered", func(t *testing.T) {
		obj := signedObject(t, s, s.options(MACSHA256), false)
		obj.Put(dicomtest.StringElement(tagPatientName, core.VRPN, "TEST^SIGM"))
		path := writeTestFile(t, "tampered.dcm", transfer.ExplicitVRLittleEndian, obj.Elements())
		if log, err := dcmsign(t, tool, tamperedRejected, "--verify", "+cf", pki.CAFile, path); err == nil {
			t.Fatalf("dcmsign accepted a tampered file:\n%s", log)
		}
	})
}
