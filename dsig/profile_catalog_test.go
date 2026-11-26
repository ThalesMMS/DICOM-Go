package dsig

import (
	"crypto/rsa"
	"crypto/x509"
	"errors"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/iodmodules"
)

func profileElements() []core.Element {
	return []core.Element{
		uiElement(tagSOPClassUID, secondaryCaptureUID), uiElement(tagSOPInstanceUID, "2.25.145201"),
		uiElement(tagStudyInstanceUID, "2.25.145202"), uiElement(tagSeriesInstanceUID, "2.25.145203"),
	}
}
func profileElement(value uint32) core.Element {
	tag := core.NewTag(uint16(value>>16), uint16(value))
	entry, ok := std.Dictionary.ByTag(tag)
	vr := entry.VR
	if !ok || vr == "" || vr == core.VRUN {
		vr = core.VROB
	}
	if vr == core.VRSQ {
		return sequenceElement(tag, core.DataSet{Elements: []core.Element{stringElement(tagCodeValue, core.VRSH, "TEST")}})
	}
	switch vr {
	case core.VRUI:
		return uiElement(tag, "2.25.145204")
	case core.VRUS, core.VRSS:
		return core.NewRawElement(tag, vr, []byte{0, 0})
	case core.VRUL, core.VRSL, core.VRFL, core.VROF:
		return core.NewRawElement(tag, vr, make([]byte, 4))
	case core.VRFD, core.VROD, core.VRUV, core.VRSV:
		return core.NewRawElement(tag, vr, make([]byte, 8))
	default:
		return core.NewRawElement(tag, vr, []byte("FIXTURE "))
	}
}

// This fixture tests structural coverage, not a valid IOD or crypto bytes.
// The independent constants below come from the cited PS3.3 tables, not from
// profileModules/requiredProfileTags. Cryptographic negative fixtures are in
// profile_conformance_test.go.
func structuralSignature(elements []core.Element, omit core.Tag) Signature {
	s := Signature{MACAlgorithm: MACSHA256, CertificateType: CertificateTypeX509, Certificate: &x509.Certificate{PublicKey: &rsa.PublicKey{}}, dataSet: elements}
	for _, el := range elements {
		if el.Tag() != omit && !excluded(el) {
			s.DataElementsSigned = append(s.DataElementsSigned, el.Tag())
		}
	}
	return s
}

