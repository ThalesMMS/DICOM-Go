package deid

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// The OpenSSL oracle runs only when DICOMGO_OPENSSL_INTEGRATION=1 and an
// openssl binary (OpenSSL 1.1.1 or later, not LibreSSL) is on PATH or named by
// DICOMGO_OPENSSL. Tolerance: exact plaintext equality both ways.

const openSSLIntegrationGate = "DICOMGO_OPENSSL_INTEGRATION"

func requireOpenSSL(t *testing.T) string {
	t.Helper()
	if os.Getenv(openSSLIntegrationGate) != "1" {
		t.Skipf("opt-in: %s=1 with openssl on PATH or in DICOMGO_OPENSSL", openSSLIntegrationGate)
	}
	path := os.Getenv("DICOMGO_OPENSSL")
	if path == "" {
		var err error
		if path, err = exec.LookPath("openssl"); err != nil {
			t.Skipf("openssl not found: %v", err)
		}
	}
	version, err := exec.Command(path, "version").CombinedOutput()
	if err != nil || !strings.HasPrefix(string(version), "OpenSSL") {
		t.Skipf("%s is not OpenSSL: %s", path, version)
	}
	t.Logf("oracle %s: %s (tolerance: exact plaintext)", path, strings.TrimSpace(string(version)))
	return path
}

func runOpenSSL(t *testing.T, tool string, args ...string) {
	t.Helper()
	var stderr bytes.Buffer
	command := exec.Command(tool, args...)
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("openssl %s: %v: %s", strings.Join(args, " "), err, stderr.String())
	}
}

