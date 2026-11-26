package dsig

import (
	"crypto/rsa"
	"fmt"
	"strings"

	"github.com/ThalesMMS/dicom-go/attrpath"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/uid"
)

// profileString reads a small single metadata value without expanding a raw
// value or exposing it in an error. Payloads and VM/VR IOD validation stay out.
func profileString(elements []core.Element, tag core.Tag, max int) (string, bool) {
	el, ok := findElement(elements, tag)
	if !ok {
		return "", false
	}
	var s string
	switch v := el.Value.(type) {
	case core.StringValue:
		if len(v) != 1 || len(v[0]) > max {
			return "", false
		}
		s = v[0]
	case core.RawValue:
		if len(v) > max {
			return "", false
		}
		s = string(v)
	default:
		return "", false
	}
	return strings.TrimRight(s, " \x00"), true
}

func (e *profileEvaluation) requireHumanPaths(elements []core.Element, paths []string, require func(core.Tag, string)) error {
	for n, text := range paths {
		if err := e.ctx.Err(); err != nil {
			return err
		}
		if len(text) > attrpath.MaxPathLength {
			return fmt.Errorf("%w: human path length exceeds 4096", ErrProfileLimit)
		}
		path, err := attrpath.Parse(text, nil)
		if err != nil || len(path) == 0 || path.Wildcard() || path.SelectsItem() {
			e.issue("human-path-invalid", core.Tag{}, fmt.Sprintf("declaration[%d]", n))
			continue
		}
		current := elements
		var root core.Tag
		var resolved []string
		found := true
		for i, seg := range path {
			tag, err := segmentTag(current, seg)
			if err != nil {
				found = false
				break
			}
			if i == 0 {
				root = tag
			}
			canonical := tag.String()
			if seg.HasItem {
				canonical += fmt.Sprintf("[%d]", seg.Item)
			}
			resolved = append(resolved, canonical)
			el, ok := findElement(current, tag)
			if !ok {
				found = false
				break
			}
			if seg.HasItem {
				seq, ok := el.Value.(core.SequenceValue)
				if !ok || seg.Item < 0 || seg.Item >= len(seq.Items) {
					found = false
					break
				}
				current = seq.Items[seg.Item].Elements
			}
		}
		canonical := strings.Join(resolved, ".")
		if !found {
			e.issue("human-path-not-found", root, canonical)
			continue
		}
		require(root, canonical)
	}
	return nil
}

func hasVerificationPurpose(purposes []Code) bool {
	for _, purpose := range purposes {
		if purpose.Value == "5" && purpose.Scheme == "ASTM-sigpurpose" {
			return true
		}
	}
	return false
}

func (e *profileEvaluation) checkSR(sig Signature, present map[core.Tag]core.Element, require func(core.Tag, string)) error {
	sop, ok := profileString(sig.dataSet, tagSOPClassUID, 64)
	class, known := uid.LookupStorageSOPClass(sop)
	if !ok || !known || (class.Category != uid.StorageCategoryStructuredReport && sop != "1.2.840.10008.5.1.4.1.1.88.59") {
		e.issue("sr-or-kos-required", tagSOPClassUID, "")
	}
	if len(sig.Purposes) == 0 {
		e.issue("sr-purpose-required", tagDigitalSignaturePurposeCodeSeq, "")
	}
	if observer, exists := present[tagVerifyingObserverSeq]; exists {
		if _, ok := observer.Value.(core.SequenceValue); !ok {
			e.issue("sr-observer-sequence", tagVerifyingObserverSeq, "")
		}
	}
	observers := sequenceItems(sig.dataSet, tagVerifyingObserverSeq)
	if len(observers) > 1 {
		e.issue("sr-observer-count", tagVerifyingObserverSeq, "")
	}
	flag, _ := profileString(sig.dataSet, tagVerificationFlag, 16)
	if flag == "VERIFIED" {
		if len(observers) != 1 {
			e.issue("sr-verified-observer-required", tagVerifyingObserverSeq, "")
		} else {
			el, exists := findElement(observers[0].Elements, tagVerificationDateTime)
			if !exists || excluded(el) {
				e.issue("sr-verification-datetime-required", tagVerificationDateTime, "(0040,A073)[0].(0040,A030)")
			}
		}
		// Requirement applies to at least one candidate, not every additional
		// Creator/author signature in the document. No fabricated root A030 AT.
		extra := []core.Tag{tagSOPInstanceUID, tagVerificationFlag, tagVerifyingObserverSeq}
		candidateOK := func(candidate Signature) bool {
			if candidate.Path != "" || !hasVerificationPurpose(candidate.Purposes) || !baseCandidate(candidate) {
				return false
			}
			signed := make(map[core.Tag]bool, len(candidate.DataElementsSigned))
			for _, tag := range candidate.DataElementsSigned {
				signed[tag] = true
			}
			for tag := range e.required {
				el, exists := present[tag]
				if !exists || excluded(el) || !signed[tag] {
					return false
				}
			}
			for _, tag := range extra {
				el, exists := present[tag]
				if !exists || excluded(el) || !signed[tag] {
					return false
				}
			}
			return len(observers) == 1 && hasSignableElement(observers[0].Elements, tagVerificationDateTime)
		}
		if hasVerificationPurpose(sig.Purposes) {
			for _, tag := range extra {
				require(tag, "")
			}
		}
		e.report.SRVerificationCandidate = candidateOK(sig)
		if !e.report.SRVerificationCandidate {
			candidates, err := e.srCandidates(sig.dataSet)
			if err != nil {
				return err
			}
			for _, candidate := range candidates {
				if candidateOK(candidate) {
					e.report.SRVerificationCandidate = true
					break
				}
			}
		}
		if !e.report.SRVerificationCandidate {
			e.issue("sr-verification-candidate-required", tagDigitalSignaturePurposeCodeSeq, "")
		}
	}
	refMAC := core.NewTag(0x0400, 0x0403)
	return e.boundedWalk(sig.dataSet, func(el core.Element, path string) {
		if el.Tag() != refMAC {
			return
		}
		seq, ok := el.Value.(core.SequenceValue)
		if !ok || len(seq.Items) == 0 {
			e.issue("sr-referenced-mac-sequence", refMAC, path)
			return
		}
		for i, item := range seq.Items {
			algorithm, ok := profileString(item.Elements, tagMACAlgorithm, 16)
			if !ok || !allowedProfileMAC(algorithm) {
				e.issue("sr-referenced-mac-algorithm", tagMACAlgorithm, fmt.Sprintf("%s[%d].(0400,0015)", path, i))
			}
		}
	})
}

