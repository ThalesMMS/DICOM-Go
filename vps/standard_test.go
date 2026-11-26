package vps

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/commontest"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
	"github.com/ThalesMMS/dicom-go/render"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestStandardGrayscalePlanarMPRRoundTrip(t *testing.T) {
	state := &State{
		SOPClassUID:         GrayscalePlanarMPRVolumetricPresentationStateStorage,
		SOPInstanceUID:      "1.2.826.0.1.3680043.9.7433.741.1",
		StudyInstanceUID:    "1.2.826.0.1.3680043.9.7433.741.9102",
		SeriesInstanceUID:   "1.2.826.0.1.3680043.9.7433.741.9103",
		FrameOfReferenceUID: "1.2.826.0.1.3680043.9.7433.741.9107",
		Inputs: []Input{{
			Number:                   1,
			InputSetUID:              "1.2.826.0.1.3680043.9.7433.741.9128",
			ReferencedInstances:      []ReferencedInstance{{SOPClassUID: "1.2.840.10008.5.1.4.1.1.2", SOPInstanceUID: "1.2.3.4.1"}},
			CropSpecificationNumbers: []int{1},
			VOI:                      &VOI{WindowCenter: 40, WindowWidth: 400},
			RenderingMethod:          RenderingMaximumIP,
		}},
		CroppingSpecifications: []CroppingSpecification{{
			Number: 1, Method: CropBoundingBox, BoundingBox: [6]float64{-100, 100, -80, 80, -60, 60},
		}},
		MPRGeometry: &MPRGeometry{
			ThicknessType:   MPRThicknessSlab,
			SlabThickness:   12,
			TopLeft:         render.Vec3{X: -100, Y: -80, Z: 10},
			WidthDirection:  render.Vec3{X: 1},
			Width:           200,
			HeightDirection: render.Vec3{Y: 1},
			Height:          160,
		},
		Display: &Display{PixelPresentation: PixelPresentationMonochrome, PresentationLUTShape: PresentationLUTIdentity},
	}

	roundTrip := roundTripStandardState(t, state)
	if roundTrip.FrameOfReferenceUID != state.FrameOfReferenceUID {
		t.Fatalf("FrameOfReferenceUID = %q, want %q", roundTrip.FrameOfReferenceUID, state.FrameOfReferenceUID)
	}
	if !reflect.DeepEqual(roundTrip.Inputs, state.Inputs) {
		t.Fatalf("Inputs = %#v, want %#v", roundTrip.Inputs, state.Inputs)
	}
	if !reflect.DeepEqual(roundTrip.CroppingSpecifications, state.CroppingSpecifications) || !reflect.DeepEqual(roundTrip.MPRGeometry, state.MPRGeometry) {
		t.Fatalf("standard geometry/crop did not round trip: %#v", roundTrip)
	}
	if !reflect.DeepEqual(roundTrip.Display, state.Display) {
		t.Fatalf("Display = %#v, want %#v", roundTrip.Display, state.Display)
	}
	applied, err := Apply(roundTrip)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if applied.Preset.Mode != render.VRModeMIP || applied.VOI == nil || applied.VOI.WindowCenter != 40 || applied.VOI.WindowWidth != 400 {
		t.Fatalf("AppliedState = %#v, want MIP and WC/WW", applied)
	}
}

func TestStandardVolumeRenderingRoundTrip(t *testing.T) {
	state := standardVRFixtureState()
	withCommon := cloneStandardVRFixture(state)
	withCommon.Common = object.DerivedCommon{Source: commontest.Source()}
	if common, err := Write(withCommon); err != nil {
		t.Fatalf("Write with common modules: %v", err)
	} else {
		commontest.Check(t, common.Dataset, true)
	}
	state.SOPInstanceUID = "1.2.826.0.1.3680043.9.7433.741.2"
	state.Shading = &Shading{Style: ShadingDoubleSided, Ambient: 0.2, LightDirection: render.Vec3{Y: 1}, Diffuse: 0.7, Specular: 0.1, Shininess: 0.4}

	roundTrip := roundTripStandardState(t, state)
	if !reflect.DeepEqual(roundTrip.VolumeRenderGeometry, state.VolumeRenderGeometry) || !reflect.DeepEqual(roundTrip.Display, state.Display) || !reflect.DeepEqual(roundTrip.Shading, state.Shading) {
		t.Fatalf("standard VR modules did not round trip: got %#v", roundTrip)
	}
	if roundTrip.RenderPresetName != "" {
		t.Fatalf("RenderPresetName = %q, standard path must not synthesize a private preset", roundTrip.RenderPresetName)
	}
	file, err := Write(state)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if file.Dataset.Has(tagPrivateCreator) {
		t.Fatal("standard VPS unexpectedly contains private creator")
	}
	applied, err := Apply(roundTrip)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if applied.Preset.Mode != render.VRModeDVR || !applied.Preset.ShadingDefault || applied.VolumeRenderGeometry == nil {
		t.Fatalf("AppliedState = %#v, want standard DVR geometry and shading", applied)
	}
}

