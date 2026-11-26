package printmgmt

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
)

func nativeTestTable(bits int) PresentationLUT {
	p := PresentationLUT{InputBits: bits, OutputBits: 12, Entries: make([]uint16, 1<<bits), Explanation: "SYNTHETIC"}
	for i := range p.Entries {
		p.Entries[i] = uint16(i * 4095 / (len(p.Entries) - 1))
	}
	return p
}
func TestNativeLUTShapesAndTables(t *testing.T) {
	for _, p := range []PresentationLUT{{Shape: "IDENTITY"}, {Shape: "LIN OD"}, nativeTestTable(8), nativeTestTable(12)} {
		ds, err := BuildPresentationLUT(p)
		if err != nil {
			t.Fatal(err)
		}
		got, err := parsePresentationLUT(ds)
		if err != nil || got.Shape != p.Shape || got.InputBits != p.InputBits || got.OutputBits != p.OutputBits || len(got.Entries) != len(p.Entries) {
			t.Fatalf("roundtrip %+v %v", got, err)
		}
		bits := p.InputBits
		if bits == 0 {
			bits = 8
		}
		lut, err := p.DisplayLUT(bits, display.DefaultPrintDensity, nil)
		if err != nil || lut == nil {
			t.Fatalf("display LUT %v", err)
		}
	}
	p := nativeTestTable(8)
	p.Entries[7] = 4096
	if p.Validate() == nil {
		t.Fatal("out-of-precision table accepted")
	}
	if (PresentationLUT{Shape: "INVERSE"}).Validate() == nil {
		t.Fatal("softcopy shape accepted for hardcopy")
	}
	if _, err := nativeTestTable(12).DisplayLUT(8, display.DefaultPrintDensity, nil); err == nil {
		t.Fatal("depth mismatch accepted")
	}
}
func TestNativeLUTResidentBoundsAndStatuses(t *testing.T) {
	base, _ := BuildPresentationLUT(nativeTestTable(8))
	items, _ := base.GetSequence(TagPresentationLUTSequence)
	for name, element := range map[string]core.Element{
		"wrong descriptor VR": {Header: core.ElementHeader{Tag: TagLUTDescriptor, VR: core.VRLO}, Value: core.Uint16Value{256, 0, 12}},
		"descriptor VM":       {Header: core.ElementHeader{Tag: TagLUTDescriptor, VR: core.VRUS}, Value: core.Uint16Value{256, 0}},
		"first mapped":        {Header: core.ElementHeader{Tag: TagLUTDescriptor, VR: core.VRUS}, Value: core.Uint16Value{256, 1, 12}},
		"count zero":          {Header: core.ElementHeader{Tag: TagLUTDescriptor, VR: core.VRUS}, Value: core.Uint16Value{0, 0, 12}},
		"data VR":             core.NewRawElement(TagLUTData, core.VROB, make([]byte, 512)),
		"data short":          core.NewRawElement(TagLUTData, core.VROW, make([]byte, 510)),
		"data oversized":      core.NewRawElement(TagLUTData, core.VROW, make([]byte, 8194)),
		"nil resident":        {Header: core.ElementHeader{Tag: TagLUTData, VR: core.VROW}},
		"opaque data":         {Header: core.ElementHeader{Tag: TagLUTData, VR: core.VROW}, Value: physicalOpaqueValue{}},
		"opaque descriptor":   {Header: core.ElementHeader{Tag: TagLUTDescriptor, VR: core.VRUS}, Value: physicalOpaqueValue{}},
	} {
		t.Run(name, func(t *testing.T) {
			item := object.FromElements(items[0].Elements(), nil)
			item.Put(element)
			ds := object.FromElements([]core.Element{sequenceOf(TagPresentationLUTSequence, core.DataSet{Elements: item.Elements()})}, nil)
			if _, err := parsePresentationLUT(ds); statusOf(err) != StatusInvalidAttributeValue {
				t.Fatalf("accepted/misclassified %v", err)
			}
		})
	}
	if _, err := parsePresentationLUT(object.New(nil)); statusOf(err) != StatusMissingAttribute {
		t.Fatalf("missing status %v", err)
	}
	ds, _ := BuildPresentationLUT(nativeTestTable(12))
	if _, err := parsePresentationLUTBudget(ds, 8191); statusOf(err) != StatusResourceLimitation {
		t.Fatalf("preallocation budget %v", err)
	}
	shape, _ := BuildPresentationLUT(PresentationLUT{Shape: "IDENTITY"})
	shape.Put(core.NewRawElement(TagPresentationLUTShape, core.VRCS, []byte("IDENTITY         ")))
	if _, err := parsePresentationLUT(shape); err == nil {
		t.Fatal("resident CS overlength accepted")
	}
}
func TestNativeAnnotationResidentCharsetAndPadding(t *testing.T) {
	for _, tc := range []struct {
		name, text, cs string
		valid          bool
	}{
		{"ascii64", strings.Repeat("A", 64), "", true},
		{"utf8-64", strings.Repeat("é", 64), "ISO_IR 192", true},
		{"too-many", strings.Repeat("A", 65), "", false},
		{"undeclared-utf8", "é", "", false},
		{"unsupported-charset", "A", "ISO_IR 100", false},
		{"tab", "A\t", "", false},
		{"c1", "A\u0081", "ISO_IR 192", false},
		{"multi-value", "A\\B", "", false},
		{"odd64padding", strings.Repeat("A", 63) + "é", "ISO_IR 192", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := BuildAnnotationBox(1, tc.text, tc.cs)
			if (err == nil) != tc.valid {
				t.Fatalf("valid %t: %v", tc.valid, err)
			}
		})
	}
	for _, e := range []core.Element{
		core.NewRawElement(TagAnnotationText, core.VRLO, []byte("A"+strings.Repeat(" ", 64))),
		{Header: core.ElementHeader{Tag: TagAnnotationText, VR: core.VRLO}, Value: core.StringValue{"A", "B"}},
		core.NewRawElement(TagAnnotationText, core.VRSH, []byte("A")),
		{Header: core.ElementHeader{Tag: TagAnnotationText, VR: core.VRLO}},
		{Header: core.ElementHeader{Tag: TagAnnotationText, VR: core.VRLO}, Value: physicalOpaqueValue{}},
	} {
		if _, _, err := nativeText(object.FromElements([]core.Element{e}, nil), TagAnnotationText, ""); err == nil {
			t.Fatal("malformed resident text accepted")
		}
	}
	ds := object.FromElements([]core.Element{core.NewRawElement(TagAnnotationText, core.VRLO, []byte{})}, nil)
	text, present, err := nativeText(ds, TagAnnotationText, "")
	if err != nil || !present || text != "" {
		t.Fatal("zero length did not clear")
	}
}
func TestNativeDensityExplicitZeroAndNoFallback(t *testing.T) {
	p := FilmBoxPresentation{MinDensitySet: true, ReflectedAmbientLightSet: true}
	d, err := p.NativePrintDensity()
	if err != nil || d.MinDensity != 0 || d.ReflectedAmbientLight != 0 {
		t.Fatalf("explicit zero %+v %v", d, err)
	}
	p = FilmBoxPresentation{MinDensity: 300, MaxDensity: 100}
	if _, err := p.NativePrintDensity(); err == nil {
		t.Fatal("invalid density silently replaced")
	}
	p = FilmBoxPresentation{IlluminationSet: true}
	if _, err := p.NativePrintDensity(); err == nil {
		t.Fatal("zero illumination accepted")
	}
	for _, p := range []FilmBoxPresentation{{MinDensity: -1}, {MaxDensity: 65536}, {Illumination: 65536}, {ReflectedAmbientLight: -1}} {
		if _, err := p.NativePrintDensity(); err == nil {
			t.Fatal("out-of-US density admitted", p)
		}
	}
	p = FilmBoxPresentation{MinDensitySet: true, ReflectedAmbientLightSet: true}
	if p.PrintDensity() != display.DefaultPrintDensity {
		t.Fatal("legacy density defaults changed")
	}
}

