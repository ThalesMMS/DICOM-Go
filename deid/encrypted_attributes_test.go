package deid

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// Synthetic recipients: keys and certificates are generated per test run and
// never written to the repository.

type testRecipient struct {
	key         *rsa.PrivateKey
	certificate *x509.Certificate
}

func (r testRecipient) recipientKey() RecipientKey {
	return RecipientKey{Certificate: r.certificate, PrivateKey: r.key}
}

var (
	testRecipientsOnce sync.Once
	testRecipients     [3]testRecipient
)

// syntheticRecipients returns three distinct 2048-bit RSA recipients.
func syntheticRecipients(t testing.TB) [3]testRecipient {
	t.Helper()
	testRecipientsOnce.Do(func() {
		for index := range testRecipients {
			key, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				panic(err)
			}
			testRecipients[index] = testRecipient{key: key, certificate: syntheticCertificate(t, key, int64(index+1), x509.KeyUsageKeyEncipherment)}
		}
	})
	return testRecipients
}

func syntheticCertificate(t testing.TB, key *rsa.PrivateKey, serial int64, usage x509.KeyUsage) *x509.Certificate {
	t.Helper()
	publicDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	ski := sha1.Sum(publicDER)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "Synthetic Research Recipient"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     usage,
		SubjectKeyId: ski[:],
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return certificate
}

const (
	reversiblePatientName = "SYNTHETIC^ÁLVARO^REVERSIBLE"
	reversiblePatientID   = "SYN-REV-0042"
	reversibleBirthDate   = "19700101"
	reversibleStudyUID    = "1.2.826.0.1.3680043.10.1177.1"
	reversibleSOPUID      = "1.2.826.0.1.3680043.10.1177.1.1"
	reversibleRefUID      = "1.2.826.0.1.3680043.10.1177.1.2"
	reversiblePrivate     = "SYNTHETIC PRIVATE PAYLOAD"
)

var reversibleSecrets = []string{"SYNTHETIC^", "ÁLVARO", "SYN-REV-0042", reversibleBirthDate, reversibleStudyUID, reversibleSOPUID, reversibleRefUID, reversiblePrivate, "SYNTHETIC STUDY"}

func reversibleSource() *object.Object {
	obj := object.FromElements([]core.Element{
		core.NewRawElement(tagSpecificCharacterSet, core.VRCS, padText("ISO_IR 192")),
		core.NewRawElement(tagSOPClassUID, core.VRUI, padUID("1.2.840.10008.5.1.4.1.1.7")),
		core.NewRawElement(testTagSOPInstanceUID, core.VRUI, padUID(reversibleSOPUID)),
		core.NewRawElement(testTagStudyDate, core.VRDA, padText("20240115")),
		core.NewRawElement(core.NewTag(0x0008, 0x1030), core.VRLO, padText("SYNTHETIC STUDY")),
		sequenceElement(testTagNestedSequence, core.DataSet{Elements: []core.Element{
			core.NewRawElement(testTagReferencedSOPUID, core.VRUI, padUID(reversibleRefUID)),
		}}),
		core.NewRawElement(testTagPatientName, core.VRPN, padText(reversiblePatientName)),
		core.NewRawElement(testTagPatientID, core.VRLO, padText(reversiblePatientID)),
		core.NewRawElement(testTagPatientBirthDate, core.VRDA, padText(reversibleBirthDate)),
		core.NewRawElement(testTagPrivateCreator, core.VRLO, padText("SYNTHETIC CREATOR")),
		core.NewRawElement(testTagPrivateData, core.VRLO, padText(reversiblePrivate)),
		core.NewRawElement(testTagStudyInstanceUID, core.VRUI, padUID(reversibleStudyUID)),
	}, std.Dictionary)
	return obj
}

