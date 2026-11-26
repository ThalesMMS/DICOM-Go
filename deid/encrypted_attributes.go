package deid

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"reflect"
	"sort"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// Reversible de-identification (PS3.15 E.1.1 and Annex C, PS3.3
// C.12.1.1.4.1): the original values of every attribute the profile removed
// or replaced are encoded as the single item of a Modified Attributes
// Sequence (0400,0550), encrypted as CMS EnvelopedData for one or more
// recipient certificates, and stored as an item of the Encrypted Attributes
// Sequence (0400,0500). Only a holder of a recipient private key can recover
// them (Reidentify).

var (
	tagEncryptedAttributesSequence            = core.NewTag(0x0400, 0x0500)
	tagEncryptedContentTransferSyntaxUID      = core.NewTag(0x0400, 0x0510)
	tagEncryptedContent                       = core.NewTag(0x0400, 0x0520)
	tagModifiedAttributesSequence             = core.NewTag(0x0400, 0x0550)
	tagSpecificCharacterSet                   = core.NewTag(0x0008, 0x0005)
	tagFloatPixelData                         = core.NewTag(0x7FE0, 0x0008)
	tagDoubleFloatPixelData                   = core.NewTag(0x7FE0, 0x0009)
	encryptedAttributesDeidentificationMethod = "Encrypted Attributes Sequence for authorized re-identification"
)

var (
	ErrInvalidEncryptedAttributesOptions = errors.New("deid: invalid encrypted attributes options")
	ErrEncryptedAttributesRecipient      = errors.New("deid: unusable encrypted attributes recipient certificate")
	ErrNoEncryptedAttributes             = errors.New("deid: no Encrypted Attributes Sequence")
	ErrRecipientNotFound                 = errors.New("deid: no Encrypted Attributes item is addressed to this recipient")
	ErrEncryptedAttributesDecrypt        = errors.New("deid: encrypted attributes could not be decrypted with this key")
	ErrMalformedEncryptedAttributes      = errors.New("deid: malformed encrypted attributes")
	ErrUnsupportedEncryptedAttributes    = errors.New("deid: unsupported encrypted attributes encoding or algorithm")
	ErrRecipientKey                      = errors.New("deid: recipient key does not match its certificate")
)

// KeyTransport is the CMS key transport algorithm that encrypts the
// content-encryption key for each recipient.
type KeyTransport string

const (
	// KeyTransportRSAOAEPSHA256 is RSAES-OAEP with SHA-256 and MGF1-SHA-256
	// (RFC 3560, RFC 8017). It is the default.
	KeyTransportRSAOAEPSHA256 KeyTransport = "rsaes-oaep-sha256"
	// KeyTransportRSAPKCS1v15 is RSAES-PKCS1-v1_5 (rsaEncryption), for
	// recipients whose software predates RSAES-OAEP.
	KeyTransportRSAPKCS1v15 KeyTransport = "rsaes-pkcs1-v1_5"

	// Recognized when reading only.
	keyTransportRSAOAEPSHA1   KeyTransport = "rsaes-oaep-sha1"
	keyTransportRSAOAEPSHA384 KeyTransport = "rsaes-oaep-sha384"
	keyTransportRSAOAEPSHA512 KeyTransport = "rsaes-oaep-sha512"
)

// ContentEncryption is the CMS content-encryption algorithm.
type ContentEncryption string

const (
	ContentEncryptionAES128CBC ContentEncryption = "aes128-cbc"
	ContentEncryptionAES192CBC ContentEncryption = "aes192-cbc"
	// ContentEncryptionAES256CBC is the default.
	ContentEncryptionAES256CBC ContentEncryption = "aes256-cbc"

	// Recognized when reading only.
	contentEncryptionDESEDE3CBC ContentEncryption = "des-ede3-cbc"
)

// minimumRecipientKeyBits is the smallest RSA modulus accepted for a
// recipient when encrypting.
const minimumRecipientKeyBits = 2048