func TestStandardVolumeRenderingTableClassificationRoundTrip(t *testing.T) {
	state := standardVRFixtureState()
	state.Display.Classification[0] = ClassificationComponent{
		InputNumbers:          []int{1},
		RGBTransferFunction:   RGBTransferTable,
		AlphaTransferFunction: AlphaTransferTable,
		PaletteDescriptor:     [3]uint16{4, 0, 16},
		RedPalette:            []uint16{0, 1000, 40000, 65535},
		GreenPalette:          []uint16{0, 2000, 30000, 60000},
		BluePalette:           []uint16{0, 3000, 20000, 50000},
		AlphaPalette:          []uint16{0, 5140, 50115, 65535},
		Description:           "Clinical table",
	}
	file, err := Write(state)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	direct, err := Read(file.Dataset)
	if err != nil {
		t.Fatalf("Read typed OW: %v", err)
	}
	if !reflect.DeepEqual(direct.Display, state.Display) {
		t.Fatalf("typed OW display = %#v, want %#v", direct.Display, state.Display)
	}
	roundTrip := roundTripStandardState(t, state)
	if !reflect.DeepEqual(roundTrip.Display, state.Display) {
		t.Fatalf("TABLE display = %#v, want %#v", roundTrip.Display, state.Display)
	}
	applied, err := Apply(roundTrip)
	if err != nil {
		t.Fatal(err)
	}
	if applied.Preset.TF.Domain() != render.VRTransferDomainNormalized || applied.Preset.Name != "Clinical table" {
		t.Fatalf("TABLE preset = %#v", applied.Preset)
	}
	lut := applied.Preset.TF.BakeLUT(0, 1, 4)
	third := lut.At(2)
	if math.Abs(third.A-float64(50115)/65535) > 1e-12 {
		t.Fatalf("TABLE alpha[2] = %.16f", third.A)
	}
}

func TestStandardVolumeRenderingPaletteUsesSelectedByteOrder(t *testing.T) {
	var encoded bytes.Buffer
	if err := parser.NewWriter(&encoded, transfer.ExplicitVRBigEndian).WriteElement(
		wordPalette(tagRedPaletteData, []uint16{0x0102, 0x0304}),
	); err != nil {
		t.Fatal(err)
	}
	wantSuffix := []byte{0x01, 0x02, 0x03, 0x04}
	if got := encoded.Bytes(); len(got) < len(wantSuffix) || !bytes.Equal(got[len(got)-len(wantSuffix):], wantSuffix) {
		t.Fatalf("big-endian palette bytes = % x, want suffix % x", got, wantSuffix)
	}
}

