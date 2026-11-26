package dsig

import (
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/testutil"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// Opt-in comparison against the pinned locally installed DCMTK. The oracle
// checks signature integrity and attribute subsets, not IOD conformance or
// the truth of an Authorization human declaration. Current C.2/C.3 lists
// remain authoritative when the older implementation disagrees.
func TestDCMTKProfileCoverageIndependentFixtures(t *testing.T) {
	tool := requireDCMSign(t)
	pki := testutil.NewTestPKI(t)
	s := newSigner(t, pki, "profile-oracle.test", "rsa2048")
	fixtures := []struct {
		name, sop string
		tag       uint32
		profile   Profile
		require   string
	}{
		{"Enhanced MR", "1.2.840.10008.5.1.4.1.1.4.1", 0x00189100, ProfileCreatorRSA, "+rc"},
		{"Enhanced CT", "1.2.840.10008.5.1.4.1.1.2.1", 0x00189361, ProfileCreatorRSA, "+rc"},
		{"SEG", "1.2.840.10008.5.1.4.1.1.66.4", 0x00620002, ProfileCreatorRSA, "+rc"},
		{"MRS", "1.2.840.10008.5.1.4.1.1.4.2", 0x00189052, ProfileAuthorizationRSA, "+ru"},
		{"Raw private", "1.2.840.10008.5.1.4.1.1.66", 0x00111001, ProfileCreatorRSA, "+rc"},
		{"Functional Groups", "1.2.840.10008.5.1.4.1.1.4.1", 0x52009229, ProfileCreatorRSA, "+rc"},
	}
	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			elements := profileElements()
			elements[0] = uiElement(tagSOPClassUID, f.sop)
			el := profileElement(f.tag)
			if f.tag == 0x00111001 {
				el = core.NewRawElement(core.NewTag(0x0011, 0x1001), core.VROB, []byte{1, 2})
				elements = append(elements, uiElement(core.NewTag(0x0008, 0x9123), "2.25.145211"), stringElement(core.NewTag(0x0011, 0x0010), core.VRLO, "SYNTHETIC"))
			}
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
				requireValid(t, obj, 1)
				_, profileErr := CheckProfileWithOptions(sig, f.profile, ProfileOptions{HumanVerifiedPaths: []string{}})
				if (profileErr != nil) != missing {
					t.Fatalf("Go profile mismatch missing=%v: %v", missing, profileErr)
				}
				path := writeTestFile(t, "coverage.dcm", transfer.ExplicitVRLittleEndian, obj.Elements())
				log, err := dcmsign(t, tool, "independent profile fixture: full accepted; one required present tag omitted rejected", "--verify", f.require, "+cf", pki.CAFile, path)
				t.Logf("fixture=%s missing=%v oracle-error=%v\n%s", f.name, missing, err, log)
				if (err != nil) != missing {
					t.Fatalf("oracle profile mismatch missing=%v: %v\n%s", missing, err, log)
				}
			}
		})
	}
	t.Run("verified SR nested date", func(t *testing.T) {
		elements := append(srProfileElements(true), sequenceElement(core.NewTag(0x0040, 0xA043), core.DataSet{Elements: []core.Element{stringElement(tagCodeValue, core.VRSH, "99"), stringElement(tagCodingSchemeDesignator, core.VRSH, "99TEST"), stringElement(tagCodeMeaning, core.VRLO, "Synthetic fixture")}}), sequenceElement(tagContentSequence, core.DataSet{Elements: []core.Element{stringElement(core.NewTag(0x0040, 0xA040), core.VRCS, "TEXT"), stringElement(core.NewTag(0x0040, 0xA160), core.VRUT, "Synthetic test content")}}))
		obj := newObject(elements, transfer.ExplicitVRLittleEndian)
		opts := s.options(MACSHA256)
		opts.Profile = ProfileSRRSA
		opts.Purpose = &Code{Value: "5", Scheme: "ASTM-sigpurpose", Meaning: "Verification Signature"}
		if _, err := Sign(obj, opts); err != nil {
			t.Fatal(err)
		}
		path := writeTestFile(t, "sr.dcm", transfer.ExplicitVRLittleEndian, obj.Elements())
		log, err := dcmsign(t, tool, "verified SR with nested observer VerificationDateTime accepted", "--verify", "+rs", "+cf", pki.CAFile, path)
		if err != nil {
			t.Fatalf("SR oracle %v\n%s", err, log)
		}
		t.Log(log)
	})
}

// These two current normative requirements are absent from the local DCMTK
// implementation. Record the difference instead of weakening our checker.
func TestDCMTKProfileCurrentNormativeDifferences(t *testing.T) {
	tool := requireDCMSign(t)
	pki := testutil.NewTestPKI(t)
	s := newSigner(t, pki, "profile-current.test", "rsa2048")
	for _, tag := range []uint32{0x501E0020, 0x0040B039} {
		el := profileElement(tag)
		elements := append(profileElements(), el)
		obj := newObject(elements, transfer.ExplicitVRLittleEndian)
		opts := s.options(MACSHA256)
		opts.Tags = omitProfileTag(elements, el.Tag())
		sig, err := Sign(obj, opts)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := CheckProfileWithOptions(sig, ProfileCreatorRSA, ProfileOptions{}); err == nil {
			t.Fatal("current normative omission accepted")
		}
		requireValid(t, obj, 1)
		path := writeTestFile(t, "current.dcm", transfer.ExplicitVRLittleEndian, obj.Elements())
		log, err := dcmsign(t, tool, "pinned DCMTK omits current waveform/retired Curve TypeOfData requirements", "--verify", "+rc", "+cf", pki.CAFile, path)
		t.Logf("required=%s Go=reject DCMTK-error=%v\n%s", el.Tag(), err, log)
		if err != nil {
			t.Fatalf("oracle changed; requalify recorded normative difference: %v\n%s", err, log)
		}
	}
}