// EncryptedAttributesOptions selects reversible de-identification. The
// certificates are trusted as given: the caller decides which recipients are
// authorized and whether a certificate is current.
type EncryptedAttributesOptions struct {
	// Recipients are the X.509 certificates, each with an RSA public key of
	// at least 2048 bits, that may decrypt the original values. A
	// certificate whose Key Usage extension is present must allow key
	// encipherment.
	Recipients []*x509.Certificate
	// KeyTransport defaults to KeyTransportRSAOAEPSHA256.
	KeyTransport KeyTransport
	// ContentEncryption defaults to ContentEncryptionAES256CBC.
	ContentEncryption ContentEncryption
	// SubjectKeyIdentifier names recipients by their Subject Key Identifier
	// instead of by issuer and serial number.
	SubjectKeyIdentifier bool
}

// EncryptedAttributesReport describes the Encrypted Attributes item a
// profile added, without any value, certificate name or key material.
type EncryptedAttributesReport struct {
	Recipients        int               `json:"recipients"`
	Attributes        int               `json:"attributes"`
	KeyTransport      KeyTransport      `json:"key_transport"`
	ContentEncryption ContentEncryption `json:"content_encryption"`
	TransferSyntaxUID string            `json:"transfer_syntax_uid"`
}

// Validate checks the recipients and algorithms as planning does, so an
// application can reject a certificate before it plans any file.
func (options EncryptedAttributesOptions) Validate() error {
	_, err := normalizeEncryptedAttributesOptions(&options)
	return err
}

func normalizeEncryptedAttributesOptions(options *EncryptedAttributesOptions) (*EncryptedAttributesOptions, error) {
	if options == nil {
		return nil, nil
	}
	out := *options
	out.Recipients = append([]*x509.Certificate(nil), options.Recipients...)
	if out.KeyTransport == "" {
		out.KeyTransport = KeyTransportRSAOAEPSHA256
	}
	if out.ContentEncryption == "" {
		out.ContentEncryption = ContentEncryptionAES256CBC
	}
	if out.KeyTransport != KeyTransportRSAOAEPSHA256 && out.KeyTransport != KeyTransportRSAPKCS1v15 {
		return nil, fmt.Errorf("%w: unsupported key transport %q", ErrInvalidEncryptedAttributesOptions, out.KeyTransport)
	}
	if _, _, err := contentEncryptionParameters(out.ContentEncryption); err != nil {
		return nil, err
	}
	if len(out.Recipients) == 0 || len(out.Recipients) > maxCMSRecipients {
		return nil, fmt.Errorf("%w: between 1 and %d recipients are required", ErrInvalidEncryptedAttributesOptions, maxCMSRecipients)
	}
	for _, certificate := range out.Recipients {
		if _, err := recipientPublicKey(certificate, out.SubjectKeyIdentifier); err != nil {
			return nil, err
		}
	}
	return &out, nil
}

func recipientPublicKey(certificate *x509.Certificate, useSKI bool) (*rsa.PublicKey, error) {
	if certificate == nil {
		return nil, fmt.Errorf("%w: nil certificate", ErrEncryptedAttributesRecipient)
	}
	publicKey, ok := certificate.PublicKey.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("%w: only RSA key transport is supported", ErrEncryptedAttributesRecipient)
	}
	if publicKey.N.BitLen() < minimumRecipientKeyBits {
		return nil, fmt.Errorf("%w: RSA key shorter than %d bits", ErrEncryptedAttributesRecipient, minimumRecipientKeyBits)
	}
	if certificate.KeyUsage != 0 && certificate.KeyUsage&x509.KeyUsageKeyEncipherment == 0 {
		return nil, fmt.Errorf("%w: key usage does not allow key encipherment", ErrEncryptedAttributesRecipient)
	}
	if useSKI && len(certificate.SubjectKeyId) == 0 {
		return nil, fmt.Errorf("%w: certificate has no subject key identifier", ErrEncryptedAttributesRecipient)
	}
	if len(certificate.RawIssuer) == 0 || certificate.SerialNumber == nil {
		return nil, fmt.Errorf("%w: certificate has no issuer or serial number", ErrEncryptedAttributesRecipient)
	}
	return publicKey, nil
}

