package dsig

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/testutil"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func omitProfileTag(elements []core.Element, omit core.Tag) []core.Tag {
	var tags []core.Tag
	for _, el := range elements {
		if el.Tag() != omit && !excluded(el) {
			tags = append(tags, el.Tag())
		}
	}
	return tags
}
func hasProfileIssue(report ProfileReport, code string, tag core.Tag) bool {
	for _, issue := range report.Issues {
		if issue.Code == code && issue.Tag == tag {
			return true
		}
	}
	return false
}

// Each negative is cryptographically valid: one present normative attribute
// is excluded before signing with ProfileNone. These tiny datasets test
// profile coverage, not IOD conformance or clinical document content.
func TestProfileCryptoValidMissingNormativeAttributes(t *testing.T) {
	pki := testutil.NewTestPKI(t)
	s := newSigner(t, pki, "profile-fixtures.test", "rsa2048")
	fixtures := []struct {
		name, sop string
		tag       uint32
	}{
		{"Enhanced MR nucleus", "1.2.840.10008.5.1.4.1.1.4.1", 0x00189100},
		{"Enhanced CT acquisition", "1.2.840.10008.5.1.4.1.1.2.1", 0x00189361},
		{"SEG segment sequence", "1.2.840.10008.5.1.4.1.1.66.4", 0x00620002},
		{"Labelmap SEG pixeldata", "1.2.840.10008.5.1.4.1.1.66.7", 0x7FE00010},
		{"MRS spectrum", "1.2.840.10008.5.1.4.1.1.4.2", 0x00189052},
		{"Raw creator version", "1.2.840.10008.5.1.4.1.1.66", 0x00089123},
		{"Functional Groups shared", "1.2.840.10008.5.1.4.1.1.4.1", 0x52009229},
		{"Functional Groups perframe", "1.2.840.10008.5.1.4.1.1.2.1", 0x52009230},
		{"Equipment UDI", "1.2.840.10008.5.1.4.1.1.7", 0x0018100A},
	}
	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			elements := profileElements()
			elements[0] = uiElement(tagSOPClassUID, f.sop)
			el := profileElement(f.tag)
			elements = append(elements, el)
			for _, missing := range []bool{false, true} {
				obj := newObject(elements, transfer.ExplicitVRLittleEndian)
				opts := s.options(MACSHA256)
				if missing {
					opts.Tags = omitProfileTag(elements, el.Tag())
				}
				sig, err := Sign(obj, opts)
				if err != nil {
					t.Fatal(err)
				}
				requireValid(t, roundTrip(t, obj, transfer.ExplicitVRLittleEndian), 1)
				for _, profile := range []Profile{ProfileCreatorRSA, ProfileAuthorizationRSA} {
					report, err := CheckProfileWithOptions(sig, profile, ProfileOptions{HumanVerifiedPaths: []string{}})
					wantMissing := missing && !(f.name == "Equipment UDI" && profile == ProfileAuthorizationRSA)
					if errors.Is(err, ErrProfileViolation) != wantMissing {
						t.Fatalf("%s missing=%v %v %+v", profile, missing, err, report)
					}
					if wantMissing && !hasProfileIssue(report, "required-not-signed", el.Tag()) {
						t.Fatalf("missing diagnostic: %+v", report)
					}
				}
			}
		})
	}
}

