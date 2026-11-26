package gsps

import (
	"bytes"
	"image"
	"image/color"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
)

// DCMTK dcmpstat_graphic_layer_read_write (dcmpstat/tests/tgl.cc) checks that
// the modern CIELab element wins when both color representations are present.
func TestReferenceGraphicLayerCIELabTakesPriorityOnWire(t *testing.T) {
	const layerName = "FINDINGS"
	lab := [3]uint16{0xffff, 0x8080, 0x8080}
	gray := uint16(0x3400)
	state := renderState(GraphicAnnotation{
		LayerName: layerName, GraphicType: GraphicTypePoint, GraphicUnits: AnnotationUnitsPixel,
		Polyline: []Point2D{{X: 5, Y: 6}},
	})
	file, err := Write(state)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	retiredRGB := core.NewTag(0x0070, 0x0067)
	file.Dataset.Put(derivedio.Seq(tagGraphicLayerSequence, derivedio.DataSet(
		derivedio.CS(tagGraphicLayer, layerName),
		derivedio.IS(tagGraphicLayerOrder, 1),
		derivedio.US(retiredRGB, 0xffff, 0, 0),
		derivedio.US(tagGraphicLayerRecommendedGrayscale, gray),
		derivedio.US(tagGraphicLayerRecommendedCIELab, lab[:]...),
	)))

	var wire bytes.Buffer
	if err := object.WriteFile(&wire, file); err != nil {
		t.Fatal(err)
	}
	parsed, err := object.ReadFile(&wire)
	if err != nil {
		t.Fatal(err)
	}
	defer parsed.Close()
	received, err := Read(parsed.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	wantLayers := []GraphicLayer{{Name: layerName, Order: 1, RecommendedCIELab: &lab, RecommendedGrayscale: &gray}}
	if !reflect.DeepEqual(received.GraphicLayers, wantLayers) {
		t.Fatalf("read graphic layers = %+v, want modern CIELab and grayscale recommendations", received.GraphicLayers)
	}

	target := ImageState{Rows: 20, Columns: 40}
	overlay, err := RenderAnnotationOverlay(received, target, image.Rect(0, 0, 40, 20), samePoint)
	if err != nil {
		t.Fatal(err)
	}
	if got := overlay.NRGBAAt(5, 6); got != (color.NRGBA{R: 255, G: 255, B: 255, A: 255}) {
		t.Fatalf("color overlay used the wrong recommendation: %v", got)
	}
	grayscale, err := Render(blackFrame(20, 40), received, target, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := grayscale.GrayAt(5, 6).Y; got != 0x34 {
		t.Fatalf("grayscale renderer used P-value %#02x, want %#02x", got, 0x34)
	}

	adapted, err := Write(received)
	if err != nil {
		t.Fatal(err)
	}
	defer adapted.Close()
	items := derivedio.Sequence(adapted.Dataset, tagGraphicLayerSequence)
	if len(items) != 1 || items[0].Has(retiredRGB) {
		t.Fatalf("rewritten Graphic Layer Sequence = %+v, want one modern item", items)
	}
	element, ok := items[0].Get(tagGraphicLayerRecommendedCIELab)
	raw, _ := element.RawBytes()
	if !ok || element.VR() != core.VRUS || !bytes.Equal(raw, []byte{0xff, 0xff, 0x80, 0x80, 0x80, 0x80}) {
		t.Fatalf("rewritten CIELab VR=%v, bytes=%x, want US and %#04x", element.VR(), raw, lab)
	}
}
