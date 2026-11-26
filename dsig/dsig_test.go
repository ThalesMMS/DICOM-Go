package dsig

import (
	"bytes"
	"crypto"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/internal/testutil"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var (
	tagPatientName      = core.NewTag(0x0010, 0x0010)
	tagReferencedStudy  = core.NewTag(0x0008, 0x1110)
	tagPixelData        = core.NewTag(0x7FE0, 0x0010)
	testSOPInstanceUID  = "1.2.826.0.1.3680043.10.543.998.1"
	testStudyUID        = "1.2.826.0.1.3680043.10.543.998.2"
	testSeriesUID       = "1.2.826.0.1.3680043.10.543.998.3"
	secondaryCaptureUID = "1.2.840.10008.5.1.4.1.1.7"
)

// testElements is a synthetic Secondary Capture with a sequence and either
// native or encapsulated Pixel Data.
func testElements(encapsulated bool) []core.Element {
	elements := []core.Element{
		uiElement(core.NewTag(0x0008, 0x0016), secondaryCaptureUID),
		uiElement(core.NewTag(0x0008, 0x0018), testSOPInstanceUID),
		dicomtest.StringElement(core.NewTag(0x0008, 0x0070), core.VRLO, "SYNTHETIC"),
		sequenceElement(tagReferencedStudy, core.DataSet{Elements: []core.Element{
			uiElement(core.NewTag(0x0008, 0x1150), "1.2.840.10008.3.1.2.3.1"),
			uiElement(core.NewTag(0x0008, 0x1155), "1.2.826.0.1.3680043.10.543.998.9"),
		}}),
		dicomtest.StringElement(tagPatientName, core.VRPN, "TEST^SIGN"),
		dicomtest.StringElement(core.NewTag(0x0010, 0x0020), core.VRLO, "SIGN998"),
		uiElement(core.NewTag(0x0020, 0x000D), testStudyUID),
		uiElement(core.NewTag(0x0020, 0x000E), testSeriesUID),
		usTestElement(core.NewTag(0x0028, 0x0002), 1),
		dicomtest.StringElement(core.NewTag(0x0028, 0x0004), core.VRCS, "MONOCHROME2"),
		usTestElement(core.NewTag(0x0028, 0x0010), 4),
		usTestElement(core.NewTag(0x0028, 0x0011), 4),
		usTestElement(core.NewTag(0x0028, 0x0100), 8),
		usTestElement(core.NewTag(0x0028, 0x0101), 8),
		usTestElement(core.NewTag(0x0028, 0x0102), 7),
		usTestElement(core.NewTag(0x0028, 0x0103), 0),
	}
	if encapsulated {
		return append(elements, core.Element{Header: core.ElementHeader{Tag: tagPixelData, VR: core.VROB, Length: core.UndefinedLength, LengthSet: true}, Value: core.FragmentSequence{Fragments: [][]byte{{1, 2, 3, 4, 5, 6, 7, 8}, {9, 10, 11, 12, 13, 14, 15, 16}}}})
	}
	return append(elements, core.NewRawElement(tagPixelData, core.VROB, bytes.Repeat([]byte{0x5A}, 16)))
}

func testSyntax(encapsulated bool) transfer.Syntax {
	if encapsulated {
		return transfer.EncapsulatedUncompressedExplicitVRLittleEndian
	}
	return transfer.ExplicitVRLittleEndian
}