type nativeModelBackend struct{ *MemoryBackend }

func (nativeModelBackend) HonorsPresentationLUTs() bool     { return true }
func (nativeModelBackend) HonorsBasicAnnotationBoxes() bool { return true }
func TestNativeBackendCommitmentAndAssociationBudgets(t *testing.T) {
	if _, err := NewService(NewMemoryBackend(1), ServiceOptions{Native: &NativeServiceOptions{PresentationLUT: true}}); !errors.Is(err, ErrNativeUnsupported) {
		t.Fatalf("capture backend advertised composition %v", err)
	}
	opt := &NativeServiceOptions{PresentationLUT: true, AnnotationFormats: []AnnotationFormat{{ID: "SYNTHETIC", Slots: 2}}, Limits: NativeLimits{MaxPresentationLUTs: 1, MaxLUTBytes: 512}}
	s, err := NewService(nativeModelBackend{NewMemoryBackend(1)}, ServiceOptions{Native: opt})
	if err != nil {
		t.Fatal(err)
	}
	opt.AnnotationFormats[0].Slots = 64
	if s.native.AnnotationFormats[0].Slots != 2 {
		t.Fatal("caller mutation changed service")
	}
	b := s.Bind(&ul.Association{AcceptedContexts: []ul.AcceptedContext{{AbstractSyntaxUID: PresentationLUTSOPClassUID}}})
	ds, _ := BuildPresentationLUT(nativeTestTable(8))
	r, err := b.createPresentationLUT(dimse.NormalizedCreateRequest{AffectedSOPInstanceUID: "1.2.3.4"}, ds)
	if err != nil || r.Response.Status != 0 {
		t.Fatal(err)
	}
	if _, err := b.createPresentationLUT(dimse.NormalizedCreateRequest{}, ds); statusOf(err) != StatusResourceLimitation {
		t.Fatalf("count budget %v", err)
	}
	b.Close()
	if len(b.luts) != 0 {
		t.Fatal("close retained LUTs")
	}
	if _, err := b.createPresentationLUT(dimse.NormalizedCreateRequest{}, ds); err == nil {
		t.Fatal("closed binding accepted create")
	}
	if _, err := NewClient(nil, false).PrintFilm(context.Background(), Film{Columns: 1, Rows: 1, Native: NativeFilmOptions{PresentationLUT: &PresentationLUT{Shape: "IDENTITY"}}}); !errors.Is(err, ErrNativeUnsupported) {
		t.Fatalf("unnegotiated native request sent %v", err)
	}
}
