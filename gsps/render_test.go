package gsps

import (
	"errors"
	"image"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
)

// blackFrame is a 12-bit frame of zeros, black without a VOI transform.
func blackFrame(rows, columns int) display.Frame {
	return display.Frame{Rows: rows, Columns: columns, Pixels: make([]byte, rows*columns*2),
		Format: display.PixelFormat{BitsAllocated: 16, BitsStored: 12, HighBit: 11}}
}

func renderState(annotations ...GraphicAnnotation) *State {
	return &State{
		SOPInstanceUID: "1.2.3.9", StudyInstanceUID: "1.2.3.1", SeriesInstanceUID: "1.2.3.2",
		DisplayedArea:      DisplayedArea{Defined: true, TopLeftX: 1, TopLeftY: 1, BottomRightX: 40, BottomRightY: 20},
		GraphicAnnotations: annotations,
	}
}

func at(img *image.Gray, x, y int) uint8 { return img.Pix[y*img.Stride+x] }

func TestRenderPlacesPixelAnnotationsWithTheImageAndDisplayAnnotationsOnTheScreen(t *testing.T) {
	state := renderState(
		// A filled 4x3 rectangle at columns 2..5, rows 1..3 of the image.
		GraphicAnnotation{LayerName: "SHAPES", Polyline: []Point2D{{2, 1}, {6, 1}, {6, 4}, {2, 4}, {2, 1}}, Filled: true},
		// A point at the centre of the displayed area.
		GraphicAnnotation{LayerName: "MARKS", GraphicType: GraphicTypePoint, GraphicUnits: AnnotationUnitsDisplay, Polyline: []Point2D{{0.5, 0.5}}},
	)
	gray := uint16(0x8000)
	state.GraphicLayers = []GraphicLayer{{Name: "MARKS", Order: 2, RecommendedGrayscale: &gray}, {Name: "SHAPES", Order: 1}}
	state.SpatialTransform = SpatialTransform{RotationDegrees: 90}
	out, err := Render(blackFrame(20, 40), state, ImageState{}, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Rect.Dx() != 20 || out.Rect.Dy() != 40 {
		t.Fatalf("rotated size %v", out.Rect)
	}
	// Rotating 90 degrees clockwise maps image (x, y) to (20-y, x): the
	// rectangle's interior pixel (3, 2) lands on (17, 3).
	if at(out, 17, 3) != 255 || at(out, 17, 10) != 0 {
		t.Fatalf("filled rectangle: %d, %d", at(out, 17, 3), at(out, 17, 10))
	}
	if at(out, 10, 20) != 0x80 {
		t.Fatalf("DISPLAY point at the centre: %d", at(out, 10, 20))
	}
}

func TestRenderCropsToTheDisplayedAreaAndPadsWithTheShutterValue(t *testing.T) {
	state := renderState()
	state.DisplayedArea = DisplayedArea{Defined: true, TopLeftX: -4, TopLeftY: 11, BottomRightX: 10, BottomRightY: 20}
	state.Shutter = &display.Shutter{Rectangle: &display.RectShutter{Left: 1, Right: 40, Upper: 1, Lower: 20}, PresentationValue: 200}
	frame := blackFrame(20, 40)
	frame.Pixels[2*(10*40+0)] = 0xFF // image (0, 10), a bright pixel
	frame.Pixels[2*(10*40+0)+1] = 0x0F
	out, err := Render(frame, state, ImageState{}, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Rect.Dx() != 15 || out.Rect.Dy() != 10 {
		t.Fatalf("displayed area size %v", out.Rect)
	}
	// Output column 5 is image column 0 (TopLeftX -4 is 1-based).
	if at(out, 4, 0) != 200 || at(out, 6, 0) != 0 || at(out, 5, 0) != 255 {
		t.Fatalf("pad %d, image %d, bright %d", at(out, 4, 0), at(out, 6, 0), at(out, 5, 0))
	}
	whole, err := Render(frame, state, ImageState{}, RenderOptions{WholeImage: true})
	if err != nil || whole.Rect.Dx() != 40 || whole.Rect.Dy() != 20 {
		t.Fatalf("whole image %v: %v", whole.Rect, err)
	}
}

func TestSpatialTransformCornersFollowTheDisplayedImage(t *testing.T) {
	for _, tc := range []struct {
		transform SpatialTransform
		want      [4]int
	}{
		{SpatialTransform{}, [4]int{2, 3, 10, 5}},
		{SpatialTransform{FlipHorizontal: true}, [4]int{10, 3, 2, 5}},
		{SpatialTransform{RotationDegrees: 90}, [4]int{2, 5, 10, 3}},
		{SpatialTransform{RotationDegrees: 180}, [4]int{10, 5, 2, 3}},
		{SpatialTransform{RotationDegrees: 270}, [4]int{10, 3, 2, 5}},
		{SpatialTransform{RotationDegrees: 90, FlipHorizontal: true}, [4]int{2, 3, 10, 5}},
	} {
		x0, y0, x1, y1 := tc.transform.Corners(10, 5, 2, 3)
		if got := [4]int{x0, y0, x1, y1}; got != tc.want {
			t.Errorf("%+v: corners %v, want %v", tc.transform, got, tc.want)
		}
		area := DisplayedArea{TopLeftX: x0, TopLeftY: y0, BottomRightX: x1, BottomRightY: y1}
		if l, top, r, b := area.ImageRegion(); l != 2 || top != 3 || r != 10 || b != 5 {
			t.Errorf("%+v: region %d,%d..%d,%d", tc.transform, l, top, r, b)
		}
	}
	// A rotated state crops the region its corners select.
	state := renderState()
	state.SpatialTransform = SpatialTransform{RotationDegrees: 90}
	state.DisplayedArea.TopLeftX, state.DisplayedArea.TopLeftY, state.DisplayedArea.BottomRightX, state.DisplayedArea.BottomRightY = state.SpatialTransform.Corners(1, 1, 10, 4)
	frame := blackFrame(20, 40)
	frame.Pixels[2*(3*40+0)], frame.Pixels[2*(3*40+0)+1] = 0xFF, 0x0F // image (0, 3)
	out, err := Render(frame, state, ImageState{}, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Rect.Dx() != 4 || out.Rect.Dy() != 10 || at(out, 0, 0) != 255 {
		t.Fatalf("rotated crop %v, top left %d", out.Rect, at(out, 0, 0))
	}
}

func TestRenderShutterCoversActivatedOverlays(t *testing.T) {
	state := renderState()
	overlay := display.Overlay{Rows: 1, Columns: 8, OriginRow: 1, OriginCol: 1, Group: 0x6000, Data: []byte{0xFF}}
	state.OverlayActivations = []OverlayActivation{{Group: 0x6000, Layer: "OVERLAY"}}
	gray := uint16(0x6400)
	state.GraphicLayers = []GraphicLayer{{Name: "OVERLAY", RecommendedGrayscale: &gray}}
	state.Shutter = &display.Shutter{Rectangle: &display.RectShutter{Left: 5, Right: 40, Upper: 1, Lower: 20}, PresentationValue: 7}
	out, err := Render(blackFrame(20, 40), state, ImageState{}, RenderOptions{ImageOverlays: []display.Overlay{overlay}})
	if err != nil {
		t.Fatal(err)
	}
	if at(out, 3, 0) != 7 || at(out, 5, 0) != 0x64 || at(out, 9, 0) != 0 {
		t.Fatalf("shuttered %d, overlay %d, image %d", at(out, 3, 0), at(out, 5, 0), at(out, 9, 0))
	}
}

func TestRenderDrawsTextInItsBoundingBox(t *testing.T) {
	for _, justification := range []string{JustifyLeft, JustifyCenter, JustifyRight} {
		state := renderState(GraphicAnnotation{Text: "Hi", BoundingBox: &BoundingBox{
			TopLeft: Point2D{X: 0.1, Y: 0.2}, BottomRight: Point2D{X: 0.9, Y: 0.8}, Units: AnnotationUnitsDisplay, Justification: justification}})
		out, err := Render(blackFrame(20, 40), state, ImageState{}, RenderOptions{Font: BasicFont(1)})
		if err != nil {
			t.Fatal(err)
		}
		first, last := -1, -1
		for x := 0; x < 40; x++ {
			for y := 0; y < 20; y++ {
				if at(out, x, y) == 255 {
					if first < 0 {
						first = x
					}
					last = x
				}
			}
		}
		// "Hi" is 11 pixels wide; the box spans x 4 to 36.
		want := map[string]int{JustifyLeft: 4, JustifyCenter: 20 - 5, JustifyRight: 36 - 11}[justification]
		if first != want || last-first > 10 {
			t.Errorf("%s: text spans x %d..%d, want from %d", justification, first, last, want)
		}
	}
}

func TestRenderDrawsEveryGraphicType(t *testing.T) {
	state := renderState(
		GraphicAnnotation{GraphicType: GraphicTypeCircle, Polyline: []Point2D{{10, 10}, {14, 10}}, Filled: true},
		GraphicAnnotation{GraphicType: GraphicTypeEllipse, Polyline: []Point2D{{22, 10}, {38, 10}, {30, 7}, {30, 13}}},
		GraphicAnnotation{GraphicType: GraphicTypeInterpolated, Polyline: []Point2D{{1, 18}, {10, 15}, {20, 18}}},
		GraphicAnnotation{Text: "A", Anchor: Point2D{X: 1e30, Y: -1e30}, AnchorVisible: true},
	)
	out, err := Render(blackFrame(20, 40), state, ImageState{}, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []image.Point{{10, 10}, {12, 12}, {37, 9}, {30, 7}, {10, 15}} {
		if at(out, p.X, p.Y) != 255 {
			t.Errorf("pixel %v is %d", p, at(out, p.X, p.Y))
		}
	}
	if at(out, 30, 10) != 0 {
		t.Error("the ellipse is not filled")
	}
}

func TestRenderRejectsMismatchedOrUnreferencedImages(t *testing.T) {
	state := renderState()
	state.ReferencedImages = []ReferencedImage{{SeriesInstanceUID: "1.2", SOPClassUID: "1.2.3", SOPInstanceUID: "1.2.3.4"}}
	if _, err := Render(blackFrame(20, 40), state, ImageState{SOPInstanceUID: "9.9"}, RenderOptions{}); !errors.Is(err, ErrMissingReference) {
		t.Fatalf("unreferenced image: %v", err)
	}
	if _, err := Render(blackFrame(20, 40), state, ImageState{Rows: 10, Columns: 10}, RenderOptions{}); !errors.Is(err, ErrInvalidObject) {
		t.Fatalf("size mismatch: %v", err)
	}
	state.DisplayedArea = DisplayedArea{Defined: true, TopLeftX: 1, TopLeftY: 1, BottomRightX: 1 << 20, BottomRightY: 1 << 20}
	if _, err := Render(blackFrame(20, 40), state, ImageState{}, RenderOptions{}); !errors.Is(err, ErrInvalidObject) {
		t.Fatalf("huge displayed area: %v", err)
	}
}

func TestRenderValidatesPresentationLUT(t *testing.T) {
	for _, tc := range []struct {
		name    string
		lut     *display.LUT
		wantErr bool
	}{
		{name: "no table"},
		{name: "10 bits", lut: &display.LUT{Entries: []uint16{0, 1023}, BitsPerEntry: 10}},
		{name: "16 bits", lut: &display.LUT{Entries: []uint16{0, 65535}, BitsPerEntry: 16}},
		{name: "empty entries", lut: &display.LUT{BitsPerEntry: 12}, wantErr: true},
		{name: "too many entries", lut: &display.LUT{Entries: make([]uint16, 1<<16+1), BitsPerEntry: 12}, wantErr: true},
		{name: "nonzero first mapped", lut: &display.LUT{Entries: []uint16{0, 1023}, FirstMapped: 1, BitsPerEntry: 10}, wantErr: true},
		{name: "zero bits", lut: &display.LUT{Entries: []uint16{0, 1}}, wantErr: true},
		{name: "below 10 bits", lut: &display.LUT{Entries: []uint16{0, 1}, BitsPerEntry: 9}, wantErr: true},
		{name: "above 16 bits", lut: &display.LUT{Entries: []uint16{0, 1}, BitsPerEntry: 17}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := Render(blackFrame(1, 1), &State{PresentationLUT: tc.lut}, ImageState{}, RenderOptions{})
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidObject) || out != nil {
					t.Fatalf("Render = %v, %v; want nil, ErrInvalidObject", out, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if out.Rect != image.Rect(0, 0, 1, 1) || at(out, 0, 0) != 0 {
				t.Fatalf("rendered frame = %+v, want one black pixel", out)
			}
		})
	}
}

func TestBasicFontCoversPrintableASCII(t *testing.T) {
	font := BasicFont(2)
	for r := rune(0x21); r <= 0x7E; r++ {
		img := image.NewGray(image.Rect(0, 0, 12, 18))
		font.Draw(img, 0, 0, string(r), 255)
		set := 0
		for _, v := range img.Pix {
			if v != 0 {
				set++
			}
		}
		if set == 0 {
			t.Errorf("glyph %q is empty", r)
		}
	}
	if font.Width("ab") != 22 || font.LineHeight() != 18 || font.Width("") != 0 {
		t.Fatalf("metrics %d %d", font.Width("ab"), font.LineHeight())
	}
}

func TestGSPSRoundTripsIdentificationLayersTextBoxesAndLUTs(t *testing.T) {
	gray := uint16(0xFFFF)
	lab := [3]uint16{0xFFFF, 0x8080, 0x8080}
	plut := make([]uint16, 256)
	for i := range plut {
		plut[i] = uint16(i * 4)
	}
	state := renderState(
		GraphicAnnotation{LayerName: "TEXT", Text: "Lesion 1", BoundingBox: &BoundingBox{TopLeft: Point2D{X: 2, Y: 3}, BottomRight: Point2D{X: 30, Y: 9}, Units: AnnotationUnitsPixel, Justification: JustifyCenter}, Anchor: Point2D{X: 10, Y: 12}, AnchorVisible: true},
		GraphicAnnotation{LayerName: "TEXT", Text: "Boxed only", BoundingBox: &BoundingBox{TopLeft: Point2D{X: 0.125, Y: 0.125}, BottomRight: Point2D{X: 0.5, Y: 0.25}, Units: AnnotationUnitsDisplay, Justification: JustifyRight}},
		GraphicAnnotation{LayerName: "SHAPES", GraphicType: GraphicTypeCircle, Polyline: []Point2D{{10, 10}, {12, 10}}, Filled: true},
	)
	state.SeriesNumber, state.InstanceNumber, state.Laterality = 3, 7, "L"
	state.ContentLabel, state.ContentDescription, state.ContentCreatorName = "MEASURED", "Synthetic", "Doe^Jane"
	state.CreationDate, state.CreationTime = "20260926", "101500"
	state.GraphicLayers = []GraphicLayer{{Name: "TEXT", Order: 5, RecommendedGrayscale: &gray, RecommendedCIELab: &lab, Description: "Labels"}}
	state.PresentationLUT = &display.LUT{Entries: plut, BitsPerEntry: 10}
	state.PresentationLUTExplanation = "Ramp"
	state.ModalityLUT = &ModalityLUT{Slope: 2, Intercept: -100, RescaleType: "HU"}
	file, err := Write(state)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := file.Dataset.Get(tagPresentationLUTShape); ok {
		t.Fatal("a Presentation LUT Sequence and Shape were both written")
	}
	read, err := Read(file.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	if read.SeriesNumber != 3 || read.InstanceNumber != 7 || read.Laterality != "L" || read.ContentLabel != "MEASURED" ||
		read.ContentDescription != "Synthetic" || read.ContentCreatorName != "Doe^Jane" || read.CreationDate != "20260926" || read.CreationTime != "101500" {
		t.Fatalf("identification: %+v", read)
	}
	wantLayers := []GraphicLayer{state.GraphicLayers[0], {Name: "SHAPES", Order: 6}}
	if !reflect.DeepEqual(read.GraphicLayers, wantLayers) {
		t.Fatalf("layers %+v", read.GraphicLayers)
	}
	for i, want := range state.GraphicAnnotations {
		got := read.GraphicAnnotations[i]
		if got.Text != want.Text || !reflect.DeepEqual(got.BoundingBox, want.BoundingBox) || got.Anchor != want.Anchor || got.AnchorVisible != want.AnchorVisible || got.Filled != want.Filled {
			t.Fatalf("annotation %d: %+v", i, got)
		}
	}
	if !reflect.DeepEqual(read.PresentationLUT.Entries, plut) || read.PresentationLUTExplanation != "Ramp" {
		t.Fatalf("Presentation LUT %+v", read.PresentationLUT)
	}
	if m := read.ModalityLUT; m == nil || m.Slope != 2 || m.Intercept != -100 || m.RescaleType != "HU" {
		t.Fatalf("Modality LUT %+v", read.ModalityLUT)
	}
	applied, err := Apply(read, ImageState{Rows: 20, Columns: 40})
	if err != nil {
		t.Fatal(err)
	}
	if box := applied.GraphicAnnotations[1].BoundingBox; box.Units != AnnotationUnitsPixel || box.TopLeft != (Point2D{X: 5, Y: 2.5}) {
		t.Fatalf("applied bounding box %+v", box)
	}
	if applied.ModalityLUT == nil || applied.ModalityLUT.Slope != 2 || applied.PresentationLUTTable == nil || len(applied.GraphicLayers) != 2 {
		t.Fatalf("applied %+v", applied)
	}

	lutState := renderState()
	entries := []uint16{0, 10, 20, 30}
	lutState.ModalityLUT = &ModalityLUT{LUT: &display.LUT{Entries: entries, BitsPerEntry: 16}, LUTType: "OD", Explanation: "Steps"}
	file, err = Write(lutState)
	if err != nil {
		t.Fatal(err)
	}
	read, err = Read(file.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	if m := read.ModalityLUT; m == nil || m.LUT == nil || !reflect.DeepEqual(m.LUT.Entries, entries) || m.LUTType != "OD" || m.Explanation != "Steps" {
		t.Fatalf("Modality LUT table %+v", read.ModalityLUT)
	}
}

func TestGSPSWriteRejectsInvalidModelValues(t *testing.T) {
	for name, mutate := range map[string]func(*State){
		"layer name":       func(s *State) { s.GraphicAnnotations[0].LayerName = "lower case" },
		"long layer name":  func(s *State) { s.GraphicLayers = []GraphicLayer{{Name: "SEVENTEEN_LETTERS"}} },
		"duplicate layer":  func(s *State) { s.GraphicLayers = []GraphicLayer{{Name: "A"}, {Name: "A"}} },
		"long text":        func(s *State) { s.GraphicAnnotations[0].Text = strings.Repeat("x", 1025) },
		"justification":    func(s *State) { s.GraphicAnnotations[0].BoundingBox = &BoundingBox{Justification: "MIDDLE"} },
		"box without text": func(s *State) { s.GraphicAnnotations[0].Text, s.GraphicAnnotations[0].BoundingBox = "", &BoundingBox{} },
		"filled open line": func(s *State) {
			s.GraphicAnnotations[0].Polyline, s.GraphicAnnotations[0].Filled = []Point2D{{1, 1}, {2, 2}}, true
		},
		"rotation":      func(s *State) { s.SpatialTransform.RotationDegrees = 45 },
		"content label": func(s *State) { s.ContentLabel = "not a code" },
		"laterality":    func(s *State) { s.Laterality = "B" },
		"plut first mapped": func(s *State) {
			s.PresentationLUT = &display.LUT{Entries: []uint16{1, 2}, FirstMapped: 1, BitsPerEntry: 12}
		},
		"plut bits":          func(s *State) { s.PresentationLUT = &display.LUT{Entries: []uint16{1, 2}, BitsPerEntry: 8} },
		"zero rescale slope": func(s *State) { s.ModalityLUT = &ModalityLUT{} },
	} {
		state := renderState(GraphicAnnotation{Text: "x"})
		mutate(state)
		if _, err := Write(state); !errors.Is(err, ErrInvalidObject) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestFromImageBuildsTheDefaultPresentationState(t *testing.T) {
	image := syntheticImageObject()
	state, err := FromImage(image, FromImageOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(state.SOPInstanceUID, "2.25.") || !strings.HasPrefix(state.SeriesInstanceUID, "2.25.") || state.StudyInstanceUID != syntheticStudy {
		t.Fatalf("UIDs %s %s %s", state.SOPInstanceUID, state.SeriesInstanceUID, state.StudyInstanceUID)
	}
	want := DisplayedArea{Defined: true, TopLeftX: 1, TopLeftY: 1, BottomRightX: syntheticColumns, BottomRightY: syntheticRows, PresentationSizeMode: PresentationSizeScaleToFit, PixelSpacing: []float64{0.5, 0.5}}
	if !reflect.DeepEqual(state.DisplayedArea, want) || state.PresentationLUTShape != PresentationLUTIdentity {
		t.Fatalf("displayed area %+v shape %s", state.DisplayedArea, state.PresentationLUTShape)
	}
	if state.ModalityLUT == nil || state.ModalityLUT.Intercept != -1024 || state.SoftcopyVOI.WindowCenter != 400 || state.SoftcopyVOI.WindowWidth != 1500 {
		t.Fatalf("modality %+v VOI %+v", state.ModalityLUT, state.SoftcopyVOI)
	}
	if _, err := Write(state); err != nil {
		t.Fatal(err)
	}
	ignored, err := FromImage(image, FromImageOptions{VOI: VOIIgnore})
	if err != nil || softcopyVOIDefined(ignored.SoftcopyVOI) {
		t.Fatalf("VOI ignored: %+v %v", ignored.SoftcopyVOI, err)
	}

	inverted := syntheticImageObject()
	inverted.Put(derivedio.CS(derivedio.TagPhotometricInterpretation, "MONOCHROME1"))
	state, err = FromImage(inverted, FromImageOptions{})
	if err != nil || state.PresentationLUTShape != PresentationLUTInverse {
		t.Fatalf("MONOCHROME1: %v %v", state, err)
	}
	if _, err := FromImage(derivedio.Object(), FromImageOptions{}); !errors.Is(err, ErrMissingReference) {
		t.Fatalf("image without UIDs: %v", err)
	}
}

// TestFromImageMatchesDCMPSMK compares FromImage with the presentation state
// DCMTK dcmpsmk makes of the same image. Enable with
// DICOMGO_DCMTK_INTEGRATION=1 and dcmpsmk on PATH.
func TestFromImageMatchesDCMPSMK(t *testing.T) {
	dcmpsmk := dcmtkoracle.Tool(t, "dcmpsmk")
	dir := t.TempDir()
	imagePath, statePath := filepath.Join(dir, "image.dcm"), filepath.Join(dir, "ps.dcm")
	imageFile, err := derivedio.File("1.2.840.10008.5.1.4.1.1.2", syntheticImage, syntheticImageObject())
	if err != nil {
		t.Fatal(err)
	}
	writeDICOM(t, imagePath, imageFile)
	dcmtkoracle.Run(t, dcmtkoracle.Exact, dcmpsmk, "+Vw", imagePath, statePath)
	encoded, err := os.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer encoded.Close()
	dcmtkFile, err := object.ReadFile(encoded)
	if err != nil {
		t.Fatal(err)
	}
	want, err := Read(dcmtkFile.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	got, err := FromImage(syntheticImageObject(), FromImageOptions{VOI: VOIPreferWindow})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.ReferencedImages, want.ReferencedImages) || got.PresentationLUTShape != want.PresentationLUTShape ||
		!reflect.DeepEqual(got.ModalityLUT, want.ModalityLUT) || got.SoftcopyVOI.WindowCenter != want.SoftcopyVOI.WindowCenter ||
		got.SoftcopyVOI.WindowWidth != want.SoftcopyVOI.WindowWidth || got.SeriesNumber != want.SeriesNumber {
		t.Fatalf("FromImage %+v\ndcmpsmk %+v", got, want)
	}
	area, dcmtkArea := got.DisplayedArea, want.DisplayedArea
	area.ReferencedImages, dcmtkArea.ReferencedImages = nil, nil
	if !reflect.DeepEqual(area, dcmtkArea) {
		t.Fatalf("displayed area %+v, dcmpsmk %+v", area, dcmtkArea)
	}
}

// TestGeneratedStatesPassDCIODVFY checks the generated presentation states
// with dciodvfy, which also reports a missing Laterality it cannot know is
// not needed (as for dcmpsmk's states). Enable with DICOMGO_DCIODVFY set to
// the dciodvfy executable.
func TestGeneratedStatesPassDCIODVFY(t *testing.T) {
	tool := os.Getenv("DICOMGO_DCIODVFY")
	if tool == "" {
		t.Skip("opt-in: DICOMGO_DCIODVFY=/path/to/dciodvfy")
	}
	dir := t.TempDir()
	for name, state := range renderCases(t) {
		state.GraphicAnnotations = []GraphicAnnotation{
			{LayerName: "TEXT", Text: "Label", BoundingBox: &BoundingBox{TopLeft: Point2D{X: 2, Y: 2}, BottomRight: Point2D{X: 30, Y: 9}}, Anchor: Point2D{X: 10, Y: 20}, AnchorVisible: true},
			{LayerName: "SHAPES", GraphicType: GraphicTypeEllipse, Polyline: []Point2D{{5, 10}, {25, 10}, {15, 5}, {15, 15}}, Filled: true},
		}
		file, err := Write(state)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, name+".dcm")
		writeDICOM(t, path, file)
		out, _ := exec.Command(tool, "-new", path).CombinedOutput()
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, "Error") && !strings.Contains(line, "Laterality") {
				t.Errorf("%s: %s", name, line)
			}
		}
	}
}

// TestApplyKeepsDisplayAnnotationsInPlaceUnderRotation checks that Apply's
// pixel coordinates for a DISPLAY point land, once the image is rotated
// and flipped, where Render draws the DISPLAY point.
func TestApplyKeepsDisplayAnnotationsInPlaceUnderRotation(t *testing.T) {
	for _, transform := range []SpatialTransform{{}, {RotationDegrees: 90}, {RotationDegrees: 180, FlipHorizontal: true}, {RotationDegrees: 270}} {
		display := renderState(GraphicAnnotation{GraphicType: GraphicTypePoint, GraphicUnits: AnnotationUnitsDisplay, Polyline: []Point2D{{0.25, 0.75}}})
		display.SpatialTransform = transform
		area := &display.DisplayedArea
		area.TopLeftX, area.TopLeftY, area.BottomRightX, area.BottomRightY = transform.Corners(1, 1, 40, 20)
		applied, err := Apply(display, ImageState{Rows: 20, Columns: 40})
		if err != nil {
			t.Fatal(err)
		}
		pixel := *display
		pixel.GraphicAnnotations = applied.GraphicAnnotations
		want, err := Render(blackFrame(20, 40), display, ImageState{}, RenderOptions{})
		if err != nil {
			t.Fatal(err)
		}
		got, err := Render(blackFrame(20, 40), &pixel, ImageState{}, RenderOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.Pix, want.Pix) {
			t.Errorf("%+v: the applied point %v is drawn elsewhere", transform, applied.GraphicAnnotations[0].Polyline)
		}
	}
}
