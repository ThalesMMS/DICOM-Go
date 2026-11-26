package dsig

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// SignOptions configure Sign.
type SignOptions struct {
	// Certificate and Signer are the signer's X.509 certificate and the
	// matching private key (RSA for the PS3.15 RSA profiles; ECDSA also
	// works for Base signatures outside those profiles).
	Certificate *x509.Certificate
	Signer      crypto.Signer
	// MACAlgorithm defaults to SHA256.
	MACAlgorithm string
	// MACTransferSyntaxUID defaults to Explicit VR Little Endian. Signing
	// encapsulated Pixel Data needs the encapsulated transfer syntax the
	// object is stored in (ErrEncapsulatedMACSyntax otherwise).
	MACTransferSyntaxUID string
	// Tags are the Data Elements Signed. Nil signs every element of the data
	// set that PS3.3 allows; listed tags must be present and allowed.
	Tags []core.Tag
	// Purpose, when set, adds a Digital Signature Purpose Code Sequence item.
	Purpose *Code
	// Profile, when not ProfileNone, is checked before the signature is
	// added; a violation leaves obj unchanged.
	Profile Profile
	// ProfileOptions provide an explicit human declaration for Authorization
	// and bounded profile evaluation. Zero options fail closed when context
	// is unknown; they never assume the signer performed human verification.
	ProfileOptions ProfileOptions
	// Time is the Digital Signature DateTime; zero uses the current time.
	// It is written in UTC (offset +0000): DCMTK dcmsign drops the offset
	// when it checks the certificate's validity at the signature time, so a
	// local time behind UTC made valid signatures fail there.
	Time time.Time
	// UID is the Digital Signature UID; empty generates a 2.25 UID.
	UID string
	// Item, when set, signs the data set of a sequence item instead of the
	// top level, like DCMTK dcmsign --sign-item: an attribute path that
	// selects one existing item, such as "ContentSequence[0]",
	// "(0040,A730)[0].(0040,A730)[2]" or a Signature.Path. The Data
	// Elements Signed and the profile checks then apply to that item.
	Item string
	// Rand defaults to crypto/rand.
	Rand io.Reader
}