func TestProfileRawPrivateSupersetAndUN(t *testing.T) {
	creator := core.NewTag(0x0011, 0x0010)
	data := core.NewTag(0x0011, 0x1001)
	private := []core.Element{stringElement(creator, core.VRLO, "FIXTURE"), core.NewRawElement(data, core.VROB, []byte{1, 2})}
	elements := append(profileElements(), private...)
	if _, err := CheckProfileWithOptions(structuralSignature(elements, data), ProfileCreatorRSA, ProfileOptions{}); err != nil {
		t.Fatalf("non-Raw private not generic requirement: %v", err)
	}
	elements = append(elements, uiElement(core.NewTag(0x0008, 0x9123), "2.25.145205"))
	for _, omit := range []core.Tag{creator, data} {
		report, err := CheckProfileWithOptions(structuralSignature(elements, omit), ProfileCreatorRSA, ProfileOptions{})
		if !errors.Is(err, ErrProfileViolation) || !hasProfileIssue(report, "required-not-signed", omit) {
			t.Fatalf("Raw %s: %v %+v", omit, err, report)
		}
	}
	elements = append(elements, core.NewRawElement(core.NewTag(0x0011, 0x1002), core.VRUN, []byte{1, 2}))
	report, err := CheckProfileWithOptions(structuralSignature(elements, core.Tag{}), ProfileCreatorRSA, ProfileOptions{})
	if !errors.Is(err, ErrProfileViolation) || !hasProfileIssue(report, "required-unsignable", core.NewTag(0x0011, 0x1002)) {
		t.Fatalf("Raw UN silently omitted: %v %+v", err, report)
	}
	// Private group lengths are barred from the MAC and are not private raw data.
	clean := append(profileElements(), uiElement(core.NewTag(0x0008, 0x9123), "2.25.145205"), core.NewRawElement(core.NewTag(0x0011, 0), core.VRUL, make([]byte, 4)))
	if _, err := CheckProfileWithOptions(structuralSignature(clean, core.Tag{}), ProfileCreatorRSA, ProfileOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestProfileRequiredSequenceWithUNIsImpossible(t *testing.T) {
	tag := core.NewTag(0x5200, 0x9229)
	elements := append(profileElements(), sequenceElement(tag, core.DataSet{Elements: []core.Element{core.NewRawElement(core.NewTag(0x0011, 0x1001), core.VRUN, []byte{1, 2})}}))
	report, err := CheckProfileWithOptions(structuralSignature(elements, core.Tag{}), ProfileCreatorRSA, ProfileOptions{})
	if !errors.Is(err, ErrProfileViolation) || !hasProfileIssue(report, "required-unsignable", tag) {
		t.Fatalf("required sequence UN silently omitted: %v %+v", err, report)
	}
}

func TestProfileHumanDeclarationParentCoverage(t *testing.T) {
	parent := core.NewTag(0x0008, 0x1110)
	child := core.NewTag(0x0008, 0x1155)
	elements := append(profileElements(), sequenceElement(parent, core.DataSet{Elements: []core.Element{uiElement(child, "2.25.145206")}}), stringElement(core.NewTag(0x0011, 0x0010), core.VRLO, "PRIVATE SECRET"), core.NewRawElement(core.NewTag(0x0011, 0x1001), core.VROB, []byte{1, 2}))
	sig := structuralSignature(elements, core.Tag{})
	report, err := CheckProfileWithOptions(sig, ProfileAuthorizationRSA, ProfileOptions{})
	if !errors.Is(err, ErrProfileIncomplete) || report.Coverage != ProfileCoverageNeedsContext {
		t.Fatalf("unknown declaration: %v %+v", err, report)
	}
	opts := ProfileOptions{HumanVerifiedPaths: []string{"ReferencedStudySequence[0].ReferencedSOPInstanceUID", `(0011,"PRIVATE SECRET",01)`}}
	report, err = CheckProfileWithOptions(sig, ProfileAuthorizationRSA, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, tag := range report.RequiredTags {
		if tag == child {
			t.Fatal("nested child added to root DataElementsSigned")
		}
	}
	sig = structuralSignature(elements, parent)
	report, err = CheckProfileWithOptions(sig, ProfileAuthorizationRSA, opts)
	if !errors.Is(err, ErrProfileViolation) || !hasProfileIssue(report, "required-not-signed", parent) {
		t.Fatalf("unsigned parent: %v %+v", err, report)
	}
	if !strings.Contains(report.Issues[0].Path, "(0008,1110)") {
		t.Fatalf("noncanonical path: %+v", report)
	}
	for _, path := range []string{"ReferencedStudySequence[*].ReferencedSOPInstanceUID", "ReferencedStudySequence[0]", "(0011,\"DO NOT LEAK\",01)", "PatientName"} {
		report, err := CheckProfileWithOptions(sig, ProfileAuthorizationRSA, ProfileOptions{HumanVerifiedPaths: []string{path}})
		if !errors.Is(err, ErrProfileViolation) {
			t.Fatalf("path %q unexpectedly accepted", path)
		}
		for _, issue := range report.Issues {
			if strings.Contains(issue.Path, "SECRET") || strings.Contains(issue.Path, "LEAK") {
				t.Fatalf("creator leaked %+v", issue)
			}
		}
	}
}

func TestProfileHumanPathLengthIsBoundedBeforeParse(t *testing.T) {
	sig := structuralSignature(profileElements(), core.Tag{})
	report, err := CheckProfileWithOptions(sig, ProfileAuthorizationRSA, ProfileOptions{HumanVerifiedPaths: []string{strings.Repeat("SECRET", 1000)}})
	if !errors.Is(err, ErrProfileLimit) || report.Coverage != ProfileCoveragePartial {
		t.Fatalf("path bound %v %+v", err, report)
	}
	if strings.Contains(err.Error(), "SECRET") {
		t.Fatal("original path leaked")
	}
}

func srProfileElements(verified bool) []core.Element {
	elements := profileElements()
	elements[0] = uiElement(tagSOPClassUID, "1.2.840.10008.5.1.4.1.1.88.33")
	elements = append(elements, stringElement(core.NewTag(0x0040, 0xA040), core.VRCS, "CONTAINER"), stringElement(tagVerificationFlag, core.VRCS, "UNVERIFIED"))
	if verified {
		elements[len(elements)-1] = stringElement(tagVerificationFlag, core.VRCS, "VERIFIED")
		elements = append(elements, sequenceElement(tagVerifyingObserverSeq, core.DataSet{Elements: []core.Element{stringElement(tagVerificationDateTime, core.VRDT, "20261008120000+0000")}}))
	}
	return elements
}

func TestProfileSRDocumentConditionsAndCryptoSeparation(t *testing.T) {
	pki := testutil.NewTestPKI(t)
	s := newSigner(t, pki, "sr-profile.test", "rsa2048")
	for _, verified := range []bool{false, true} {
		obj := newObject(srProfileElements(verified), transfer.ExplicitVRLittleEndian)
		opts := s.options(MACSHA256)
		opts.Profile = ProfileSRRSA
		purpose := "1"
		if verified {
			purpose = "5"
		}
		opts.Purpose = &Code{Value: purpose, Scheme: "ASTM-sigpurpose", Meaning: "Synthetic"}
		sig, err := Sign(obj, opts)
		if err != nil {
			t.Fatalf("verified=%v %v", verified, err)
		}
		report, err := CheckProfileWithOptions(sig, ProfileSRRSA, ProfileOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if report.SRVerificationCandidate != verified {
			t.Fatalf("candidate flag %+v", report)
		}
		for _, tag := range sig.DataElementsSigned {
			if tag == tagVerificationDateTime {
				t.Fatal("fabricated root VerificationDateTime")
			}
		}
		requireValid(t, roundTrip(t, obj, transfer.ExplicitVRLittleEndian), 1)
	}
	obj := newObject(srProfileElements(true), transfer.ExplicitVRLittleEndian)
	verification := s.options(MACSHA256)
	verification.Profile = ProfileSRRSA
	verification.Purpose = &Code{Value: "5", Scheme: "ASTM-sigpurpose"}
	if _, err := Sign(obj, verification); err != nil {
		t.Fatal(err)
	}
	author := s.options(MACSHA256)
	author.Profile = ProfileSRRSA
	author.Purpose = &Code{Value: "1", Scheme: "ASTM-sigpurpose"}
	author.Tags = omitProfileTag(obj.Elements(), tagVerifyingObserverSeq)
	sig, err := Sign(obj, author)
	if err != nil {
		t.Fatalf("another purpose may use sibling verification: %v", err)
	}
	report, err := CheckProfileWithOptions(sig, ProfileSRRSA, ProfileOptions{})
	if err != nil || !report.SRVerificationCandidate {
		t.Fatalf("sibling: %v %+v", err, report)
	}
	requireValid(t, obj, 2)
	// Corrupt only verification signature bytes: structural candidate coverage
	// remains explicit and never asserts signature integrity/trust.
	items := sequenceItems(obj.Elements(), tagDigitalSignaturesSequence)
	detached := append([]core.DataSet(nil), items...)
	detached[0].Elements = append([]core.Element(nil), items[0].Elements...)
	for i, el := range detached[0].Elements {
		if el.Tag() == tagSignature {
			raw := append([]byte(nil), bytesValue(items[0].Elements, tagSignature)...)
			raw[0] ^= 1
			detached[0].Elements[i] = core.NewRawElement(tagSignature, core.VROB, raw)
		}
	}
	obj.Put(sequenceElement(tagDigitalSignaturesSequence, detached...))
	signatures := Signatures(obj.Elements())
	report, err = CheckProfileWithOptions(signatures[1], ProfileSRRSA, ProfileOptions{})
	if err != nil || !report.SRVerificationCandidate {
		t.Fatalf("structural candidate vs crypto distinction: %v %+v", err, report)
	}
	if Verify(obj)[0].Valid() {
		t.Fatal("corrupted candidate crypto unexpectedly valid")
	}
	// A purpose-only candidate with no readable RSA certificate is not eligible.
	broken := signatures[0]
	broken.Certificate = nil
	broken.dataSet = srProfileElements(true)
	if _, err := CheckProfileWithOptions(broken, ProfileSRRSA, ProfileOptions{}); !errors.Is(err, ErrProfileViolation) {
		t.Fatal(err)
	}
}

func TestProfileSRNegativeConditions(t *testing.T) {
	elements := srProfileElements(true)
	sig := structuralSignature(elements, core.Tag{})
	sig.Purposes = []Code{{Value: "5", Scheme: "ASTM-sigpurpose"}}
	variants := []struct {
		name   string
		change func(*Signature)
		code   string
		tag    core.Tag
	}{
		{"nested", func(s *Signature) { s.Path = "(0040,A730)[0]" }, "main-dataset-required", core.Tag{}},
		{"wrong sop", func(s *Signature) {
			s.dataSet = append([]core.Element(nil), s.dataSet...)
			s.dataSet[0] = uiElement(tagSOPClassUID, secondaryCaptureUID)
		}, "sr-or-kos-required", tagSOPClassUID},
		{"no purpose", func(s *Signature) { s.Purposes = nil }, "sr-purpose-required", tagDigitalSignaturePurposeCodeSeq},
		{"wrong scheme", func(s *Signature) { s.Purposes = []Code{{Value: "5", Scheme: "WRONG"}} }, "sr-verification-candidate-required", tagDigitalSignaturePurposeCodeSeq},
		{"missing observer signature", func(s *Signature) { s.DataElementsSigned = omitProfileTag(s.dataSet, tagVerifyingObserverSeq) }, "required-not-signed", tagVerifyingObserverSeq},
		{"missing nested date", func(s *Signature) {
			s.dataSet = append([]core.Element(nil), s.dataSet...)
			s.dataSet[len(s.dataSet)-1] = sequenceElement(tagVerifyingObserverSeq, core.DataSet{})
		}, "sr-verification-datetime-required", tagVerificationDateTime},
		{"two observers", func(s *Signature) {
			s.dataSet = append([]core.Element(nil), s.dataSet...)
			s.dataSet[len(s.dataSet)-1] = sequenceElement(tagVerifyingObserverSeq, core.DataSet{}, core.DataSet{})
		}, "sr-observer-count", tagVerifyingObserverSeq},
	}
	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			s := sig
			v.change(&s)
			report, err := CheckProfileWithOptions(s, ProfileSRRSA, ProfileOptions{})
			if !errors.Is(err, ErrProfileViolation) || !hasProfileIssue(report, v.code, v.tag) {
				t.Fatalf("%v %+v", err, report)
			}
		})
	}
	unverified := srProfileElements(false)
	// Optional references remain if-present; bad nested MAC evidence fails.
	ref := sequenceElement(tagCurrentRequestedEvid, core.DataSet{Elements: []core.Element{sequenceElement(core.NewTag(0x0400, 0x0403), core.DataSet{Elements: []core.Element{stringElement(tagMACAlgorithm, core.VRCS, "NOT_ALLOWED")}})}})
	unverified = append(unverified, ref)
	s := structuralSignature(unverified, core.Tag{})
	s.Purposes = []Code{{Value: "1", Scheme: "ASTM-sigpurpose"}}
	report, err := CheckProfileWithOptions(s, ProfileSRRSA, ProfileOptions{})
	if !errors.Is(err, ErrProfileViolation) || !hasProfileIssue(report, "sr-referenced-mac-algorithm", tagMACAlgorithm) {
		t.Fatalf("ref MAC %v %+v", err, report)
	}
	for _, sop := range []string{"1.2.840.10008.5.1.4.1.1.88.33", "1.2.840.10008.5.1.4.1.1.88.59"} {
		elements := srProfileElements(false)
		elements[0] = uiElement(tagSOPClassUID, sop)
		s := structuralSignature(elements, core.Tag{})
		s.Purposes = []Code{{Value: "1", Scheme: "ASTM-sigpurpose"}}
		if _, err := CheckProfileWithOptions(s, ProfileSRRSA, ProfileOptions{}); err != nil {
			t.Fatalf("SR/KOS optional evidence: %v", err)
		}
	}
}

func TestProfileBoundsCancelDeterminismPresenceAndAtomicSign(t *testing.T) {
	sig := structuralSignature(profileElements(), core.Tag{})
	// Editing the public AT list cannot invent an absent mandatory element.
	sig.dataSet = sig.dataSet[:3]
	report, err := CheckProfileWithOptions(sig, ProfileCreatorRSA, ProfileOptions{})
	if !errors.Is(err, ErrProfileViolation) || !hasProfileIssue(report, "required-absent", tagSeriesInstanceUID) {
		t.Fatalf("forged presence: %v %+v", err, report)
	}
	a, _ := CheckProfileWithOptions(sig, ProfileCreatorRSA, ProfileOptions{})
	b, _ := CheckProfileWithOptions(sig, ProfileCreatorRSA, ProfileOptions{})
	if !reflect.DeepEqual(a, b) {
		t.Fatal("report nondeterministic")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	report, err = CheckProfileContext(ctx, sig, ProfileCreatorRSA, ProfileOptions{})
	if !errors.Is(err, context.Canceled) || report.Coverage != ProfileCoveragePartial {
		t.Fatalf("cancel %v %+v", err, report)
	}
	seq := sequenceElement(tagContentSequence, core.DataSet{}, core.DataSet{}, core.DataSet{})
	for _, opts := range []ProfileOptions{{MaxElements: 2}, {MaxElements: 6}, {MaxDepth: -1}, {MaxDepth: 65}, {HumanVerifiedPaths: make([]string, 257)}} {
		s := structuralSignature(append(profileElements(), seq), core.Tag{})
		if _, err := CheckProfileWithOptions(s, ProfileCreatorRSA, opts); !errors.Is(err, ErrProfileLimit) {
			t.Fatalf("limit %+v %v", opts, err)
		}
	}
	// A cyclic caller-built sequence graph is rejected by the depth fence.
	cycle := []core.Element{sequenceElement(tagContentSequence)}
	cycle[0].Value = core.SequenceValue{Items: []core.DataSet{{Elements: cycle}}}
	if _, err := CheckProfileWithOptions(Signature{dataSet: cycle}, ProfileCreatorRSA, ProfileOptions{}); !errors.Is(err, ErrProfileLimit) {
		t.Fatal(err)
	}
	many := append(profileElements(), profileElement(0x00620002), profileElement(0x00189100))
	s := structuralSignature(many, core.Tag{})
	s.DataElementsSigned = nil
	report, err = CheckProfileWithOptions(s, ProfileCreatorRSA, ProfileOptions{MaxIssues: 1})
	if !errors.Is(err, ErrProfileLimit) || len(report.Issues) != 1 {
		t.Fatalf("issue bound %v %+v", err, report)
	}
	pki := testutil.NewTestPKI(t)
	signer := newSigner(t, pki, "atomic-profile.test", "rsa2048")
	nested := sequenceElement(tagContentSequence, core.DataSet{Elements: profileElements()})
	obj := newObject(append(profileElements(), nested), transfer.ExplicitVRLittleEndian)
	before := obj.Elements()
	opts := signer.options(MACSHA256)
	opts.Profile = ProfileCreatorRSA
	opts.Item = "ContentSequence[0]"
	if _, err := Sign(obj, opts); !errors.Is(err, ErrProfileViolation) {
		t.Fatalf("nested preflight %v", err)
	}
	if !reflect.DeepEqual(before, obj.Elements()) || len(Signatures(obj.Elements())) != 0 {
		t.Fatal("profile failure published a signature")
	}
	opts.Item = ""
	opts.Profile = ProfileAuthorizationRSA
	if _, err := Sign(obj, opts); !errors.Is(err, ErrProfileIncomplete) {
		t.Fatalf("atomic unknown human %v", err)
	}
	if !reflect.DeepEqual(before, obj.Elements()) {
		t.Fatal("unknown human context modified object")
	}
	bounded := opts
	bounded.Tags = make([]core.Tag, 10)
	bounded.ProfileOptions = ProfileOptions{HumanVerifiedPaths: []string{}, MaxElements: 5}
	if _, err := Sign(obj, bounded); !errors.Is(err, ErrProfileLimit) {
		t.Fatalf("sign preflight tags bound: %v", err)
	}
	if !reflect.DeepEqual(before, obj.Elements()) {
		t.Fatal("bounded preflight modified object")
	}
	opts.ProfileOptions = ProfileOptions{HumanVerifiedPaths: []string{}}
	if _, err := Sign(obj, opts); err != nil {
		t.Fatal(err)
	}
}