// isDeidentificationMarker reports the attributes the Basic Profile adds to
// every output. Their original values are always stored, so re-identification
// removes those that the source did not have.
func isDeidentificationMarker(tag core.Tag) bool {
	switch tag {
	case tagPatientIdentityRemoved, tagDeidentificationMethod, tagDeidentificationMethodCodeSequence, tagLongitudinalTemporalInformation:
		return true
	}
	return false
}

// isUnencryptedPixelTag reports the pixel data the Encrypted Attributes item
// never carries: a pixel cleaning or redaction is not reversed.
func isUnencryptedPixelTag(tag core.Tag) bool {
	return tag == tagPixelData || tag == tagFloatPixelData || tag == tagDoubleFloatPixelData
}

// applyEncryptedAttributes stores the original values of every top-level
// attribute that planned removes or changes as a new Encrypted Attributes
// item of planned.
func (state *profilePlanner) applyEncryptedAttributes(source *object.Object, planned *object.Object) error {
	options := state.options.EncryptedAttributes
	if options == nil {
		return nil
	}
	after := make(map[core.Tag]core.Element)
	for _, element := range planned.ToDataSet().Elements {
		after[element.Tag()] = element
	}
	var originals []core.Element
	var characterSet *core.Element
	for _, element := range source.ToDataSet().Elements {
		tag := element.Tag()
		if tag == tagSpecificCharacterSet {
			copied := element
			characterSet = &copied
		}
		if isUnencryptedPixelTag(tag) {
			continue
		}
		replacement, kept := after[tag]
		if kept && !isDeidentificationMarker(tag) && reflect.DeepEqual(element, replacement) {
			continue
		}
		original, err := state.materializeOriginal(source, element)
		if err != nil {
			return err
		}
		originals = append(originals, original)
	}
	if characterSet != nil && len(originals) > 0 {
		// The item carries the character set its text values are encoded in.
		original, err := state.materializeOriginal(source, *characterSet)
		if err != nil {
			return err
		}
		if !containsElementTag(originals, tagSpecificCharacterSet) {
			originals = append(originals, original)
		}
	}
	sort.Slice(originals, func(i, j int) bool { return originals[i].Tag().Less(originals[j].Tag()) })
	item, err := encryptOriginals(originals, source.ValueByteOrder(), options)
	if err != nil {
		return err
	}
	appendEncryptedAttributesItem(planned, item)
	state.report.EncryptedAttributes = &EncryptedAttributesReport{
		Recipients:        len(options.Recipients),
		Attributes:        len(originals),
		KeyTransport:      options.KeyTransport,
		ContentEncryption: options.ContentEncryption,
		TransferSyntaxUID: transfer.ExplicitVRLittleEndian.UID,
	}
	return nil
}

func containsElementTag(elements []core.Element, tag core.Tag) bool {
	for _, element := range elements {
		if element.Tag() == tag {
			return true
		}
	}
	return false
}

// materializeOriginal returns a detached copy of element with its value in
// memory, reading a deferred top-level value from source.
func (state *profilePlanner) materializeOriginal(source *object.Object, element core.Element) (core.Element, error) {
	out := profileCloneElement(element)
	if out.Value == nil {
		if element.Header.Length != core.UndefinedLength && int64(element.Header.Length) > state.options.Limits.MaxValueBytes {
			return core.Element{}, ErrProfileResourceLimit
		}
		var buffer bytes.Buffer
		if _, err := source.CopyValueTo(element.Tag(), &buffer); err != nil {
			return core.Element{}, fmt.Errorf("%w: %v", ErrDeferredValueUnavailable, err)
		}
		out.Value = core.RawValue(buffer.Bytes())
	}
	if hasNestedDeferredValue(out) {
		return core.Element{}, ErrDeferredValueUnavailable
	}
	return out, nil
}

func hasNestedDeferredValue(element core.Element) bool {
	sequence, ok := element.Value.(core.SequenceValue)
	if !ok {
		return false
	}
	for _, item := range sequence.Items {
		for _, nested := range item.Elements {
			if nested.Value == nil || hasNestedDeferredValue(nested) {
				return true
			}
		}
	}
	return false
}