func hasSignableElement(elements []core.Element, tag core.Tag) bool {
	el, ok := findElement(elements, tag)
	return ok && !excluded(el)
}

func baseCandidate(sig Signature) bool {
	if !allowedProfileMAC(sig.MACAlgorithm) || sig.CertificateType != CertificateTypeX509 || sig.Certificate == nil {
		return false
	}
	_, ok := sig.Certificate.PublicKey.(*rsa.PublicKey)
	return ok
}

// srCandidates parses only main-dataset, purpose-5 candidates. Budgets precede
// certificate/AT decoding. Their cryptographic integrity is deliberately NOT
// inferred from purpose, a nonempty signature blob or an RSA certificate.
func (e *profileEvaluation) srCandidates(elements []core.Element) ([]Signature, error) {
	items := sequenceItems(elements, tagDigitalSignaturesSequence)
	if len(items) > 256 {
		return nil, ErrProfileLimit
	}
	macs := sequenceItems(elements, tagMACParametersSequence)
	if len(macs) > 256 {
		return nil, ErrProfileLimit
	}
	var out []Signature
	certificateBytes := 0
	for _, item := range items {
		if err := e.ctx.Err(); err != nil {
			return nil, err
		}
		var purposes []Code
		for _, code := range sequenceItems(item.Elements, tagDigitalSignaturePurposeCodeSeq) {
			value, ok := profileString(code.Elements, tagCodeValue, 16)
			if !ok {
				continue
			}
			scheme, ok := profileString(code.Elements, tagCodingSchemeDesignator, 16)
			if !ok {
				continue
			}
			if value == "5" && scheme == "ASTM-sigpurpose" {
				purposes = append(purposes, Code{Value: value, Scheme: scheme})
				break
			}
		}
		if len(purposes) == 0 {
			continue
		}
		cert := bytesValue(item.Elements, tagCertificateOfSigner)
		if len(cert) > 65536 {
			return nil, ErrProfileLimit
		}
		certificateBytes += len(cert)
		if certificateBytes > 1<<20 {
			return nil, ErrProfileLimit
		}
		s := Signature{dataSet: elements, item: item.Elements, Purposes: purposes}
		s.CertificateType, _ = profileString(item.Elements, tagCertificateType, 16)
		s.Certificate = parseCertificate(cert)
		s.CertifiedTimestampType, _ = profileString(item.Elements, tagCertifiedTimestampType, 16)
		s.CertifiedTimestamp = bytesValue(item.Elements, tagCertifiedTimestamp)
		if len(bytesValue(item.Elements, tagSignature)) == 0 {
			continue
		}
		id, ok := uint16Value(item.Elements, tagMACIDNumber)
		if !ok {
			continue
		}
		for _, mac := range macs {
			if match, valid := uint16Value(mac.Elements, tagMACIDNumber); !valid || match != id {
				continue
			}
			s.MACAlgorithm, _ = profileString(mac.Elements, tagMACAlgorithm, 16)
			at, exists := findElement(mac.Elements, tagDataElementsSigned)
			if !exists {
				break
			}
			switch v := at.Value.(type) {
			case core.RawValue:
				if len(v)/4 > e.opts.MaxElements {
					return nil, ErrProfileLimit
				}
			case core.TagValue:
				if len(v) > e.opts.MaxElements {
					return nil, ErrProfileLimit
				}
			}
			s.DataElementsSigned, _ = tagValues(mac.Elements, tagDataElementsSigned)
			break
		}
		out = append(out, s)
	}
	return out, nil
}