func encodeExplicitLE(t testing.TB, obj *object.Object) []byte {
	t.Helper()
	var buffer bytes.Buffer
	if err := object.WriteDataSet(&buffer, obj, transfer.ExplicitVRLittleEndian); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func reversibleOptions(options EncryptedAttributesOptions) BasicProfileOptions {
	profile := DefaultBasicProfileOptions()
	profile.EncryptedAttributes = &options
	return profile
}

func TestReversibleBasicProfileRoundTripRestoresOriginals(t *testing.T) {
	recipients := syntheticRecipients(t)
	cases := []struct {
		name    string
		options EncryptedAttributesOptions
		want    EncryptedAttributesReport
	}{
		{name: "default OAEP AES-256", options: EncryptedAttributesOptions{}, want: EncryptedAttributesReport{KeyTransport: KeyTransportRSAOAEPSHA256, ContentEncryption: ContentEncryptionAES256CBC}},
		{name: "PKCS1 v1.5 AES-128", options: EncryptedAttributesOptions{KeyTransport: KeyTransportRSAPKCS1v15, ContentEncryption: ContentEncryptionAES128CBC}, want: EncryptedAttributesReport{KeyTransport: KeyTransportRSAPKCS1v15, ContentEncryption: ContentEncryptionAES128CBC}},
		{name: "subject key identifier AES-192", options: EncryptedAttributesOptions{SubjectKeyIdentifier: true, ContentEncryption: ContentEncryptionAES192CBC}, want: EncryptedAttributesReport{KeyTransport: KeyTransportRSAOAEPSHA256, ContentEncryption: ContentEncryptionAES192CBC}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			source := reversibleSource()
			original := encodeExplicitLE(t, source)
			test.options.Recipients = []*x509.Certificate{recipients[0].certificate, recipients[1].certificate}
			report, err := ApplyBasicProfile(context.Background(), source, reversibleOptions(test.options), nil)
			if err != nil {
				t.Fatalf("ApplyBasicProfile: %v", err)
			}
			encrypted := report.EncryptedAttributes
			if encrypted == nil || encrypted.Recipients != 2 || encrypted.Attributes == 0 ||
				encrypted.KeyTransport != test.want.KeyTransport || encrypted.ContentEncryption != test.want.ContentEncryption ||
				encrypted.TransferSyntaxUID != transfer.ExplicitVRLittleEndian.UID {
				t.Fatalf("encrypted attributes report = %+v", encrypted)
			}
			deidentified := encodeExplicitLE(t, source)
			reportJSON, _ := json.Marshal(report)
			for _, secret := range reversibleSecrets {
				if bytes.Contains(deidentified, []byte(secret)) || bytes.Contains(reportJSON, []byte(secret)) {
					t.Fatalf("de-identified output or report exposes %q", secret)
				}
			}
			items, ok := source.GetSequence(tagEncryptedAttributesSequence)
			if !ok || len(items) != 1 {
				t.Fatalf("Encrypted Attributes Sequence items = %d", len(items))
			}
			if uid, _ := items[0].GetUID(tagEncryptedContentTransferSyntaxUID); uid != transfer.ExplicitVRLittleEndian.UID {
				t.Fatalf("Encrypted Content Transfer Syntax UID = %q", uid)
			}
			methods, _ := source.GetStrings(tagDeidentificationMethod)
			if !strings.Contains(strings.Join(methods, "|"), encryptedAttributesDeidentificationMethod) {
				t.Fatalf("De-identification Method = %q", methods)
			}

			restored, err := DecryptEncryptedAttributes(source, recipients[1].recipientKey())
			if err != nil {
				t.Fatalf("DecryptEncryptedAttributes: %v", err)
			}
			if name, _ := restored.GetString(testTagPatientName); strings.TrimSpace(name) != reversiblePatientName {
				t.Fatalf("decrypted Patient's Name = %q", name)
			}
			if !bytes.Equal(encodeExplicitLE(t, source), deidentified) {
				t.Fatal("DecryptEncryptedAttributes changed the object")
			}

			reidentified, err := Reidentify(context.Background(), source, recipients[1].recipientKey())
			if err != nil {
				t.Fatalf("Reidentify: %v", err)
			}
			if reidentified.Restored != encrypted.Attributes || reidentified.MarkersRemoved != 4 || reidentified.RemainingItems != 0 ||
				reidentified.KeyTransport != test.want.KeyTransport || reidentified.ContentEncryption != test.want.ContentEncryption {
				t.Fatalf("re-identification report = %+v (stored %d)", reidentified, encrypted.Attributes)
			}
			if got := encodeExplicitLE(t, source); !bytes.Equal(got, original) {
				t.Fatalf("re-identified data set differs from the original:\n got %q\nwant %q", got, original)
			}
		})
	}
}