// encryptOriginals encodes originals as the Encrypted Attributes Data Set
// (one Modified Attributes Sequence item) in Explicit VR Little Endian and
// returns the Encrypted Attributes Sequence item that carries it.
func encryptOriginals(originals []core.Element, byteOrder binary.ByteOrder, options *EncryptedAttributesOptions) (core.DataSet, error) {
	plaintext, err := encodeModifiedAttributes(originals, byteOrder)
	if err != nil {
		return core.DataSet{}, err
	}
	defer clear(plaintext)
	recipients := make([]cmsRecipient, len(options.Recipients))
	for index, certificate := range options.Recipients {
		publicKey, err := recipientPublicKey(certificate, options.SubjectKeyIdentifier)
		if err != nil {
			return core.DataSet{}, err
		}
		recipients[index] = cmsRecipient{certificate: certificate, publicKey: publicKey}
	}
	content, err := encryptEnvelopedData(plaintext, recipients, options.KeyTransport, options.ContentEncryption, options.SubjectKeyIdentifier, rand.Reader)
	if err != nil {
		return core.DataSet{}, err
	}
	return core.DataSet{Elements: []core.Element{
		core.NewRawElement(tagEncryptedContentTransferSyntaxUID, core.VRUI, padUID(transfer.ExplicitVRLittleEndian.UID)),
		core.NewRawElement(tagEncryptedContent, core.VROB, padOB(content)),
	}}, nil
}

// encodeModifiedAttributes encodes the Encrypted Attributes Data Set: a
// Modified Attributes Sequence with originals as its single item, in
// Explicit VR Little Endian.
func encodeModifiedAttributes(originals []core.Element, byteOrder binary.ByteOrder) ([]byte, error) {
	payload := object.FromElements([]core.Element{
		profileSequenceElement(tagModifiedAttributesSequence, []core.DataSet{{Elements: originals}}),
	}, std.Dictionary)
	if byteOrder != nil {
		payload.SetValueByteOrder(byteOrder)
	}
	var plaintext bytes.Buffer
	if err := object.WriteDataSet(&plaintext, payload, transfer.ExplicitVRLittleEndian); err != nil {
		return nil, fmt.Errorf("deid: encode modified attributes: %w", err)
	}
	return plaintext.Bytes(), nil
}

func padOB(value []byte) []byte {
	if len(value)%2 == 0 {
		return value
	}
	return append(value, 0)
}

// appendEncryptedAttributesItem adds item after any Encrypted Attributes
// items obj already carries (an earlier reversible de-identification).
func appendEncryptedAttributesItem(obj *object.Object, item core.DataSet) {
	var items []core.DataSet
	if existing, ok := obj.Get(tagEncryptedAttributesSequence); ok {
		if sequence, ok := existing.Value.(core.SequenceValue); ok {
			items = make([]core.DataSet, 0, len(sequence.Items)+1)
			for _, existingItem := range sequence.Items {
				items = append(items, profileCloneDataSet(existingItem))
			}
		}
	}
	items = append(items, item)
	obj.Put(profileSequenceElement(tagEncryptedAttributesSequence, items))
}

// RecipientKey is a recipient certificate and its private key. PrivateKey
// may be an *rsa.PrivateKey or any crypto.Decrypter over an RSA key, such as
// a hardware token.
type RecipientKey struct {
	Certificate *x509.Certificate
	PrivateKey  crypto.Decrypter
}

func (key RecipientKey) validate() error {
	if key.Certificate == nil || key.PrivateKey == nil {
		return fmt.Errorf("%w: certificate and private key are required", ErrRecipientKey)
	}
	certificateKey, ok := key.Certificate.PublicKey.(*rsa.PublicKey)
	if !ok {
		return fmt.Errorf("%w: only RSA key transport is supported", ErrRecipientKey)
	}
	privateKeyPublic, ok := key.PrivateKey.Public().(*rsa.PublicKey)
	if !ok || !certificateKey.Equal(privateKeyPublic) {
		return ErrRecipientKey
	}
	return nil
}