// Sign adds a digital signature over the top-level data set of obj, or of
// the sequence item SignOptions.Item selects: a MAC Parameters Sequence item
// and a Digital Signatures Sequence item with the next free MAC ID Number of
// that data set. It returns the new signature.
func Sign(obj *object.Object, opts SignOptions) (Signature, error) {
	if obj == nil {
		return Signature{}, errors.New("dicom dsig: nil object")
	}
	if opts.Certificate == nil || opts.Signer == nil {
		return Signature{}, errors.New("dicom dsig: a certificate and a signer are required")
	}
	if !publicKeysEqual(opts.Certificate.PublicKey, opts.Signer.Public()) {
		return Signature{}, errors.New("dicom dsig: the signer key does not match the certificate")
	}
	if opts.MACAlgorithm == "" {
		opts.MACAlgorithm = MACSHA256
	}
	if opts.MACTransferSyntaxUID == "" {
		opts.MACTransferSyntaxUID = transfer.ExplicitVRLittleEndian.UID
	}
	if opts.Rand == nil {
		opts.Rand = rand.Reader
	}
	if opts.Time.IsZero() {
		opts.Time = time.Now()
	}
	if opts.UID == "" {
		uid, err := newUID(opts.Rand)
		if err != nil {
			return Signature{}, err
		}
		opts.UID = uid
	}
	if _, _, err := macHash(opts.MACAlgorithm); err != nil {
		return Signature{}, err
	}
	if _, err := macTransferSyntax(opts.MACTransferSyntaxUID); err != nil {
		return Signature{}, err
	}
	if order := obj.ValueByteOrder(); order != nil && order != binary.LittleEndian {
		return Signature{}, fmt.Errorf("%w: big endian values", ErrUnsupported)
	}

	location, err := parseLocation(opts.Item)
	if err != nil {
		return Signature{}, err
	}
	elements, err := locationElements(obj, location)
	if err != nil {
		return Signature{}, err
	}
	// Validate the metadata graph before signedTags recursively checks UN.
	// Parsed datasets are acyclic; synthetic caller graphs still obey bounds.
	if opts.Profile != ProfileNone {
		evaluation, err := newProfileEvaluation(nil, opts.Profile, opts.ProfileOptions)
		if err != nil {
			return Signature{}, err
		}
		if len(opts.Tags) > evaluation.opts.MaxElements {
			return Signature{}, ErrProfileLimit
		}
		if err := evaluation.boundedWalk(elements, nil); err != nil {
			return Signature{}, err
		}
	}
	tags, err := signedTags(elements, opts.Tags)
	if err != nil {
		return Signature{}, err
	}
	macID := uint16(1)
	for _, item := range sequenceItems(elements, tagMACParametersSequence) {
		if id, ok := uint16Value(item.Elements, tagMACIDNumber); ok && id >= macID {
			macID = id + 1
		}
	}

	macItem := []core.Element{
		usElement(tagMACIDNumber, macID),
		stringElement(tagMACCalculationTransferSyntax, core.VRUI, opts.MACTransferSyntaxUID),
		stringElement(tagMACAlgorithm, core.VRCS, opts.MACAlgorithm),
		atElement(tagDataElementsSigned, tags),
	}
	signatureItem := []core.Element{
		usElement(tagMACIDNumber, macID),
		stringElement(tagDigitalSignatureUID, core.VRUI, opts.UID),
		stringElement(tagDigitalSignatureDateTime, core.VRDT, opts.Time.UTC().Format("20060102150405.000000-0700")),
		stringElement(tagCertificateType, core.VRCS, CertificateTypeX509),
	}
	if opts.Purpose != nil {
		signatureItem = append(signatureItem, core.Element{
			Header: core.ElementHeader{Tag: tagDigitalSignaturePurposeCodeSeq, VR: core.VRSQ, Length: core.UndefinedLength, LengthSet: true},
			Value: core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{
				stringElement(tagCodeValue, core.VRSH, opts.Purpose.Value),
				stringElement(tagCodingSchemeDesignator, core.VRSH, opts.Purpose.Scheme),
				stringElement(tagCodeMeaning, core.VRLO, opts.Purpose.Meaning),
			}}}},
		})
	}
	pending := Signature{
		Path: location.String(), MACIDNumber: macID, UID: opts.UID, CertificateType: CertificateTypeX509,
		Certificate: opts.Certificate, CertificateOfSigner: opts.Certificate.Raw,
		MACAlgorithm: opts.MACAlgorithm, MACTransferSyntaxUID: opts.MACTransferSyntaxUID,
		DataElementsSigned: tags, macParametersItemPresent: true,
		dataSet: elements, item: signatureItem,
	}
	if opts.Purpose != nil {
		pending.Purposes = []Code{*opts.Purpose}
	}
	if opts.Profile != ProfileNone {
		if _, err := CheckProfileWithOptions(pending, opts.Profile, opts.ProfileOptions); err != nil {
			return Signature{}, err
		}
	}
	mac, oid, err := computeMAC(pending)
	if err != nil {
		return Signature{}, err
	}
	var signature []byte
	switch opts.Certificate.PublicKey.(type) {
	case *rsa.PublicKey:
		info, err := digestInfoDER(oid, mac, true)
		if err != nil {
			return Signature{}, err
		}
		signature, err = opts.Signer.Sign(opts.Rand, info, crypto.Hash(0))
		if err != nil {
			return Signature{}, fmt.Errorf("dicom dsig: sign: %w", err)
		}
	case *ecdsa.PublicKey:
		hash, hashErr := macCryptoHash(opts.MACAlgorithm)
		if hashErr != nil {
			return Signature{}, hashErr
		}
		signature, err = opts.Signer.Sign(opts.Rand, mac, hash)
		if err != nil {
			return Signature{}, fmt.Errorf("dicom dsig: sign: %w", err)
		}
	default:
		return Signature{}, fmt.Errorf("%w: %T signer key", ErrUnsupported, opts.Certificate.PublicKey)
	}
	signatureItem = append(signatureItem,
		core.NewRawElement(tagCertificateOfSigner, core.VROB, opts.Certificate.Raw),
		core.NewRawElement(tagSignature, core.VROB, signature),
	)
	sort.SliceStable(signatureItem, func(i, j int) bool { return tagLess(signatureItem[i].Tag(), signatureItem[j].Tag()) })

	err = updateLocation(obj, location, func(elements []core.Element) (edit, error) {
		return edit{put: []core.Element{
			appendItem(elements, tagMACParametersSequence, macItem),
			appendItem(elements, tagDigitalSignaturesSequence, signatureItem),
		}}, nil
	})
	if err != nil {
		return Signature{}, err
	}
	for _, sig := range Signatures(obj.Elements()) {
		if sig.UID == opts.UID {
			return sig, nil
		}
	}
	return Signature{}, errors.New("dicom dsig: signature not found after signing")
}