// writeRecipientFiles writes the synthetic recipient certificate and PKCS #8
// key as PEM files in a private temporary directory.
func writeRecipientFiles(t *testing.T, recipient testRecipient) (string, string) {
	t.Helper()
	dir := t.TempDir()
	certificatePath := filepath.Join(dir, "recipient.crt")
	keyPath := filepath.Join(dir, "recipient.key")
	if err := os.WriteFile(certificatePath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: recipient.certificate.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(recipient.key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certificatePath, keyPath
}

func encryptedContentOf(t *testing.T, obj *object.Object) []byte {
	t.Helper()
	items, ok := obj.GetSequence(tagEncryptedAttributesSequence)
	if !ok || len(items) == 0 {
		t.Fatal("no Encrypted Attributes item")
	}
	element, _ := items[len(items)-1].Get(tagEncryptedContent)
	content, _ := element.RawBytes()
	// Drop the OB padding byte of an odd-length DER encoding.
	der, rest, err := berElement(content, 0)
	if err != nil || len(rest) > 1 {
		t.Fatalf("Encrypted Content is not one DER element: %v", err)
	}
	return der
}

func TestOpenSSLDecryptsEncryptedAttributes(t *testing.T) {
	tool := requireOpenSSL(t)
	recipients := syntheticRecipients(t)
	certificatePath, keyPath := writeRecipientFiles(t, recipients[1])
	for name, options := range map[string]EncryptedAttributesOptions{
		"default OAEP SHA-256 AES-256":   {},
		"PKCS1 AES-128":                  {KeyTransport: KeyTransportRSAPKCS1v15, ContentEncryption: ContentEncryptionAES128CBC},
		"subject key identifier AES-192": {SubjectKeyIdentifier: true, ContentEncryption: ContentEncryptionAES192CBC},
	} {
		t.Run(name, func(t *testing.T) {
			options.Recipients = []*x509.Certificate{recipients[0].certificate, recipients[1].certificate}
			source := reversibleSource()
			if _, err := ApplyBasicProfile(context.Background(), source, reversibleOptions(options), nil); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			contentPath := filepath.Join(dir, "content.der")
			plainPath := filepath.Join(dir, "plain.bin")
			if err := os.WriteFile(contentPath, encryptedContentOf(t, source), 0o600); err != nil {
				t.Fatal(err)
			}
			runOpenSSL(t, tool, "cms", "-decrypt", "-binary", "-inform", "DER", "-in", contentPath, "-recip", certificatePath, "-inkey", keyPath, "-out", plainPath)
			plaintext, err := os.ReadFile(plainPath)
			if err != nil {
				t.Fatal(err)
			}
			fromOpenSSL, err := decodeModifiedAttributes(plaintext, transfer.ExplicitVRLittleEndian)
			if err != nil {
				t.Fatalf("OpenSSL plaintext is not an Encrypted Attributes Data Set: %v", err)
			}
			ours, err := DecryptEncryptedAttributes(source, recipients[1].recipientKey())
			if err != nil {
				t.Fatal(err)
			}
			want := encodeExplicitLE(t, ours)
			if got := encodeExplicitLE(t, object.FromDataSet(fromOpenSSL, nil)); !bytes.Equal(got, want) {
				t.Fatal("OpenSSL and dicom-go decrypt different originals")
			}
		})
	}
}

func TestOpenSSLEncryptedContentReidentifies(t *testing.T) {
	tool := requireOpenSSL(t)
	recipients := syntheticRecipients(t)
	certificatePath, _ := writeRecipientFiles(t, recipients[0])
	cases := []struct {
		name string
		args []string
		want EncryptedAttributesReport
	}{
		{name: "OAEP SHA-256 AES-256 DER", args: []string{"-aes256", "-recip", certificatePath, "-keyopt", "rsa_padding_mode:oaep", "-keyopt", "rsa_oaep_md:sha256"}, want: EncryptedAttributesReport{KeyTransport: KeyTransportRSAOAEPSHA256, ContentEncryption: ContentEncryptionAES256CBC}},
		{name: "PKCS1 AES-128 streamed BER", args: []string{"-stream", "-aes128", "-recip", certificatePath}, want: EncryptedAttributesReport{KeyTransport: KeyTransportRSAPKCS1v15, ContentEncryption: ContentEncryptionAES128CBC}},
		{name: "OAEP SHA-1 Triple-DES streamed BER", args: []string{"-stream", "-des3", "-recip", certificatePath, "-keyopt", "rsa_padding_mode:oaep"}, want: EncryptedAttributesReport{KeyTransport: keyTransportRSAOAEPSHA1, ContentEncryption: contentEncryptionDESEDE3CBC}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			source := reversibleSource()
			original := encodeExplicitLE(t, source)
			options := reversibleOptions(EncryptedAttributesOptions{Recipients: []*x509.Certificate{recipients[0].certificate}})
			if _, err := ApplyBasicProfile(context.Background(), source, options, nil); err != nil {
				t.Fatal(err)
			}
			// Re-encrypt the same Encrypted Attributes Data Set with OpenSSL.
			originals, err := DecryptEncryptedAttributes(source, recipients[0].recipientKey())
			if err != nil {
				t.Fatal(err)
			}
			plaintext, err := encodeModifiedAttributes(originals.Elements(), nil)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			plainPath := filepath.Join(dir, "plain.bin")
			contentPath := filepath.Join(dir, "content.der")
			if err := os.WriteFile(plainPath, plaintext, 0o600); err != nil {
				t.Fatal(err)
			}
			args := append([]string{"cms", "-encrypt", "-binary", "-outform", "DER", "-in", plainPath, "-out", contentPath}, test.args...)
			runOpenSSL(t, tool, args...)
			content, err := os.ReadFile(contentPath)
			if err != nil {
				t.Fatal(err)
			}
			source.Put(profileSequenceElement(tagEncryptedAttributesSequence, []core.DataSet{{Elements: []core.Element{
				core.NewRawElement(tagEncryptedContentTransferSyntaxUID, core.VRUI, padUID(transfer.ExplicitVRLittleEndian.UID)),
				core.NewRawElement(tagEncryptedContent, core.VROB, padOB(content)),
			}}}))
			report, err := Reidentify(context.Background(), source, recipients[0].recipientKey())
			if err != nil {
				t.Fatalf("Reidentify OpenSSL content: %v", err)
			}
			if report.KeyTransport != test.want.KeyTransport || report.ContentEncryption != test.want.ContentEncryption {
				t.Fatalf("algorithms = %s/%s", report.KeyTransport, report.ContentEncryption)
			}
			if !bytes.Equal(encodeExplicitLE(t, source), original) {
				t.Fatal("re-identification from OpenSSL content differs from the original")
			}
		})
	}
}