// ReidentificationReport describes one re-identification without any value.
type ReidentificationReport struct {
	// Restored counts the attributes copied back from the Modified
	// Attributes Sequence.
	Restored int `json:"restored"`
	// MarkersRemoved counts de-identification markers (Patient Identity
	// Removed, De-identification Method and its Code Sequence, Longitudinal
	// Temporal Information Modified) the source did not have.
	MarkersRemoved int `json:"markers_removed"`
	// RemainingItems counts the Encrypted Attributes items left, such as
	// those of an earlier de-identification or for other recipients.
	RemainingItems    int               `json:"remaining_items"`
	KeyTransport      KeyTransport      `json:"key_transport"`
	ContentEncryption ContentEncryption `json:"content_encryption"`
}

// decryptedItem is one Encrypted Attributes item decrypted for a recipient.
type decryptedItem struct {
	index      int
	originals  core.DataSet
	algorithms envelopedAlgorithms
}

// decryptLatestItem decrypts the last Encrypted Attributes item addressed to
// key: the most recent reversible de-identification.
func decryptLatestItem(obj *object.Object, key RecipientKey) (decryptedItem, []core.DataSet, error) {
	if obj == nil {
		return decryptedItem{}, nil, ErrNilObject
	}
	if err := key.validate(); err != nil {
		return decryptedItem{}, nil, err
	}
	element, ok := obj.Get(tagEncryptedAttributesSequence)
	if !ok {
		return decryptedItem{}, nil, ErrNoEncryptedAttributes
	}
	sequence, ok := element.Value.(core.SequenceValue)
	if !ok || len(sequence.Items) == 0 {
		return decryptedItem{}, nil, ErrNoEncryptedAttributes
	}
	var unsupported error
	for index := len(sequence.Items) - 1; index >= 0; index-- {
		item := sequence.Items[index]
		syntax, content, err := encryptedAttributesItem(item)
		if err != nil {
			if unsupported == nil {
				unsupported = err
			}
			continue
		}
		plaintext, algorithms, err := decryptEnvelopedData(content, key.Certificate, key.PrivateKey)
		if errors.Is(err, ErrRecipientNotFound) {
			continue
		}
		if err != nil {
			return decryptedItem{}, nil, err
		}
		originals, err := decodeModifiedAttributes(plaintext, syntax)
		clear(plaintext)
		if err != nil {
			return decryptedItem{}, nil, err
		}
		return decryptedItem{index: index, originals: originals, algorithms: algorithms}, sequence.Items, nil
	}
	if unsupported != nil {
		return decryptedItem{}, nil, unsupported
	}
	return decryptedItem{}, nil, ErrRecipientNotFound
}

func encryptedAttributesItem(item core.DataSet) (transfer.Syntax, []byte, error) {
	syntaxElement, okSyntax := profileDataSetElement(item, tagEncryptedContentTransferSyntaxUID)
	contentElement, okContent := profileDataSetElement(item, tagEncryptedContent)
	if !okSyntax || !okContent {
		return transfer.Syntax{}, nil, fmt.Errorf("%w: item lacks the transfer syntax or the content", ErrMalformedEncryptedAttributes)
	}
	uid := ""
	if values := syntaxElement.StringValues(); len(values) == 1 {
		uid = transfer.NormalizeUID(values[0])
	}
	syntax, ok := transfer.DefaultRegistry.Get(uid)
	if !ok || !syntax.ExplicitVR || !syntax.IsLittleEndian() || syntax.MediaPayload {
		// PS3.3 C.12.1.1.4.1.1: only explicit VR little endian syntaxes.
		return transfer.Syntax{}, nil, fmt.Errorf("%w: encrypted content transfer syntax", ErrUnsupportedEncryptedAttributes)
	}
	content, ok := contentElement.RawBytes()
	if !ok || len(content) == 0 {
		return transfer.Syntax{}, nil, fmt.Errorf("%w: encrypted content", ErrMalformedEncryptedAttributes)
	}
	// An odd-length DER encoding is padded to even length with one zero.
	return syntax, content, nil
}