// RemoveSignature deletes the signature sig names (by Path and UID) from
// obj: its Digital Signatures Sequence item and, unless another signature
// of that data set uses it, its MAC Parameters Sequence item. A sequence
// left empty is removed.
func RemoveSignature(obj *object.Object, sig Signature) error {
	if obj == nil {
		return errors.New("dicom dsig: nil object")
	}
	if _, err := findSignature(obj, sig.Path, sig.UID); err != nil {
		return err
	}
	path, err := parseLocation(sig.Path)
	if err != nil {
		return err
	}
	return updateLocation(obj, path, func(elements []core.Element) (edit, error) {
		var removedMACID *uint16
		remainingIDs := map[uint16]bool{}
		var signatures []core.DataSet
		for _, item := range sequenceItems(elements, tagDigitalSignaturesSequence) {
			id, _ := uint16Value(item.Elements, tagMACIDNumber)
			if removedMACID == nil && stringValue(item.Elements, tagDigitalSignatureUID) == sig.UID {
				removedMACID = &id
				continue
			}
			remainingIDs[id] = true
			signatures = append(signatures, item)
		}
		var macItems []core.DataSet
		for _, item := range sequenceItems(elements, tagMACParametersSequence) {
			if id, ok := uint16Value(item.Elements, tagMACIDNumber); ok && removedMACID != nil && id == *removedMACID && !remainingIDs[id] {
				continue
			}
			macItems = append(macItems, item)
		}
		var e edit
		for _, sequence := range []struct {
			tag   core.Tag
			items []core.DataSet
		}{{tagMACParametersSequence, macItems}, {tagDigitalSignaturesSequence, signatures}} {
			tag, items := sequence.tag, sequence.items
			if len(items) == 0 {
				e.remove = append(e.remove, tag)
				continue
			}
			e.put = append(e.put, core.Element{
				Header: core.ElementHeader{Tag: tag, VR: core.VRSQ, Length: core.UndefinedLength, LengthSet: true},
				Value:  core.SequenceValue{Items: items},
			})
		}
		return e, nil
	})
}

// RemoveAllSignatures deletes every digital signature of obj, including
// those in sequence items, and returns how many it removed.
func RemoveAllSignatures(obj *object.Object) (int, error) {
	if obj == nil {
		return 0, errors.New("dicom dsig: nil object")
	}
	signatures := Signatures(obj.Elements())
	for _, sig := range signatures {
		if err := RemoveSignature(obj, sig); err != nil {
			return 0, err
		}
	}
	return len(signatures), nil
}

// signedTags resolves the Data Elements Signed, in ascending tag order.
func signedTags(elements []core.Element, requested []core.Tag) ([]core.Tag, error) {
	var wanted map[core.Tag]bool
	if requested != nil {
		wanted = make(map[core.Tag]bool, len(requested))
		for _, tag := range requested {
			wanted[tag] = true
		}
	}
	var tags []core.Tag
	for _, element := range sortedElements(elements) {
		tag := element.Tag()
		if wanted != nil && !wanted[tag] {
			continue
		}
		if excluded(element) {
			if wanted != nil {
				return nil, fmt.Errorf("dicom dsig: %s may not be signed", tag)
			}
			continue
		}
		tags = append(tags, tag)
		delete(wanted, tag)
	}
	for tag := range wanted {
		return nil, fmt.Errorf("dicom dsig: %s is not in the data set", tag)
	}
	if len(tags) == 0 {
		return nil, errors.New("dicom dsig: no data element to sign")
	}
	return tags, nil
}

func appendItem(elements []core.Element, tag core.Tag, item []core.Element) core.Element {
	items := append([]core.DataSet(nil), sequenceItems(elements, tag)...)
	items = append(items, core.DataSet{Elements: item})
	return core.Element{
		Header: core.ElementHeader{Tag: tag, VR: core.VRSQ, Length: core.UndefinedLength, LengthSet: true},
		Value:  core.SequenceValue{Items: items},
	}
}

func stringElement(tag core.Tag, vr core.VR, value string) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: vr}, Value: core.StringValue{value}}
}

func usElement(tag core.Tag, value uint16) core.Element {
	return object.USElement(tag, binary.LittleEndian, value)
}

func atElement(tag core.Tag, tags []core.Tag) core.Element {
	return object.ATElement(tag, binary.LittleEndian, tags...)
}

func tagLess(a, b core.Tag) bool {
	if a.Group != b.Group {
		return a.Group < b.Group
	}
	return a.Element < b.Element
}

func publicKeysEqual(a, b crypto.PublicKey) bool {
	type equaler interface{ Equal(crypto.PublicKey) bool }
	key, ok := a.(equaler)
	return ok && key.Equal(b)
}

func newUID(r io.Reader) (string, error) {
	uid, err := core.NewUIDFromReader(r)
	if err != nil {
		return "", fmt.Errorf("dicom dsig: %w", err)
	}
	return uid, nil
}