func TestReversibleDeidentificationWithoutTheKeyRecoversNothing(t *testing.T) {
	recipients := syntheticRecipients(t)
	for _, transport := range []KeyTransport{KeyTransportRSAOAEPSHA256, KeyTransportRSAPKCS1v15} {
		t.Run(string(transport), func(t *testing.T) {
			source := reversibleSource()
			options := EncryptedAttributesOptions{Recipients: []*x509.Certificate{recipients[0].certificate}, KeyTransport: transport}
			if _, err := ApplyBasicProfile(context.Background(), source, reversibleOptions(options), nil); err != nil {
				t.Fatal(err)
			}
			deidentified := encodeExplicitLE(t, source)

			// A recipient the data was not encrypted for.
			if _, err := Reidentify(context.Background(), source, recipients[2].recipientKey()); !errors.Is(err, ErrRecipientNotFound) {
				t.Fatalf("other recipient: %v, want ErrRecipientNotFound", err)
			}
			// A key that does not belong to the certificate.
			mismatched := RecipientKey{Certificate: recipients[0].certificate, PrivateKey: recipients[2].key}
			if _, err := Reidentify(context.Background(), source, mismatched); !errors.Is(err, ErrRecipientKey) {
				t.Fatalf("mismatched key: %v, want ErrRecipientKey", err)
			}
			// A forged certificate that names the recipient (same issuer and
			// serial number) over another key.
			forged := RecipientKey{Certificate: syntheticCertificate(t, recipients[2].key, 1, x509.KeyUsageKeyEncipherment), PrivateKey: recipients[2].key}
			if _, err := Reidentify(context.Background(), source, forged); !errors.Is(err, ErrEncryptedAttributesDecrypt) {
				t.Fatalf("forged recipient: %v, want ErrEncryptedAttributesDecrypt", err)
			}
			if !bytes.Equal(encodeExplicitLE(t, source), deidentified) {
				t.Fatal("a failed re-identification changed the object")
			}
		})
	}
}

func TestReidentifyPeelsLayeredReversibleDeidentifications(t *testing.T) {
	recipients := syntheticRecipients(t)
	source := reversibleSource()
	original := encodeExplicitLE(t, source)
	first := EncryptedAttributesOptions{Recipients: []*x509.Certificate{recipients[0].certificate}}
	if _, err := ApplyBasicProfile(context.Background(), source, reversibleOptions(first), nil); err != nil {
		t.Fatal(err)
	}
	afterFirst := encodeExplicitLE(t, source)
	second := EncryptedAttributesOptions{Recipients: []*x509.Certificate{recipients[1].certificate}, KeyTransport: KeyTransportRSAPKCS1v15}
	if _, err := ApplyBasicProfile(context.Background(), source, reversibleOptions(second), nil); err != nil {
		t.Fatal(err)
	}
	if items, _ := source.GetSequence(tagEncryptedAttributesSequence); len(items) != 2 {
		t.Fatalf("Encrypted Attributes items = %d, want 2", len(items))
	}
	report, err := Reidentify(context.Background(), source, recipients[1].recipientKey())
	if err != nil {
		t.Fatal(err)
	}
	if report.RemainingItems != 1 {
		t.Fatalf("remaining items = %d, want 1", report.RemainingItems)
	}
	if got := encodeExplicitLE(t, source); !bytes.Equal(got, afterFirst) {
		t.Fatal("peeling the outer layer did not restore the first de-identification")
	}
	if _, err := Reidentify(context.Background(), source, recipients[0].recipientKey()); err != nil {
		t.Fatal(err)
	}
	if got := encodeExplicitLE(t, source); !bytes.Equal(got, original) {
		t.Fatal("peeling both layers did not restore the original")
	}
}