func TestStandardVPSFailsClosedForUnsupportedAndMalformedModules(t *testing.T) {
	valid := standardVRFixtureState()
	tests := []struct {
		name   string
		mutate func(*State)
		want   error
	}{
		{name: "table classification missing palette", mutate: func(s *State) { s.Display.Classification[0].RGBTransferFunction = RGBTransferTable }, want: ErrUnsupportedPayload},
		{name: "two input component", mutate: func(s *State) { s.Display.Classification[0].InputNumbers = []int{1, 2} }, want: ErrUnsupportedPayload},
		{name: "segmentation crop", mutate: func(s *State) { s.CroppingSpecifications[0].Method = "INCLUDE_SEG" }, want: ErrUnsupportedPayload},
		{name: "missing crop reference", mutate: func(s *State) { s.GlobalCropSpecificationNumbers = []int{2} }, want: ErrInvalidObject},
		{name: "zero window width", mutate: func(s *State) { s.Inputs[0].VOI.WindowWidth = 0 }, want: ErrInvalidObject},
		{name: "invalid ICC", mutate: func(s *State) { s.Display.ICCProfile = []byte("not an ICC profile") }, want: ErrInvalidObject},
		{name: "missing frame of reference", mutate: func(s *State) { s.FrameOfReferenceUID = "" }, want: ErrInvalidObject},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := cloneStandardVRFixture(valid)
			test.mutate(state)
			_, err := Write(state)
			if !errors.Is(err, test.want) {
				t.Fatalf("Write error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestStandardVPSReadAllowsLegacyMissingFrameOfReference(t *testing.T) {
	state := standardVRFixtureState()
	file, err := Write(state)
	if err != nil {
		t.Fatal(err)
	}
	elements := file.Dataset.Elements()
	filtered := elements[:0]
	frameOfReferenceUID := core.NewTag(0x0020, 0x0052)
	for _, element := range elements {
		if element.Header.Tag != frameOfReferenceUID {
			filtered = append(filtered, element)
		}
	}
	got, err := Read(object.FromElements(filtered, std.Dictionary))
	if err != nil {
		t.Fatalf("Read legacy standard VPS: %v", err)
	}
	if got.FrameOfReferenceUID != "" {
		t.Fatalf("FrameOfReferenceUID = %q, want empty legacy value", got.FrameOfReferenceUID)
	}
	state.FrameOfReferenceUID = ""
	if _, err := Write(state); !errors.Is(err, ErrInvalidObject) {
		t.Fatalf("Write error = %v, want strict ErrInvalidObject", err)
	}
}

func TestSRGBICCProfileReturnsDefensiveValidCopies(t *testing.T) {
	first, second := SRGBICCProfile(), SRGBICCProfile()
	if !validRGBICCProfile(first) || len(first) < 128 {
		t.Fatalf("profile is not a valid RGB ICC header")
	}
	first[16] = 'X'
	if second[16] != 'R' {
		t.Fatal("SRGBICCProfile returned shared mutable storage")
	}
}

func roundTripStandardState(t *testing.T, state *State) *State {
	t.Helper()
	file, err := Write(state)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	var encoded bytes.Buffer
	if err := object.WriteFile(&encoded, file); err != nil {
		t.Fatalf("object.WriteFile: %v", err)
	}
	readFile, err := object.ReadFile(bytes.NewReader(encoded.Bytes()))
	if err != nil {
		t.Fatalf("object.ReadFile: %v", err)
	}
	roundTrip, err := Read(readFile.Dataset)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return roundTrip
}

func syntheticRGBICCProfile() []byte {
	profile := make([]byte, 128)
	profile[3] = 128
	copy(profile[16:20], "RGB ")
	copy(profile[20:24], "XYZ ")
	copy(profile[36:40], "acsp")
	return profile
}

func standardVRFixtureState() *State {
	return &State{
		SOPClassUID:         VolumeRenderingVolumetricPresentationStateStorage,
		SOPInstanceUID:      "1.2.3",
		StudyInstanceUID:    "1.2.4",
		SeriesInstanceUID:   "1.2.5",
		FrameOfReferenceUID: "1.2.9",
		Inputs: []Input{{
			Number: 1, InputSetUID: "1.2.6",
			ReferencedInstances: []ReferencedInstance{{SOPClassUID: "1.2.7", SOPInstanceUID: "1.2.8"}},
			VOI:                 &VOI{WindowCenter: 40, WindowWidth: 400},
		}},
		GlobalCropSpecificationNumbers: []int{1},
		CroppingSpecifications: []CroppingSpecification{{
			Number: 1, Method: CropBoundingBox, BoundingBox: [6]float64{-1, 1, -1, 1, -1, 1},
		}},
		VolumeRenderGeometry: &VolumeRenderGeometry{
			Projection:  ProjectionPerspective,
			Position:    render.Vec3{Y: -10},
			LookAt:      render.Vec3{},
			Up:          render.Vec3{Z: 1},
			FieldOfView: [6]float64{-5, 5, 5, -5, 1, 20},
		},
		RenderingMethod: RenderingVolumeRendered,
		Display: &Display{
			PixelPresentation: PixelPresentationTrueColor,
			InputSetUID:       "1.2.6",
			Classification: []ClassificationComponent{{
				InputNumbers: []int{1}, RGBTransferFunction: RGBTransferEqual, AlphaTransferFunction: AlphaTransferIdentity,
			}},
			ICCProfile: SRGBICCProfile(),
			ColorSpace: "SRGB",
		},
	}
}

func cloneStandardVRFixture(source *State) *State {
	out := *source
	out.Inputs = append([]Input(nil), source.Inputs...)
	for i := range out.Inputs {
		out.Inputs[i].ReferencedInstances = append([]ReferencedInstance(nil), source.Inputs[i].ReferencedInstances...)
		if source.Inputs[i].VOI != nil {
			value := *source.Inputs[i].VOI
			out.Inputs[i].VOI = &value
		}
	}
	out.GlobalCropSpecificationNumbers = append([]int(nil), source.GlobalCropSpecificationNumbers...)
	out.CroppingSpecifications = append([]CroppingSpecification(nil), source.CroppingSpecifications...)
	geometry := *source.VolumeRenderGeometry
	out.VolumeRenderGeometry = &geometry
	display := *source.Display
	display.ICCProfile = append([]byte(nil), source.Display.ICCProfile...)
	display.Classification = append([]ClassificationComponent(nil), source.Display.Classification...)
	for i := range display.Classification {
		display.Classification[i].InputNumbers = append([]int(nil), source.Display.Classification[i].InputNumbers...)
		display.Classification[i].RedPalette = append([]uint16(nil), source.Display.Classification[i].RedPalette...)
		display.Classification[i].GreenPalette = append([]uint16(nil), source.Display.Classification[i].GreenPalette...)
		display.Classification[i].BluePalette = append([]uint16(nil), source.Display.Classification[i].BluePalette...)
		display.Classification[i].AlphaPalette = append([]uint16(nil), source.Display.Classification[i].AlphaPalette...)
	}
	out.Display = &display
	return &out
}

// C.11.24 encodes opposite XYZ corners, even when one patient coordinate is
// equal or reversed along an obliquely oriented encoded volume.
func TestStandardVPSBoundingBoxUsesOppositeXYZCorners(t *testing.T) {
	for _, box := range [][6]float64{
		{-100, 100, -80, 80, -60, 60},
		{100, 100, -80, 80, -60, 60},
		{100, -100, 80, -80, 60, -60},
	} {
		state := standardVRFixtureState()
		state.CroppingSpecifications[0].BoundingBox = box
		file, err := Write(state)
		if err != nil {
			t.Fatal(err)
		}
		items := derivedio.Sequence(file.Dataset, tagVolumeCroppingSequence)
		if len(items) != 1 {
			t.Fatalf("crop sequence length = %d", len(items))
		}
		encoded := derivedio.Floats(items[0], tagBoundingBoxCrop)
		want := []float64{box[0], box[2], box[4], box[1], box[3], box[5]}
		if !reflect.DeepEqual(encoded, want) {
			t.Fatalf("opposite XYZ corners = %v, want %v", encoded, want)
		}
		got := roundTripStandardState(t, state)
		if got.CroppingSpecifications[0].BoundingBox != box {
			t.Fatalf("round-trip crop = %v, want %v", got.CroppingSpecifications[0].BoundingBox, box)
		}
	}
}

func TestStandardVPSBoundingBoxRejectsCoincidentCorners(t *testing.T) {
	state := standardVRFixtureState()
	state.CroppingSpecifications[0].BoundingBox = [6]float64{2, 2, 3, 3, 4, 4}
	if file, err := Write(state); !errors.Is(err, ErrInvalidObject) || file != nil {
		t.Fatalf("coincident corners produced file=%v error=%v", file, err)
	}
}

func TestStandardVPSTableAlphaDescriptorIsRequiredAndConsistent(t *testing.T) {
	state := standardVRFixtureState()
	state.Display.Classification[0] = ClassificationComponent{
		InputNumbers: []int{1}, RGBTransferFunction: RGBTransferTable, AlphaTransferFunction: AlphaTransferTable,
		PaletteDescriptor: [3]uint16{2, 0, 16},
		RedPalette:        []uint16{0, 65535}, GreenPalette: []uint16{0, 65535},
		BluePalette: []uint16{0, 65535}, AlphaPalette: []uint16{0, 65535},
	}
	for _, values := range [][]uint16{nil, {2, 1, 8}, {2, 0, 16}, {3, 0, 8}} {
		file, err := Write(state)
		if err != nil {
			t.Fatal(err)
		}
		streams := derivedio.Sequence(file.Dataset, tagVolumeStreamSequence)
		components := derivedio.Sequence(streams[0], tagClassificationSequence)
		if got := derivedio.Ints(components[0], tagAlphaPaletteDescriptor); !reflect.DeepEqual(got, []int64{2, 0, 8}) {
			t.Fatalf("authored alpha descriptor = %v", got)
		}
		components[0].Put(derivedio.US(tagAlphaPaletteDescriptor, values...))
		streams[0].SetSequence(tagClassificationSequence, components...)
		file.Dataset.SetSequence(tagVolumeStreamSequence, streams...)
		if parsed, err := Read(file.Dataset); parsed != nil || !errors.Is(err, ErrInvalidObject) {
			t.Fatalf("alpha descriptor %v yielded state=%#v err=%v", values, parsed, err)
		}
	}
}

func TestStandardVPSExplicitLegacyWireMigration(t *testing.T) {
	state := standardVRFixtureState()
	box := [6]float64{-100, 100, -80, 80, -60, 60}
	state.CroppingSpecifications[0].BoundingBox = box
	state.Display.Classification[0] = ClassificationComponent{
		InputNumbers: []int{1}, RGBTransferFunction: RGBTransferTable, AlphaTransferFunction: AlphaTransferTable,
		PaletteDescriptor: [3]uint16{2, 0, 16},
		RedPalette:        []uint16{0, 65535}, GreenPalette: []uint16{0, 65535},
		BluePalette: []uint16{0, 65535}, AlphaPalette: []uint16{0, 65535},
	}
	file, err := Write(state)
	if err != nil {
		t.Fatal(err)
	}
	crops := derivedio.Sequence(file.Dataset, tagVolumeCroppingSequence)
	crops[0].SetFD(tagBoundingBoxCrop, box[:]...)
	file.Dataset.SetSequence(tagVolumeCroppingSequence, crops...)
	streams := derivedio.Sequence(file.Dataset, tagVolumeStreamSequence)
	components := derivedio.Sequence(streams[0], tagClassificationSequence)
	components[0].Remove(tagAlphaPaletteDescriptor)
	components[0].Put(wordPalette(tagAlphaPaletteData, []uint16{0, 65535}))
	streams[0].SetSequence(tagClassificationSequence, components...)
	file.Dataset.SetSequence(tagVolumeStreamSequence, streams...)
	if got, err := Read(file.Dataset); got != nil || !errors.Is(err, ErrInvalidObject) {
		t.Fatalf("default read accepted missing alpha descriptor: state=%#v error=%v", got, err)
	}
	options := ReadOptions{LegacyInterleavedBoundingBox: true, LegacyMissingAlphaDescriptor: true}
	migrated, err := ReadWithOptions(file.Dataset, options)
	if err != nil {
		t.Fatal(err)
	}
	if migrated.CroppingSpecifications[0].BoundingBox != box {
		t.Fatal("explicit legacy recovery lost the authored corners")
	}
	got := roundTripStandardState(t, migrated)
	if !reflect.DeepEqual(got.CroppingSpecifications, state.CroppingSpecifications) || !reflect.DeepEqual(got.Display, state.Display) {
		t.Fatal("canonical rewrite did not preserve migrated crop and palette")
	}
	canonical, err := Write(migrated)
	if err != nil {
		t.Fatal(err)
	}
	canonicalCrop := derivedio.Floats(derivedio.Sequence(canonical.Dataset, tagVolumeCroppingSequence)[0], tagBoundingBoxCrop)
	if !reflect.DeepEqual(canonicalCrop, []float64{-100, -80, -60, 100, 80, 60}) {
		t.Fatalf("rewrite emitted nonstandard corners: %v", canonicalCrop)
	}
	components[0].SetUS(tagAlphaPaletteDescriptor, 2, 1, 16)
	streams[0].SetSequence(tagClassificationSequence, components...)
	file.Dataset.SetSequence(tagVolumeStreamSequence, streams...)
	if got, err := ReadWithOptions(file.Dataset, options); got != nil || !errors.Is(err, ErrInvalidObject) {
		t.Fatal("legacy recovery accepted a present incompatible alpha descriptor")
	}
}

func TestStandardVPSPreservesExplicitVOIFunction(t *testing.T) {
	for _, function := range []display.VOIFunction{display.VOILinear, display.VOILinearExact, display.VOISigmoid} {
		state := standardVRFixtureState()
		state.Inputs[0].VOI.Function = function
		got := roundTripStandardState(t, state)
		if got.Inputs[0].VOI.Function != function {
			t.Fatalf("function %v became %v", function, got.Inputs[0].VOI.Function)
		}
	}
	state := standardVRFixtureState()
	state.Inputs[0].VOI.Function = display.VOIFunction(19)
	if file, err := Write(state); file != nil || !errors.Is(err, ErrUnsupportedPayload) {
		t.Fatalf("unknown VOI function accepted: %v %v", file, err)
	}
}

func TestStandardVPSTableAlphaUsesEightBitWireAndExplicitLegacyRecovery(t *testing.T) {
	state := standardVRFixtureState()
	state.Display.Classification[0] = ClassificationComponent{
		InputNumbers: []int{1}, RGBTransferFunction: RGBTransferTable, AlphaTransferFunction: AlphaTransferTable,
		PaletteDescriptor: [3]uint16{3, 0, 16}, RedPalette: []uint16{0, 32768, 65535}, GreenPalette: []uint16{0, 32768, 65535}, BluePalette: []uint16{0, 32768, 65535},
		AlphaPalette: []uint16{0, 32768, 65535},
	}
	file, err := Write(state)
	if err != nil {
		t.Fatal(err)
	}
	streams := derivedio.Sequence(file.Dataset, tagVolumeStreamSequence)
	components := derivedio.Sequence(streams[0], tagClassificationSequence)
	data, ok := components[0].Get(tagAlphaPaletteData)
	if !ok {
		t.Fatal("alpha data missing")
	}
	if words, ok := data.Value.(core.Uint16Value); !ok || !reflect.DeepEqual(words, core.Uint16Value{0x8000, 0x00ff}) {
		t.Fatalf("alpha wire words=%v", data.Value)
	}
	got := roundTripStandardState(t, state)
	if want := []uint16{0, 32896, 65535}; !reflect.DeepEqual(got.Display.Classification[0].AlphaPalette, want) {
		t.Fatalf("canonical alpha values=%v, want %v", got.Display.Classification[0].AlphaPalette, want)
	}
	components[0].SetUS(tagAlphaPaletteDescriptor, 3, 0, 16)
	components[0].Put(wordPalette(tagAlphaPaletteData, []uint16{0, 32768, 65535}))
	streams[0].SetSequence(tagClassificationSequence, components...)
	file.Dataset.SetSequence(tagVolumeStreamSequence, streams...)
	if _, err := Read(file.Dataset); !errors.Is(err, ErrInvalidObject) {
		t.Fatal("default reader accepted nonstandard sixteen-bit alpha")
	}
	legacy, err := ReadWithOptions(file.Dataset, ReadOptions{LegacySixteenBitAlpha: true})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(legacy.Display.Classification[0].AlphaPalette, state.Display.Classification[0].AlphaPalette) {
		t.Fatal("explicit legacy alpha recovery changed source values")
	}
}

func TestStandardVPSAlphaPaletteByteOrder(t *testing.T) {
	element := byteAlphaPalette([]uint16{0, 32896, 65535})
	for _, ts := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ExplicitVRBigEndian} {
		var encoded bytes.Buffer
		if err := parser.NewWriter(&encoded, ts).WriteElement(element); err != nil {
			t.Fatal(err)
		}
		raw := encoded.Bytes()
		want := []byte{0, 128, 255, 0}
		if ts == transfer.ExplicitVRBigEndian {
			want = []byte{128, 0, 0, 255}
		}
		if !bytes.Equal(raw[len(raw)-4:], want) {
			t.Fatalf("alpha bytes %v, want %v", raw[len(raw)-4:], want)
		}
	}
}

func TestStandardVPSAlphaPaletteReadBigEndian(t *testing.T) {
	words := byteAlphaPalette([]uint16{0, 32896, 65535})
	obj := object.FromElements([]core.Element{
		{Header: core.ElementHeader{Tag: tagAlphaPaletteDescriptor, VR: core.VRUS}, Value: core.RawValue{0, 3, 0, 0, 0, 8}},
		{Header: words.Header, Value: core.RawValue{128, 0, 0, 255}},
	}, std.Dictionary)
	obj.SetValueByteOrder(binary.BigEndian)
	got, err := readClassificationAlpha(obj, [3]uint16{3, 0, 16}, ReadOptions{})
	if err != nil || !reflect.DeepEqual(got, []uint16{0, 32896, 65535}) {
		t.Fatalf("big endian alpha %v %v", got, err)
	}
	obj.Put(core.Element{Header: words.Header, Value: core.RawValue{128, 0, 9, 255}})
	if _, err := readClassificationAlpha(obj, [3]uint16{3, 0, 16}, ReadOptions{}); !errors.Is(err, ErrInvalidObject) {
		t.Fatal("nonzero odd palette padding accepted")
	}
}
