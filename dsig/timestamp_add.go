package dsig

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

// AddTimestamp stores token, an RFC 3161 time stamp token, as the Certified
// Timestamp (CMS_TSP) of the signature sig names (by Path and UID) in obj,
// replacing an earlier one. The token must verify and certify the
// signature's value (VerifyTimestamp without trust anchors; certificates are
// candidate TSA certificates for a token without one). The certified
// timestamp is outside the MAC, so the signature stays valid.
func AddTimestamp(obj *object.Object, sig Signature, token []byte, certificates ...*x509.Certificate) (Signature, error) {
	if obj == nil {
		return Signature{}, errors.New("dicom dsig: nil object")
	}
	pending := sig
	pending.CertifiedTimestampType = CertifiedTimestampCMSTSP
	pending.CertifiedTimestamp = token
	if _, err := VerifyTimestamp(pending, TimestampOptions{Certificates: certificates}); err != nil {
		return Signature{}, err
	}
	path, err := parseLocation(sig.Path)
	if err != nil {
		return Signature{}, err
	}
	if len(token)%2 != 0 {
		token = append(append([]byte(nil), token...), 0)
	}
	err = updateLocation(obj, path, func(elements []core.Element) (edit, error) {
		return updateSignatureItem(elements, sig.UID, func(item []core.Element) []core.Element {
			var out []core.Element
			for _, element := range item {
				if tag := element.Tag(); tag != tagCertifiedTimestampType && tag != tagCertifiedTimestamp {
					out = append(out, element)
				}
			}
			return append(out,
				stringElement(tagCertifiedTimestampType, core.VRCS, CertifiedTimestampCMSTSP),
				core.NewRawElement(tagCertifiedTimestamp, core.VROB, token))
		})
	})
	if err != nil {
		return Signature{}, err
	}
	return findSignature(obj, sig.Path, sig.UID)
}

// updateSignatureItem replaces the Digital Signatures Sequence item with
// Digital Signature UID uid by what update returns for its elements.
func updateSignatureItem(elements []core.Element, uid string, update func([]core.Element) []core.Element) (edit, error) {
	element, ok := findElement(elements, tagDigitalSignaturesSequence)
	sequence, isSequence := element.Value.(core.SequenceValue)
	if ok && isSequence {
		items := append([]core.DataSet(nil), sequence.Items...)
		for j, item := range items {
			if stringValue(item.Elements, tagDigitalSignatureUID) != uid {
				continue
			}
			items[j] = core.DataSet{Elements: sortedElements(update(item.Elements))}
			header := element.Header
			header.Length, header.LengthSet = core.UndefinedLength, true
			return edit{put: []core.Element{{Header: header, Value: core.SequenceValue{Items: items}}}}, nil
		}
	}
	return edit{}, fmt.Errorf("%w: no signature with UID %s here", ErrMalformedSignature, uid)
}

// findSignature returns the signature with uid at path.
func findSignature(obj *object.Object, path, uid string) (Signature, error) {
	for _, sig := range Signatures(obj.Elements()) {
		if sig.Path == path && sig.UID == uid {
			return sig, nil
		}
	}
	return Signature{}, fmt.Errorf("dicom dsig: signature %s not found at %q", uid, path)
}

// TimestampSignature asks tsa for a certified timestamp of sig, checks that
// the response answers the request (TimestampRequest.Check) and adds the
// token to obj (AddTimestamp). It returns the updated signature and the
// token's details; whether the TSA is trusted is for VerifyTimestamp with
// trust anchors.
func TimestampSignature(ctx context.Context, obj *object.Object, sig Signature, tsa Timestamper, opts TimestampRequestOptions) (Signature, TimestampInfo, error) {
	if tsa == nil {
		return Signature{}, TimestampInfo{}, errors.New("dicom dsig: no time stamping authority")
	}
	req, err := NewTimestampRequest(sig, opts)
	if err != nil {
		return Signature{}, TimestampInfo{}, err
	}
	response, err := tsa.Timestamp(ctx, req.DER)
	if err != nil {
		return Signature{}, TimestampInfo{}, err
	}
	token, err := TokenFromResponse(response)
	if err != nil {
		return Signature{}, TimestampInfo{}, err
	}
	info, err := req.Check(token)
	if err != nil {
		return Signature{}, TimestampInfo{}, err
	}
	updated, err := AddTimestamp(obj, sig, token)
	if err != nil {
		return Signature{}, TimestampInfo{}, err
	}
	return updated, info, nil
}