func TestReversibleFileRoundTripRebuildsMetaAndKeepsPixels(t *testing.T) {
	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.DeflatedExplicitVRLittleEndian} {
		t.Run(syntax.Name, func(t *testing.T) {
			testReversibleFileRoundTrip(t, syntax)
		})
	}
}

func testReversibleFileRoundTrip(t *testing.T, syntax transfer.Syntax) {
	t.Helper()
	recipients := syntheticRecipients(t)
	pixels := bytes.Repeat([]byte{0x11, 0x22}, 16)
	elements := append(reversibleSource().Elements(),
		usElement(testTagRows, 4), usElement(testTagColumns, 4), usElement(testTagSamplesPerPixel, 1),
		core.NewRawElement(testTagPhotometric, core.VRCS, padText("MONOCHROME2")),
		usElement(testTagBitsAllocated, 16), usElement(testTagBitsStored, 16), usElement(testTagHighBit, 15), usElement(testTagPixelRep, 0),
		core.NewRawElement(testTagPixelData, core.VROW, pixels),
	)
	var data bytes.Buffer
	if err := object.WriteFile(&data, &object.File{Dataset: object.FromElements(elements, nil), TransferSyntax: syntax}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.dcm")
	if err := os.WriteFile(sourcePath, data.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	deidentifiedPath := filepath.Join(dir, "deidentified.dcm")
	options := reversibleOptions(EncryptedAttributesOptions{Recipients: []*x509.Certificate{recipients[0].certificate}})
	if _, err := BasicProfilePath(context.Background(), sourcePath, deidentifiedPath, options, nil); err != nil {
		t.Fatalf("BasicProfilePath: %v", err)
	}
	reidentifiedPath := filepath.Join(dir, "reidentified.dcm")
	if _, err := ReidentifyPath(context.Background(), deidentifiedPath, reidentifiedPath, recipients[0].recipientKey()); err != nil {
		t.Fatalf("ReidentifyPath: %v", err)
	}
	sourceFile, err := object.OpenFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer sourceFile.Close()
	reidentified, err := object.OpenFile(reidentifiedPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reidentified.Close()
	if reidentified.TransferSyntax.UID != syntax.UID {
		t.Fatalf("re-identified transfer syntax = %q, want %q", reidentified.TransferSyntax.UID, syntax.UID)
	}
	if !bytes.Equal(encodeExplicitLE(t, reidentified.Dataset), encodeExplicitLE(t, sourceFile.Dataset)) {
		t.Fatal("re-identified file differs from the source")
	}
	if uid, _ := reidentified.Meta.GetUID(testTagMediaStorageSOPInstanceUIDForClone); uid != reversibleSOPUID {
		t.Fatalf("Media Storage SOP Instance UID = %q", uid)
	}

	deidentified, err := object.OpenFile(deidentifiedPath)
	if err != nil {
		t.Fatal(err)
	}
	defer deidentified.Close()
	clone, report, err := CloneFileReidentified(context.Background(), deidentified, recipients[0].recipientKey())
	if err != nil {
		t.Fatalf("CloneFileReidentified: %v", err)
	}
	if report.Restored == 0 || !bytes.Equal(encodeExplicitLE(t, clone.Dataset), encodeExplicitLE(t, sourceFile.Dataset)) {
		t.Fatal("CloneFileReidentified did not restore the source")
	}
	if _, ok := deidentified.Dataset.Get(tagEncryptedAttributesSequence); !ok {
		t.Fatal("CloneFileReidentified changed its source")
	}
}

func TestReversibleDeidentificationDoesNotStorePixelData(t *testing.T) {
	recipients := syntheticRecipients(t)
	source := reversibleSource()
	source.Put(core.NewRawElement(testTagPixelData, core.VROB, []byte("SYNTHETIC PIXELS")))
	plan, err := PlanBasicProfile(context.Background(), source, reversibleOptions(EncryptedAttributesOptions{Recipients: []*x509.Certificate{recipients[0].certificate}}), nil)
	if err != nil {
		t.Fatal(err)
	}
	clone := object.FromDataSet(profileCloneDataSet(source.ToDataSet()), std.Dictionary)
	if _, err := plan.Apply(context.Background(), clone); err != nil {
		t.Fatal(err)
	}
	originals, err := DecryptEncryptedAttributes(clone, recipients[0].recipientKey())
	if err != nil {
		t.Fatal(err)
	}
	if originals.Has(testTagPixelData) || !originals.Has(testTagPatientName) || !originals.Has(tagSpecificCharacterSet) {
		t.Fatal("Modified Attributes item must carry the originals and character set but never Pixel Data")
	}
	decrypted, _, err := decryptLatestItem(clone, recipients[0].recipientKey())
	if err != nil {
		t.Fatal(err)
	}
	for index := 1; index < len(decrypted.originals.Elements); index++ {
		if !decrypted.originals.Elements[index-1].Tag().Less(decrypted.originals.Elements[index].Tag()) {
			t.Fatalf("Modified Attributes item is not in ascending tag order at %s", decrypted.originals.Elements[index].Tag())
		}
	}
}

func TestEncryptedAttributesOptionsValidation(t *testing.T) {
	recipients := syntheticRecipients(t)
	smallKey, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecTemplate := &x509.Certificate{SerialNumber: big.NewInt(9), Subject: pkix.Name{CommonName: "EC"}, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}
	ecDER, err := x509.CreateCertificate(rand.Reader, ecTemplate, ecTemplate, &ecKey.PublicKey, ecKey)
	if err != nil {
		t.Fatal(err)
	}
	ecCertificate, _ := x509.ParseCertificate(ecDER)
	good := recipients[0].certificate
	cases := []struct {
		name    string
		options EncryptedAttributesOptions
		want    error
	}{
		{name: "no recipients", options: EncryptedAttributesOptions{}, want: ErrInvalidEncryptedAttributesOptions},
		{name: "nil certificate", options: EncryptedAttributesOptions{Recipients: []*x509.Certificate{nil}}, want: ErrEncryptedAttributesRecipient},
		{name: "short RSA key", options: EncryptedAttributesOptions{Recipients: []*x509.Certificate{syntheticCertificate(t, smallKey, 5, x509.KeyUsageKeyEncipherment)}}, want: ErrEncryptedAttributesRecipient},
		{name: "EC key", options: EncryptedAttributesOptions{Recipients: []*x509.Certificate{ecCertificate}}, want: ErrEncryptedAttributesRecipient},
		{name: "signing-only key usage", options: EncryptedAttributesOptions{Recipients: []*x509.Certificate{syntheticCertificate(t, recipients[0].key, 6, x509.KeyUsageDigitalSignature)}}, want: ErrEncryptedAttributesRecipient},
		{name: "unknown key transport", options: EncryptedAttributesOptions{Recipients: []*x509.Certificate{good}, KeyTransport: "rsaes-oaep-sha1"}, want: ErrInvalidEncryptedAttributesOptions},
		{name: "unknown content encryption", options: EncryptedAttributesOptions{Recipients: []*x509.Certificate{good}, ContentEncryption: "des-ede3-cbc"}, want: ErrInvalidEncryptedAttributesOptions},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if err := test.options.Validate(); !errors.Is(err, test.want) {
				t.Fatalf("Validate error = %v, want %v", err, test.want)
			}
			source := reversibleSource()
			before := encodeExplicitLE(t, source)
			if _, err := PlanBasicProfile(context.Background(), source, reversibleOptions(test.options), nil); !errors.Is(err, test.want) {
				t.Fatalf("PlanBasicProfile error = %v, want %v", err, test.want)
			}
			if !bytes.Equal(encodeExplicitLE(t, source), before) {
				t.Fatal("failed plan changed the source")
			}
		})
	}
}

func TestReidentifyRejectsMalformedEncryptedAttributes(t *testing.T) {
	recipients := syntheticRecipients(t)
	key := recipients[0].recipientKey()
	deidentified := func(t *testing.T) *object.Object {
		source := reversibleSource()
		if _, err := ApplyBasicProfile(context.Background(), source, reversibleOptions(EncryptedAttributesOptions{Recipients: []*x509.Certificate{recipients[0].certificate}}), nil); err != nil {
			t.Fatal(err)
		}
		return source
	}
	replaceItem := func(obj *object.Object, mutate func(core.DataSet) core.DataSet) {
		element, _ := obj.Get(tagEncryptedAttributesSequence)
		sequence := element.Value.(core.SequenceValue)
		obj.Put(profileSequenceElement(tagEncryptedAttributesSequence, []core.DataSet{mutate(profileCloneDataSet(sequence.Items[0]))}))
	}
	setElement := func(item core.DataSet, replacement core.Element) core.DataSet {
		for index, element := range item.Elements {
			if element.Tag() == replacement.Tag() {
				item.Elements[index] = replacement
			}
		}
		return item
	}

	if _, err := Reidentify(context.Background(), reversibleSource(), key); !errors.Is(err, ErrNoEncryptedAttributes) {
		t.Fatalf("no sequence: %v", err)
	}
	implicit := deidentified(t)
	replaceItem(implicit, func(item core.DataSet) core.DataSet {
		return setElement(item, core.NewRawElement(tagEncryptedContentTransferSyntaxUID, core.VRUI, padUID(transfer.ImplicitVRLittleEndian.UID)))
	})
	if _, err := Reidentify(context.Background(), implicit, key); !errors.Is(err, ErrUnsupportedEncryptedAttributes) {
		t.Fatalf("implicit VR content: %v", err)
	}
	tampered := deidentified(t)
	replaceItem(tampered, func(item core.DataSet) core.DataSet {
		element, _ := profileDataSetElement(item, tagEncryptedContent)
		content, _ := element.RawBytes()
		content = append([]byte(nil), content...)
		content[len(content)-20] ^= 0x5a // inside the last ciphertext block
		return setElement(item, core.NewRawElement(tagEncryptedContent, core.VROB, content))
	})
	if _, err := Reidentify(context.Background(), tampered, key); !errors.Is(err, ErrEncryptedAttributesDecrypt) && !errors.Is(err, ErrMalformedEncryptedAttributes) {
		t.Fatalf("tampered content: %v", err)
	}
	truncated := deidentified(t)
	replaceItem(truncated, func(item core.DataSet) core.DataSet {
		element, _ := profileDataSetElement(item, tagEncryptedContent)
		content, _ := element.RawBytes()
		return setElement(item, core.NewRawElement(tagEncryptedContent, core.VROB, content[:len(content)/2]))
	})
	if _, err := Reidentify(context.Background(), truncated, key); !errors.Is(err, ErrMalformedEncryptedAttributes) {
		t.Fatalf("truncated content: %v", err)
	}
}

func TestEnvelopedDataReaderAcceptsBER(t *testing.T) {
	recipients := syntheticRecipients(t)
	plaintext := []byte("synthetic plaintext that spans several AES blocks of content")
	for _, transport := range []KeyTransport{KeyTransportRSAOAEPSHA256, KeyTransportRSAPKCS1v15} {
		der, err := encryptEnvelopedData(plaintext, []cmsRecipient{{certificate: recipients[0].certificate, publicKey: &recipients[0].key.PublicKey}}, transport, ContentEncryptionAES128CBC, false, rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		ber := testIndefiniteBER(t, der)
		if bytes.Equal(ber, der) {
			t.Fatal("BER conversion did not change the encoding")
		}
		got, algorithms, err := decryptEnvelopedData(ber, recipients[0].certificate, recipients[0].key)
		if err != nil {
			t.Fatalf("%s: decrypt BER: %v", transport, err)
		}
		if !bytes.Equal(got, plaintext) || algorithms.keyTransport != transport {
			t.Fatalf("%s: BER plaintext = %q (%s)", transport, got, algorithms.keyTransport)
		}
	}
	for _, malformed := range [][]byte{{0x30, 0x80}, {0x30, 0x85, 1, 2, 3, 4, 5}, {0x04, 0x80, 0, 0}, {0x30, 0x03, 0x02, 0x01}} {
		if _, err := berToDER(malformed); !errors.Is(err, ErrMalformedEncryptedAttributes) {
			t.Fatalf("berToDER(% x) error = %v", malformed, err)
		}
	}
}

// testIndefiniteBER re-encodes DER the way a streaming encoder does: every
// constructed value with an indefinite length, and long OCTET STRINGs and
// [0] IMPLICIT OCTET STRINGs as constructed strings of two segments.
func testIndefiniteBER(t *testing.T, der []byte) []byte {
	t.Helper()
	var out []byte
	for rest := der; len(rest) > 0; {
		var value asn1.RawValue
		var err error
		rest, err = asn1.Unmarshal(rest, &value)
		if err != nil {
			t.Fatal(err)
		}
		identifier := value.FullBytes[0]
		switch {
		case value.IsCompound:
			out = append(out, identifier, 0x80)
			out = append(out, testIndefiniteBER(t, value.Bytes)...)
			out = append(out, 0, 0)
		case (identifier == 0x04 || identifier == 0x80) && len(value.Bytes) > 32:
			half := len(value.Bytes) / 2
			out = append(out, identifier|0x20, 0x80)
			for _, segment := range [][]byte{value.Bytes[:half], value.Bytes[half:]} {
				encoded, _ := asn1.Marshal(segment)
				out = append(out, encoded...)
			}
			out = append(out, 0, 0)
		default:
			out = append(out, value.FullBytes...)
		}
	}
	return out
}

func TestPKCS7UnpadRejectsInvalidPadding(t *testing.T) {
	block := bytes.Repeat([]byte{'A'}, 16)
	for _, test := range []struct {
		padding []byte
		ok      bool
		length  int
	}{
		{padding: []byte{1}, ok: true, length: 15},
		{padding: bytes.Repeat([]byte{16}, 16), ok: true, length: 0},
		{padding: []byte{0}, ok: false},
		{padding: []byte{17}, ok: false},
		{padding: []byte{2, 3}, ok: false},
	} {
		data := append(append([]byte(nil), block[:16-len(test.padding)]...), test.padding...)
		got, ok := pkcs7Unpad(data, 16)
		if ok != test.ok || (ok && len(got) != test.length) {
			t.Fatalf("pkcs7Unpad(% x) = %d, %v", test.padding, len(got), ok)
		}
	}
}

func TestParseRecipientKeyAndCertificates(t *testing.T) {
	recipients := syntheticRecipients(t)
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: recipients[0].certificate.Raw})
	bundle := append(append([]byte(nil), certificatePEM...), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: recipients[1].certificate.Raw})...)
	certificates, err := ParseRecipientCertificates(bundle)
	if err != nil || len(certificates) != 2 {
		t.Fatalf("ParseRecipientCertificates bundle = %d, %v", len(certificates), err)
	}
	if certificates, err := ParseRecipientCertificates(recipients[0].certificate.Raw); err != nil || len(certificates) != 1 {
		t.Fatalf("ParseRecipientCertificates DER = %d, %v", len(certificates), err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(recipients[0].key)
	if err != nil {
		t.Fatal(err)
	}
	for name, keyData := range map[string][]byte{
		"PKCS8 PEM": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}),
		"PKCS1 PEM": pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(recipients[0].key)}),
		"PKCS8 DER": pkcs8,
	} {
		if _, err := ParseRecipientKey(certificatePEM, keyData); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	other := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(recipients[1].key)})
	if _, err := ParseRecipientKey(certificatePEM, other); !errors.Is(err, ErrRecipientKey) {
		t.Fatalf("mismatched key error = %v", err)
	}
	if _, err := ParseRecipientKey(bundle, other); !errors.Is(err, ErrRecipientKey) {
		t.Fatalf("certificate bundle error = %v", err)
	}
	if _, err := ParseRecipientCertificates([]byte("not a certificate")); !errors.Is(err, ErrEncryptedAttributesRecipient) {
		t.Fatalf("garbage certificate error = %v", err)
	}
}