func TestProfileNormativeModuleFixtures(t *testing.T) {
	fixtures := []struct {
		table         string
		tag           uint32
		creator, auth bool
	}{
		{"C.7-8", 0x0018100A, true, false}, {"C.7-8 manufacture", 0x00181204, true, false},
		{"C.7-9 anatomy Include", 0x00082218, true, true}, {"C.7-9 RWVM", 0x00409096, true, true},
		{"C.7-11a EOT", 0x7FE00001, true, true}, {"C.7-11a padding", 0x00280121, true, true}, {"C.7-11a color", 0x00282002, true, true},
		{"C.9-2 last overlay", 0x601E3000, true, true}, {"2004 C.10-2 curve TypeOfData", 0x501E0020, true, true},
		{"C.10-5", 0x00700001, true, true}, {"C.17-2", 0x0040A491, true, true}, {"C.17-4 Include", 0x0040A124, true, true},
		{"C.10-9 display scale", 0x003A0230, true, true}, {"C.10-11", 0x0040B020, true, true},
		{"C.7.6.16-1 shared", 0x52009229, true, true}, {"C.7.6.16-1 per-frame", 0x52009230, true, true},
		{"C.8-79 Enhanced MR", 0x00189100, true, true}, {"C.8-102 MRS", 0x00189052, true, true},
		{"C.8-103 MRS pulse", 0x00189005, false, true}, {"C.8-106 MRS data", 0x56000020, false, true},
		{"C.19-1 Raw", 0x00089123, true, true}, {"C.8-114 Enhanced CT", 0x00189361, true, true},
		{"C.8.19.2-1 XA complement", 0x00189457, true, true}, {"C.8.20-2 SEG", 0x00620002, true, true},
		{"C.24-1 Encapsulated Series", 0x0008103F, true, false}, {"C.24-2 Encapsulated Document", 0x00420010, false, true},
		{"C.8.21.1-1 Xray 3D", 0x00083011, true, true}, {"C.8.22-3 PET", 0x00189073, true, true},
		{"C.8.24.3-1 US", 0x00185010, true, true}, {"C.8.23-1 Surface SEG", 0x00620002, true, true},
		{"C.27-1 Surface Mesh", 0x00660002, true, true}, {"C.11.16-1 Display", 0x00720002, true, true},
		{"C.11.17-1 Image Box", 0x00720422, true, true}, {"C.11.18-1 Annotation", 0x00720424, true, true},
		{"C.29.1.1-1 Implant description", 0x00686210, true, true}, {"C.29.1.2-1 Implant 2D", 0x006862C0, true, true},
		{"C.29.1.3-1 Implant 3D", 0x00686350, true, true}, {"C.29.1.4-1 Implant mating", 0x006863B0, true, true},
		{"C.29.1.5-1 Implant landmarks", 0x00686500, true, true}, {"C.29.2-1 Assembly", 0x00760001, true, true},
		{"C.29.3-1 Group", 0x00780001, true, true}, {"C.27.5-1 Point Cloud", 0x00800006, true, true},
		{"C.8.31-1 Mammography", 0x001811A0, true, true}, {"C.8.33-1 Tractography Series", 0x00081111, true, false},
		{"C.8.33-2 Tractography Results", 0x00660101, true, false}, {"C.11.28-1 Volumetric", 0x00701901, true, true},
		{"C.37.1-2 Microscopy", 0x006A0002, true, false}, {"C.39.1-1 relationship complement", 0x00081115, true, false},
		{"C.39.2-1 structured waveform", 0x0040B030, true, false}, {"C.39.3-1 textual waveform", 0x0040B033, true, false},
		{"C.39.4-1 displayed segment", 0x0040B035, true, false}, {"C.39.5-1 montage activation", 0x0040B037, true, false},
		{"C.39.6-1 waveform montage", 0x0040B039, true, false},
	}
	for _, f := range fixtures {
		t.Run(f.table, func(t *testing.T) {
			element := profileElement(f.tag)
			elements := append(profileElements(), element)
			for _, profile := range []Profile{ProfileCreatorRSA, ProfileAuthorizationRSA} {
				options := ProfileOptions{HumanVerifiedPaths: []string{}}
				if _, err := CheckProfileWithOptions(structuralSignature(elements, core.Tag{}), profile, options); err != nil {
					t.Fatalf("positive %s: %v", profile, err)
				}
				report, err := CheckProfileWithOptions(structuralSignature(elements, element.Tag()), profile, options)
				want := f.creator
				if profile == ProfileAuthorizationRSA {
					want = f.auth
				}
				if errors.Is(err, ErrProfileViolation) != want {
					t.Fatalf("missing %s %s want violation=%v: %v, %+v", element.Tag(), profile, want, err, report)
				}
			}
		})
	}
}

func TestProfileCatalogMissingModuleFailsClosed(t *testing.T) {
	for _, lookup := range []func(string) ([]iodmodules.Attribute, bool){func(string) ([]iodmodules.Attribute, bool) { return nil, false }, func(string) ([]iodmodules.Attribute, bool) { return nil, true }} {
		if _, err := buildProfileCatalog([]profileModule{{name: "required", table: "primary", profiles: []Profile{ProfileCreatorRSA}}}, lookup); !errors.Is(err, ErrProfileIncomplete) {
			t.Fatal(err)
		}
	}
	// Result slices are caller-owned: mutation cannot corrupt the private catalog.
	signature := structuralSignature(append(profileElements(), profileElement(0x00620002)), core.Tag{})
	a, err := CheckProfileWithOptions(signature, ProfileCreatorRSA, ProfileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	before := append([]core.Tag(nil), a.RequiredTags...)
	a.RequiredTags[0] = core.Tag{}
	b, _ := CheckProfileWithOptions(signature, ProfileCreatorRSA, ProfileOptions{})
	if !reflect.DeepEqual(before, b.RequiredTags) {
		t.Fatal("report aliases catalog")
	}
}

func TestProfileRepeatingGroupsAreExact(t *testing.T) {
	for _, value := range []uint32{0x601E0001, 0x60203000, 0x60013000, 0x50203000, 0x50010020, 0x501E7777} {
		el := core.NewRawElement(core.NewTag(uint16(value>>16), uint16(value)), core.VROB, []byte{1, 2})
		if _, err := CheckProfileWithOptions(structuralSignature(append(profileElements(), el), el.Tag()), ProfileCreatorRSA, ProfileOptions{}); err != nil {
			t.Fatalf("outside normative module %s: %v", el.Tag(), err)
		}
	}
}
