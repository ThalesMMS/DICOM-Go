// Package dsig verifies and creates DICOM Digital Signatures (PS3.15 Annex C,
// PS3.3 C.12.1.1.3): the MAC Parameters Sequence (4FFE,0001) and the Digital
// Signatures Sequence (FFFA,FFFA) of a data set or of a sequence item. The
// signed byte stream is built from the standard; cryptography uses the Go
// standard library plus a local RIPEMD-160. Verify only checks that the data
// matches each signature and its signer certificate; VerifyWithTrust and
// VerifyCertificate also decide whether that certificate is trusted (trust
// anchors, intermediates, CRLs, validity at the current, signature or
// certified time, key usage) with crypto/x509.
//
// Certified timestamps (PS3.3 C.12.1.1.3, Certified Timestamp Type CMS_TSP)
// are RFC 3161 time stamp tokens over the Digital Signature value:
// NewTimestampRequest, TimestampSignature and AddTimestamp obtain and store
// one, VerifyTimestamp checks it. Tokens are read by a minimal DER-only CMS
// SignedData reader (RFC 5652) written for this package; no dependency is
// added.
package dsig

import (
	"crypto/x509"
	"encoding/asn1"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
)

// MAC Algorithm (0400,0015) defined terms.
const (
	MACRIPEMD160 = "RIPEMD160"
	MACMD5       = "MD5"
	MACSHA1      = "SHA1"
	MACSHA256    = "SHA256"
	MACSHA384    = "SHA384"
	MACSHA512    = "SHA512"
)

// CertificateTypeX509 is the Certificate Type (0400,0110) of an X.509
// certificate.
const CertificateTypeX509 = "X509_1993_SIG"

var (
	tagMACParametersSequence          = core.NewTag(0x4FFE, 0x0001)
	tagDigitalSignaturesSequence      = core.NewTag(0xFFFA, 0xFFFA)
	tagMACIDNumber                    = core.NewTag(0x0400, 0x0005)
	tagMACCalculationTransferSyntax   = core.NewTag(0x0400, 0x0010)
	tagMACAlgorithm                   = core.NewTag(0x0400, 0x0015)
	tagDataElementsSigned             = core.NewTag(0x0400, 0x0020)
	tagDigitalSignatureUID            = core.NewTag(0x0400, 0x0100)
	tagDigitalSignatureDateTime       = core.NewTag(0x0400, 0x0105)
	tagCertificateType                = core.NewTag(0x0400, 0x0110)
	tagCertificateOfSigner            = core.NewTag(0x0400, 0x0115)
	tagSignature                      = core.NewTag(0x0400, 0x0120)
	tagCertifiedTimestampType         = core.NewTag(0x0400, 0x0305)
	tagCertifiedTimestamp             = core.NewTag(0x0400, 0x0310)
	tagDigitalSignaturePurposeCodeSeq = core.NewTag(0x0400, 0x0401)
	tagCodeValue                      = core.NewTag(0x0008, 0x0100)
	tagCodingSchemeDesignator         = core.NewTag(0x0008, 0x0102)
	tagCodeMeaning                    = core.NewTag(0x0008, 0x0104)
)

var (
	// ErrSignatureInvalid reports a signature that does not match the signed
	// data or the signer certificate.
	ErrSignatureInvalid = errors.New("dicom dsig: signature invalid")
	// ErrMalformedSignature reports signature attributes that cannot be used
	// (missing MAC Parameters item, unreadable certificate, bad values).
	ErrMalformedSignature = errors.New("dicom dsig: malformed signature attributes")
	// ErrUnsupported reports a MAC algorithm, transfer syntax, certificate
	// type or key type this package does not handle.
	ErrUnsupported = errors.New("dicom dsig: unsupported signature parameters")
)

// Code is a coded entry, such as a Digital Signature Purpose.
type Code struct {
	Value, Scheme, Meaning string
}

// Signature is one Digital Signatures Sequence item with its MAC Parameters.
type Signature struct {
	// Path locates the data set that holds the signature: empty for the top
	// level, otherwise a sequence item path such as "(0040,A730)[0]".
	Path string

	MACIDNumber              uint16
	UID                      string
	DateTime                 string
	CertificateType          string
	CertificateOfSigner      []byte
	Certificate              *x509.Certificate
	Signature                []byte
	CertifiedTimestampType   string
	CertifiedTimestamp       []byte
	Purposes                 []Code
	MACAlgorithm             string
	MACTransferSyntaxUID     string
	DataElementsSigned       []core.Tag
	macParametersItemPresent bool

	// dataSet and item are the elements the signature was read from.
	dataSet []core.Element
	item    []core.Element
}

// location is a data set that may hold signatures.
type location struct {
	path     string
	elements []core.Element
}

// locations lists the top level and every sequence item, depth first.
func locations(elements []core.Element) []location {
	out := []location{{elements: elements}}
	var walk func(prefix string, elements []core.Element)
	walk = func(prefix string, elements []core.Element) {
		for _, element := range elements {
			tag := element.Tag()
			if tag == tagDigitalSignaturesSequence || tag == tagMACParametersSequence {
				continue
			}
			sequence, ok := element.Value.(core.SequenceValue)
			if !ok {
				continue
			}
			for i, item := range sequence.Items {
				path := fmt.Sprintf("%s%s[%d]", prefix, tag, i)
				out = append(out, location{path: path, elements: item.Elements})
				walk(path+".", item.Elements)
			}
		}
	}
	walk("", elements)
	return out
}