// roundTrip writes obj as a Part 10 file and reads it back, so verification
// works on parsed (raw) values rather than the ones Sign built.
func roundTrip(t *testing.T, obj *object.Object, syntax transfer.Syntax) *object.Object {
	t.Helper()
	data, err := dicomtest.Part10File(syntax, sortedElements(obj.Elements())...)
	if err != nil {
		t.Fatal(err)
	}
	file, err := object.ReadFile(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return file.Dataset
}

type signer struct {
	cred testutil.TestCredential
	leaf *x509.Certificate
}

func newSigner(t *testing.T, pki *testutil.TestPKI, name, kind string) signer {
	t.Helper()
	cred := pki.Issue(t, name, kind)
	leaf, err := x509.ParseCertificate(cred.Certificate.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return signer{cred: cred, leaf: leaf}
}

func (s signer) options(algorithm string) SignOptions {
	return s.optionsFor(algorithm, false)
}

// optionsFor sets the MAC Calculation Transfer Syntax to the stored one when
// Pixel Data is encapsulated, as PS3.15 requires.
func (s signer) optionsFor(algorithm string, encapsulated bool) SignOptions {
	opts := s.optionsNative(algorithm)
	if encapsulated {
		opts.MACTransferSyntaxUID = testSyntax(true).UID
	}
	return opts
}

func (s signer) optionsNative(algorithm string) SignOptions {
	return SignOptions{
		Certificate:  s.leaf,
		Signer:       s.cred.Certificate.PrivateKey.(crypto.Signer),
		MACAlgorithm: algorithm,
		// Inside the test certificate's validity, which dcmsign checks: a
		// fixed date failed once the certificates were issued after it.
		Time: s.leaf.NotBefore.Add(time.Minute),
	}
}

func uiElement(tag core.Tag, value string) core.Element {
	return dicomtest.StringElement(tag, core.VRUI, value)
}

func usTestElement(tag core.Tag, value uint16) core.Element {
	return dicomtest.Uint16Element(tag, core.VRUS, binary.LittleEndian, value)
}

func sequenceElement(tag core.Tag, items ...core.DataSet) core.Element {
	return core.Element{
		Header: core.ElementHeader{Tag: tag, VR: core.VRSQ, Length: core.UndefinedLength, LengthSet: true},
		Value:  core.SequenceValue{Items: items},
	}
}

func newObject(elements []core.Element, syntax transfer.Syntax) *object.Object {
	return object.FromElements(elements, std.Dictionary)
}

func signedObject(t *testing.T, s signer, opts SignOptions, encapsulated bool) *object.Object {
	t.Helper()
	obj := newObject(testElements(encapsulated), testSyntax(encapsulated))
	if _, err := Sign(obj, opts); err != nil {
		t.Fatal(err)
	}
	return roundTrip(t, obj, testSyntax(encapsulated))
}

func requireValid(t *testing.T, obj *object.Object, want int) []Result {
	t.Helper()
	results := Verify(obj)
	if len(results) != want {
		t.Fatalf("%d signatures, want %d", len(results), want)
	}
	for _, result := range results {
		if !result.Valid() {
			t.Fatalf("signature %s invalid: %v", result.Signature.UID, result.Err)
		}
	}
	return results
}

func TestSignVerifyRoundTrip(t *testing.T) {
	pki := testutil.NewTestPKI(t)
	rsaSigner := newSigner(t, pki, "signer-rsa.test", "rsa2048")
	for _, algorithm := range []string{MACRIPEMD160, MACMD5, MACSHA1, MACSHA256, MACSHA384, MACSHA512} {
		for _, encapsulated := range []bool{false, true} {
			obj := signedObject(t, rsaSigner, rsaSigner.optionsFor(algorithm, encapsulated), encapsulated)
			results := requireValid(t, obj, 1)
			if got := results[0].Signature; got.MACAlgorithm != algorithm || got.Certificate == nil || len(got.DataElementsSigned) != len(testElements(encapsulated)) {
				t.Fatalf("%s: signature %+v", algorithm, got)
			}
		}
	}
	implicit := rsaSigner.options(MACSHA256)
	implicit.MACTransferSyntaxUID = transfer.ImplicitVRLittleEndian.UID
	requireValid(t, signedObject(t, rsaSigner, implicit, false), 1)
	ecSigner := newSigner(t, pki, "signer-ec.test", "p256")
	requireValid(t, signedObject(t, ecSigner, ecSigner.options(MACSHA256), false), 1)
}

// TestVerifyDetectsTampering: a changed byte, a removed element or a swapped
// certificate always invalidates the signature; unsigned elements may change.
func TestVerifyDetectsTampering(t *testing.T) {
	pki := testutil.NewTestPKI(t)
	s := newSigner(t, pki, "signer-rsa.test", "rsa2048")
	opts := s.options(MACSHA256)
	opts.Tags = []core.Tag{core.NewTag(0x0008, 0x0016), core.NewTag(0x0008, 0x0018), tagReferencedStudy, tagPatientName, tagPixelData}
	base := func() *object.Object { return signedObject(t, s, opts, false) }
	expectInvalid := func(name string, obj *object.Object) {
		t.Helper()
		results := Verify(obj)
		if len(results) != 1 || results[0].Valid() {
			t.Fatalf("%s: tampering not detected: %+v", name, results)
		}
	}

	obj := base()
	obj.Put(dicomtest.StringElement(tagPatientName, core.VRPN, "TEST^SIGM"))
	expectInvalid("changed name", obj)

	obj = base()
	raw, _ := obj.GetRaw(tagPixelData)
	changed := append([]byte(nil), raw...)
	changed[3] ^= 0x01
	obj.Put(core.NewRawElement(tagPixelData, core.VROB, changed))
	expectInvalid("changed pixel byte", obj)

	obj = base()
	obj.Remove(tagPatientName)
	expectInvalid("removed element", obj)

	obj = base()
	items, _ := obj.GetSequence(tagReferencedStudy)
	nested := items[0].Elements()
	nested[1] = uiElement(core.NewTag(0x0008, 0x1155), "1.2.826.0.1.3680043.10.543.998.8")
	obj.Put(sequenceElement(tagReferencedStudy, core.DataSet{Elements: nested}))
	expectInvalid("changed nested value", obj)

	other := newSigner(t, pki, "other-rsa.test", "rsa2048")
	obj = base()
	elements := obj.Elements()
	for i, element := range elements {
		if element.Tag() != tagDigitalSignaturesSequence {
			continue
		}
		sequence := element.Value.(core.SequenceValue)
		item := sequence.Items[0]
		for j, nested := range item.Elements {
			if nested.Tag() == tagCertificateOfSigner {
				item.Elements[j] = core.NewRawElement(tagCertificateOfSigner, core.VROB, other.leaf.Raw)
			}
		}
		obj.Put(elements[i])
	}
	expectInvalid("wrong certificate", obj)

	obj = base()
	obj.Put(dicomtest.StringElement(core.NewTag(0x0010, 0x0020), core.VRLO, "CHANGED"))
	requireValid(t, obj, 1)

	obj = base()
	obj.Remove(tagMACParametersSequence)
	if results := Verify(obj); len(results) != 1 || !errors.Is(results[0].Err, ErrMalformedSignature) {
		t.Fatalf("missing MAC Parameters: %+v", results)
	}
}

func TestMultipleAndNestedSignatures(t *testing.T) {
	pki := testutil.NewTestPKI(t)
	s := newSigner(t, pki, "signer-rsa.test", "rsa2048")
	obj := newObject(testElements(false), transfer.ExplicitVRLittleEndian)
	first, err := Sign(obj, s.options(MACSHA256))
	if err != nil {
		t.Fatal(err)
	}
	opts := s.options(MACRIPEMD160)
	opts.Tags = []core.Tag{core.NewTag(0x0008, 0x0018), tagPatientName}
	second, err := Sign(obj, opts)
	if err != nil {
		t.Fatal(err)
	}
	if first.MACIDNumber != 1 || second.MACIDNumber != 2 || first.UID == second.UID {
		t.Fatalf("MAC IDs %d/%d UIDs %s/%s", first.MACIDNumber, second.MACIDNumber, first.UID, second.UID)
	}
	requireValid(t, roundTrip(t, obj, transfer.ExplicitVRLittleEndian), 2)

	// A signature held in a sequence item signs that item's data set.
	inner := newObject([]core.Element{
		uiElement(core.NewTag(0x0008, 0x1150), "1.2.840.10008.3.1.2.3.1"),
		uiElement(core.NewTag(0x0008, 0x1155), "1.2.826.0.1.3680043.10.543.998.9"),
	}, transfer.ExplicitVRLittleEndian)
	if _, err := Sign(inner, s.options(MACSHA256)); err != nil {
		t.Fatal(err)
	}
	outer := newObject(append(testElements(false)[:3], sequenceElement(tagReferencedStudy, core.DataSet{Elements: inner.Elements()})), transfer.ExplicitVRLittleEndian)
	results := requireValid(t, roundTrip(t, outer, transfer.ExplicitVRLittleEndian), 1)
	if results[0].Signature.Path != "(0008,1110)[0]" {
		t.Fatalf("nested signature path %q", results[0].Signature.Path)
	}
}

func TestCheckProfile(t *testing.T) {
	pki := testutil.NewTestPKI(t)
	s := newSigner(t, pki, "signer-rsa.test", "rsa2048")
	all := requireValid(t, signedObject(t, s, s.options(MACSHA256), false), 1)[0].Signature
	for _, profile := range []Profile{ProfileBaseRSA, ProfileCreatorRSA} {
		if err := CheckProfile(all, profile); err != nil {
			t.Fatalf("%s: %v", profile, err)
		}
	}
	if err := CheckProfile(all, ProfileAuthorizationRSA); !errors.Is(err, ErrProfileIncomplete) {
		t.Fatalf("legacy auth must fail closed: %v", err)
	}
	if _, err := CheckProfileWithOptions(all, ProfileAuthorizationRSA, ProfileOptions{HumanVerifiedPaths: []string{}}); err != nil {
		t.Fatal(err)
	}
	opts := s.options(MACSHA256)
	opts.Tags = []core.Tag{core.NewTag(0x0008, 0x0016), core.NewTag(0x0008, 0x0018), core.NewTag(0x0020, 0x000D), core.NewTag(0x0020, 0x000E)}
	partial := requireValid(t, signedObject(t, s, opts, false), 1)[0].Signature
	if err := CheckProfile(partial, ProfileCreatorRSA); !errors.Is(err, ErrProfileViolation) {
		t.Fatalf("creator profile without Image Pixel and General Equipment: %v", err)
	}
	if err := CheckProfile(partial, ProfileBaseRSA); err != nil {
		t.Fatalf("base profile: %v", err)
	}
	if err := CheckProfile(all, ProfileSRRSA); !errors.Is(err, ErrProfileViolation) {
		t.Fatalf("SR profile without a purpose code: %v", err)
	}
	ec := newSigner(t, pki, "signer-ec.test", "p256")
	ecSig := requireValid(t, signedObject(t, ec, ec.options(MACSHA256), false), 1)[0].Signature
	if err := CheckProfile(ecSig, ProfileBaseRSA); !errors.Is(err, ErrProfileViolation) {
		t.Fatalf("RSA profile with an ECDSA key: %v", err)
	}
	opts = s.options(MACSHA256)
	opts.Tags = []core.Tag{core.NewTag(0x0008, 0x0016)}
	opts.Profile = ProfileCreatorRSA
	obj := newObject(testElements(false), transfer.ExplicitVRLittleEndian)
	if _, err := Sign(obj, opts); !errors.Is(err, ErrProfileViolation) {
		t.Fatalf("Sign with a violated profile: %v", err)
	}
	if obj.Has(tagDigitalSignaturesSequence) {
		t.Fatal("a refused signature changed the object")
	}
}

func TestSignRules(t *testing.T) {
	pki := testutil.NewTestPKI(t)
	s := newSigner(t, pki, "signer-rsa.test", "rsa2048")
	obj := newObject(testElements(true), testSyntax(true))
	if _, err := Sign(obj, s.options(MACSHA256)); !errors.Is(err, ErrEncapsulatedMACSyntax) {
		t.Fatalf("encapsulated Pixel Data with a native MAC syntax: %v", err)
	}
	for _, uid := range []string{transfer.ExplicitVRBigEndian.UID, transfer.DeflatedExplicitVRLittleEndian.UID, "1.2.3"} {
		opts := s.options(MACSHA256)
		opts.MACTransferSyntaxUID = uid
		if _, err := Sign(newObject(testElements(false), transfer.ExplicitVRLittleEndian), opts); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("MAC syntax %s: %v", uid, err)
		}
	}
	opts := s.options(MACSHA256)
	opts.Tags = []core.Tag{core.NewTag(0x0002, 0x0010)}
	if _, err := Sign(newObject(testElements(false), transfer.ExplicitVRLittleEndian), opts); err == nil {
		t.Fatal("signed a tag that is not in the data set")
	}

	// The byte stream follows ascending tag order, not insertion order.
	elements := testElements(false)
	reversed := make([]core.Element, 0, len(elements))
	for i := len(elements) - 1; i >= 0; i-- {
		reversed = append(reversed, elements[i])
	}
	obj = newObject(reversed, transfer.ExplicitVRLittleEndian)
	sig, err := Sign(obj, s.options(MACSHA256))
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(sig.DataElementsSigned); i++ {
		if !tagLess(sig.DataElementsSigned[i-1], sig.DataElementsSigned[i]) {
			t.Fatalf("Data Elements Signed out of order: %v", sig.DataElementsSigned)
		}
	}
	requireValid(t, roundTrip(t, obj, transfer.ExplicitVRLittleEndian), 1)
}