// decodeModifiedAttributes parses the Encrypted Attributes Data Set and
// returns the single Modified Attributes Sequence item. The content is
// untrusted: anyone with a recipient certificate can produce it.
func decodeModifiedAttributes(plaintext []byte, syntax transfer.Syntax) (core.DataSet, error) {
	limits := DefaultProfileLimits()
	dataset, err := object.ReadDataSetWithOptions(bytes.NewReader(plaintext), syntax, object.ReadFileOptions{
		MaxTotalBytes:   int64(len(plaintext)) + 1,
		MaxElementBytes: limits.MaxValueBytes,
	})
	if err != nil {
		return core.DataSet{}, fmt.Errorf("%w: %v", ErrEncryptedAttributesDecrypt, err)
	}
	element, ok := dataset.Get(tagModifiedAttributesSequence)
	if !ok {
		return core.DataSet{}, fmt.Errorf("%w: no Modified Attributes Sequence", ErrMalformedEncryptedAttributes)
	}
	sequence, ok := element.Value.(core.SequenceValue)
	if !ok || len(sequence.Items) != 1 {
		return core.DataSet{}, fmt.Errorf("%w: Modified Attributes Sequence must have one item", ErrMalformedEncryptedAttributes)
	}
	item := sequence.Items[0]
	if err := validateProfileDataSetBudget(item, limits); err != nil {
		return core.DataSet{}, err
	}
	for _, original := range item.Elements {
		tag := original.Tag()
		if tag.Group == 0x0002 || tag.Group == 0xFFFE || original.Value == nil {
			return core.DataSet{}, fmt.Errorf("%w: %s cannot be restored", ErrMalformedEncryptedAttributes, tag)
		}
	}
	return item, nil
}

// DecryptEncryptedAttributes returns the original attributes of the most
// recent Encrypted Attributes item addressed to key, as a detached object,
// without changing obj. The result holds PHI.
func DecryptEncryptedAttributes(obj *object.Object, key RecipientKey) (*object.Object, error) {
	decrypted, _, err := decryptLatestItem(obj, key)
	if err != nil {
		return nil, err
	}
	originals := object.FromDataSet(decrypted.originals, std.Dictionary)
	originals.SetValueByteOrder(binary.LittleEndian)
	return originals, nil
}

// Reidentify reverses the most recent reversible de-identification of obj
// addressed to key: it copies the original attributes back over their
// replacements, removes the de-identification markers the source did not
// have, and removes that Encrypted Attributes item (the sequence when it was
// the only one). Pixel Data is never restored. Nothing changes on error.
func Reidentify(ctx context.Context, obj *object.Object, key RecipientKey) (ReidentificationReport, error) {
	if err := ctx.Err(); err != nil {
		return ReidentificationReport{}, err
	}
	decrypted, items, err := decryptLatestItem(obj, key)
	if err != nil {
		return ReidentificationReport{}, err
	}
	if !sameProfileByteOrder(obj.ValueByteOrder(), binary.LittleEndian) {
		return ReidentificationReport{}, fmt.Errorf("%w: object is not little endian", ErrUnsupportedEncryptedAttributes)
	}
	if err := ctx.Err(); err != nil {
		return ReidentificationReport{}, err
	}
	report := ReidentificationReport{
		KeyTransport:      decrypted.algorithms.keyTransport,
		ContentEncryption: decrypted.algorithms.contentEncryption,
	}
	remaining := make([]core.DataSet, 0, len(items)-1)
	for index, item := range items {
		if index != decrypted.index {
			remaining = append(remaining, profileCloneDataSet(item))
		}
	}
	if len(remaining) == 0 {
		obj.Remove(tagEncryptedAttributesSequence)
	} else {
		obj.Put(profileSequenceElement(tagEncryptedAttributesSequence, remaining))
	}
	restoredMarkers := make(map[core.Tag]bool)
	for _, original := range decrypted.originals.Elements {
		obj.Put(profileCloneElement(original))
		restoredMarkers[original.Tag()] = true
		report.Restored++
	}
	for _, marker := range []core.Tag{tagPatientIdentityRemoved, tagDeidentificationMethod, tagDeidentificationMethodCodeSequence, tagLongitudinalTemporalInformation} {
		if !restoredMarkers[marker] && obj.Remove(marker) {
			report.MarkersRemoved++
		}
	}
	if items, ok := obj.GetSequence(tagEncryptedAttributesSequence); ok {
		report.RemainingItems = len(items)
	}
	return report, nil
}