func findElement(elements []core.Element, tag core.Tag) (core.Element, bool) {
	for _, element := range elements {
		if element.Tag() == tag {
			return element, true
		}
	}
	return core.Element{}, false
}

func sequenceItems(elements []core.Element, tag core.Tag) []core.DataSet {
	element, ok := findElement(elements, tag)
	if !ok {
		return nil
	}
	sequence, _ := element.Value.(core.SequenceValue)
	return sequence.Items
}

func stringValue(elements []core.Element, tag core.Tag) string {
	element, ok := findElement(elements, tag)
	if !ok {
		return ""
	}
	values := element.StringValues()
	if len(values) == 0 {
		return ""
	}
	return strings.TrimSpace(values[0])
}

func bytesValue(elements []core.Element, tag core.Tag) []byte {
	element, ok := findElement(elements, tag)
	if !ok {
		return nil
	}
	raw, _ := element.Value.(core.RawValue)
	return []byte(raw)
}

func uint16Value(elements []core.Element, tag core.Tag) (uint16, bool) {
	element, ok := findElement(elements, tag)
	if !ok {
		return 0, false
	}
	raw, ok := element.Value.(core.RawValue)
	if !ok || len(raw) != 2 {
		return 0, false
	}
	return binary.LittleEndian.Uint16(raw), true
}

func tagValues(elements []core.Element, tag core.Tag) ([]core.Tag, bool) {
	element, ok := findElement(elements, tag)
	if !ok {
		return nil, false
	}
	switch value := element.Value.(type) {
	case core.TagValue:
		return append([]core.Tag(nil), value...), true
	case core.RawValue:
		if len(value)%4 != 0 {
			return nil, false
		}
		tags := make([]core.Tag, 0, len(value)/4)
		for i := 0; i < len(value); i += 4 {
			tags = append(tags, core.NewTag(binary.LittleEndian.Uint16(value[i:]), binary.LittleEndian.Uint16(value[i+2:])))
		}
		return tags, true
	}
	return nil, false
}

// parseCertificate reads a DER certificate from an OB value, whose even-length
// padding may add a trailing zero byte after the DER encoding.
func parseCertificate(value []byte) *x509.Certificate {
	var outer asn1.RawValue
	if _, err := asn1.Unmarshal(value, &outer); err != nil {
		return nil
	}
	certificate, err := x509.ParseCertificate(outer.FullBytes)
	if err != nil {
		return nil
	}
	return certificate
}

// Signatures lists the digital signatures of a data set, including those in
// sequence items. A signature whose MAC Parameters item is missing is
// returned with macParametersItemPresent false; Verify reports it malformed.
func Signatures(elements []core.Element) []Signature {
	var out []Signature
	for _, loc := range locations(elements) {
		macItems := sequenceItems(loc.elements, tagMACParametersSequence)
		for _, item := range sequenceItems(loc.elements, tagDigitalSignaturesSequence) {
			sig := Signature{Path: loc.path, dataSet: loc.elements, item: item.Elements}
			sig.MACIDNumber, _ = uint16Value(item.Elements, tagMACIDNumber)
			sig.UID = stringValue(item.Elements, tagDigitalSignatureUID)
			sig.DateTime = stringValue(item.Elements, tagDigitalSignatureDateTime)
			sig.CertificateType = stringValue(item.Elements, tagCertificateType)
			sig.CertificateOfSigner = bytesValue(item.Elements, tagCertificateOfSigner)
			sig.Signature = bytesValue(item.Elements, tagSignature)
			sig.CertifiedTimestampType = stringValue(item.Elements, tagCertifiedTimestampType)
			sig.CertifiedTimestamp = bytesValue(item.Elements, tagCertifiedTimestamp)
			for _, code := range sequenceItems(item.Elements, tagDigitalSignaturePurposeCodeSeq) {
				sig.Purposes = append(sig.Purposes, Code{
					Value:   stringValue(code.Elements, tagCodeValue),
					Scheme:  stringValue(code.Elements, tagCodingSchemeDesignator),
					Meaning: stringValue(code.Elements, tagCodeMeaning),
				})
			}
			if len(sig.CertificateOfSigner) > 0 {
				sig.Certificate = parseCertificate(sig.CertificateOfSigner)
			}
			for _, mac := range macItems {
				if id, ok := uint16Value(mac.Elements, tagMACIDNumber); ok && id == sig.MACIDNumber {
					sig.macParametersItemPresent = true
					sig.MACAlgorithm = stringValue(mac.Elements, tagMACAlgorithm)
					sig.MACTransferSyntaxUID = stringValue(mac.Elements, tagMACCalculationTransferSyntax)
					sig.DataElementsSigned, _ = tagValues(mac.Elements, tagDataElementsSigned)
					break
				}
			}
			out = append(out, sig)
		}
	}
	return out
}