// CloneFileReidentified returns a detached re-identified copy of src with
// File Meta rebuilt from the restored SOP Class and Instance UIDs. src is
// never changed.
func CloneFileReidentified(ctx context.Context, src *object.File, key RecipientKey) (*object.File, ReidentificationReport, error) {
	if err := ctx.Err(); err != nil {
		return nil, ReidentificationReport{}, err
	}
	if src == nil || src.Dataset == nil {
		return nil, ReidentificationReport{}, object.ErrNilFile
	}
	clone, err := object.CloneFile(src)
	if err != nil {
		return nil, ReidentificationReport{}, err
	}
	report, err := Reidentify(ctx, clone.Dataset, key)
	if err != nil {
		return nil, ReidentificationReport{}, err
	}
	clone, err = rederiveFileMeta(clone)
	if err != nil {
		return nil, report, err
	}
	return clone, report, nil
}

// ReidentifyPath writes the re-identification of the Part 10 file at
// sourcePath to destinationPath as CloneFileReidentified and WriteFile would,
// copying the Pixel Data from the source instead of holding it, and replacing
// the destination atomically (see BasicProfilePath).
func ReidentifyPath(ctx context.Context, sourcePath, destinationPath string, key RecipientKey) (ReidentificationReport, error) {
	var report ReidentificationReport
	err := deidentifyPath(ctx, sourcePath, destinationPath, true, func(file *object.File) (err error) {
		report, err = Reidentify(ctx, file.Dataset, key)
		return err
	})
	return report, err
}

// ParseRecipientCertificates parses one or more X.509 certificates from PEM
// CERTIFICATE blocks, or from one DER certificate.
func ParseRecipientCertificates(data []byte) ([]*x509.Certificate, error) {
	var certificates []*x509.Certificate
	rest := data
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrEncryptedAttributesRecipient, err)
		}
		certificates = append(certificates, certificate)
	}
	if len(certificates) > 0 {
		return certificates, nil
	}
	certificate, err := x509.ParseCertificate(data)
	if err != nil {
		return nil, fmt.Errorf("%w: no PEM or DER certificate", ErrEncryptedAttributesRecipient)
	}
	return []*x509.Certificate{certificate}, nil
}

// ParseRecipientKey pairs a certificate (PEM or DER) with its unencrypted
// RSA private key (PEM or DER, PKCS #8 or PKCS #1) and checks that they
// match. Encrypted PEM keys are not supported; decrypt them first or supply
// a crypto.Decrypter in RecipientKey.
func ParseRecipientKey(certificateData, keyData []byte) (RecipientKey, error) {
	certificates, err := ParseRecipientCertificates(certificateData)
	if err != nil {
		return RecipientKey{}, err
	}
	if len(certificates) != 1 {
		return RecipientKey{}, fmt.Errorf("%w: exactly one certificate is required", ErrRecipientKey)
	}
	der := keyData
	if block, _ := pem.Decode(keyData); block != nil {
		if x509.IsEncryptedPEMBlock(block) { //nolint:staticcheck // detection only
			return RecipientKey{}, fmt.Errorf("%w: encrypted PEM keys are not supported", ErrRecipientKey)
		}
		der = block.Bytes
	}
	var privateKey *rsa.PrivateKey
	if parsed, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		rsaKey, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			return RecipientKey{}, fmt.Errorf("%w: only RSA keys are supported", ErrRecipientKey)
		}
		privateKey = rsaKey
	} else if rsaKey, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		privateKey = rsaKey
	} else {
		return RecipientKey{}, fmt.Errorf("%w: unreadable private key", ErrRecipientKey)
	}
	key := RecipientKey{Certificate: certificates[0], PrivateKey: privateKey}
	if err := key.validate(); err != nil {
		return RecipientKey{}, err
	}
	return key, nil
}
