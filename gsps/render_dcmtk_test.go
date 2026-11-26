package gsps

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/commontest"
	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
)

const (
	syntheticRows    = 40
	syntheticColumns = 64
	syntheticStudy   = "1.2.826.0.1.3680043.10.1.1047.1"
	syntheticSeries  = "1.2.826.0.1.3680043.10.1.1060.2"
	syntheticImage   = "1.2.826.0.1.3680043.10.1.1060.3"
)

// syntheticPixels is a 12-bit ramp with a bright square, so rotations,
// flips and windows all show.
func syntheticPixels() []byte {
	out := make([]byte, syntheticRows*syntheticColumns*2)
	for row := 0; row < syntheticRows; row++ {
		for col := 0; col < syntheticColumns; col++ {
			value := (row*97 + col*41) % 4096
			if row >= 5 && row < 12 && col >= 8 && col < 20 {
				value = 4000
			}
			binary.LittleEndian.PutUint16(out[2*(row*syntheticColumns+col):], uint16(value))
		}
	}
	return out
}

// syntheticImageObject is a synthetic 12-bit CT image with a rescale.
func syntheticImageObject() *object.Object {
	source := commontest.Source()
	elements := []core.Element{
		derivedio.UI(derivedio.TagSOPClassUID, "1.2.840.10008.5.1.4.1.1.2"),
		derivedio.UI(derivedio.TagSOPInstanceUID, syntheticImage),
		derivedio.CS(derivedio.TagModality, "CT"),
		derivedio.UI(derivedio.TagStudyInstanceUID, syntheticStudy),
		derivedio.UI(derivedio.TagSeriesInstanceUID, syntheticSeries),
		derivedio.US(derivedio.TagSamplesPerPixel, 1),
		derivedio.CS(derivedio.TagPhotometricInterpretation, "MONOCHROME2"),
		derivedio.US(derivedio.TagRows, syntheticRows),
		derivedio.US(derivedio.TagColumns, syntheticColumns),
		derivedio.US(derivedio.TagBitsAllocated, 16),
		derivedio.US(derivedio.TagBitsStored, 12),
		derivedio.US(derivedio.TagHighBit, 11),
		derivedio.US(derivedio.TagPixelRepresentation, 0),
		derivedio.DS(tagPixelSpacing, 0.5, 0.5),
		derivedio.DS(tagRescaleIntercept, -1024),
		derivedio.DS(tagRescaleSlope, 1),
		derivedio.DS(tagWindowCenter, 400),
		derivedio.DS(tagWindowWidth, 1500),
		derivedio.Raw(derivedio.TagPixelData, core.VROW, syntheticPixels()),
	}
	for _, tag := range []core.Tag{core.NewTag(0x0008, 0x0005), core.NewTag(0x0010, 0x0010), core.NewTag(0x0010, 0x0020), core.NewTag(0x0008, 0x0020)} {
		if element, ok := source.Get(tag); ok {
			elements = append(elements, element)
		}
	}
	return derivedio.Object(elements...)
}

func syntheticFrame() display.Frame {
	return display.Frame{
		Rows: syntheticRows, Columns: syntheticColumns, Pixels: syntheticPixels(),
		Format: display.PixelFormat{BitsAllocated: 16, BitsStored: 12, HighBit: 11},
	}
}

// syntheticOverlay is a 10x30 frame of set bits at row 2, column 30.
func syntheticOverlay() display.Overlay {
	overlay := display.Overlay{Rows: 10, Columns: 30, OriginRow: 2, OriginCol: 30, Group: 0x6000, Type: "G", Data: make([]byte, 38)}
	for i := 0; i < 300; i++ {
		row, col := i/30, i%30
		if row == 0 || row == 9 || col == 0 || col == 29 || row == col {
			overlay.Data[i/8] |= 1 << (i % 8)
		}
	}
	return overlay
}

// renderCases are presentation states DCMTK dcmp2pgm renders too.
func renderCases(t *testing.T) map[string]*State {
	t.Helper()
	base := func() *State {
		state, err := FromImage(syntheticImageObject(), FromImageOptions{SOPInstanceUID: "1.2.826.0.1.3680043.10.1.1060.10", SeriesInstanceUID: "1.2.826.0.1.3680043.10.1.1060.11"})
		if err != nil {
			t.Fatal(err)
		}
		state.CreationDate, state.CreationTime = "20260926", "120000"
		return state
	}
	// transform sets the spatial transform and the displayed area corners
	// that keep the whole image displayed under it.
	transform := func(state *State, t SpatialTransform) {
		state.SpatialTransform = t
		area := &state.DisplayedArea
		area.TopLeftX, area.TopLeftY, area.BottomRightX, area.BottomRightY = t.Corners(1, 1, syntheticColumns, syntheticRows)
	}
	identity := base()
	identity.GraphicLayers = []GraphicLayer{{Name: "TEXT", Order: 1, Description: "Labels"}}
	identity.GraphicAnnotations = []GraphicAnnotation{
		{LayerName: "TEXT", Text: "Label", BoundingBox: &BoundingBox{TopLeft: Point2D{X: 2, Y: 2}, BottomRight: Point2D{X: 30, Y: 9}, Justification: JustifyCenter}, Anchor: Point2D{X: 10, Y: 20}, AnchorVisible: true},
		{LayerName: "SHAPES", GraphicType: GraphicTypeEllipse, Polyline: []Point2D{{5, 10}, {25, 10}, {15, 5}, {15, 15}}, Filled: true},
		{LayerName: "SHAPES", GraphicType: GraphicTypePoint, GraphicUnits: AnnotationUnitsDisplay, Polyline: []Point2D{{0.5, 0.5}}},
	}

	turned := base()
	transform(turned, SpatialTransform{RotationDegrees: 90, FlipHorizontal: true})
	turned.PresentationLUTShape = PresentationLUTInverse
	turned.ModalityLUT = &ModalityLUT{Slope: 2, Intercept: -2048, RescaleType: "US"}
	turned.SoftcopyVOI = SoftcopyVOI{WindowCenter: 1000, WindowWidth: 3000}
	turned.Shutter = &display.Shutter{Rectangle: &display.RectShutter{Left: 4, Right: 50, Upper: 3, Lower: 35}, PresentationValue: 40, PresentationPValue: 40 * 257, PresentationPValueDefined: true}
	turned.Overlays = []display.Overlay{syntheticOverlay()}
	turned.OverlayActivations = []OverlayActivation{{Group: 0x6000, Layer: "OVERLAY"}}

	upsideDown := base()
	transform(upsideDown, SpatialTransform{RotationDegrees: 180})
	entries := make([]uint16, 4096)
	for i := range entries {
		entries[i] = uint16(math.Round(4095 * math.Pow(float64(i)/4095, 0.5)))
	}
	upsideDown.PresentationLUT = &display.LUT{Entries: entries, BitsPerEntry: 12}
	upsideDown.PresentationLUTExplanation = "Square root"
	upsideDown.Shutter = &display.Shutter{Circle: &display.CircleShutter{CenterRow: 20, CenterCol: 30, Radius: 18}, PresentationValue: 255, PresentationPValue: 0xFFFF, PresentationPValueDefined: true}

	unwindowed := base()
	transform(unwindowed, SpatialTransform{RotationDegrees: 270})
	unwindowed.SoftcopyVOI = SoftcopyVOI{}
	unwindowed.Shutter = &display.Shutter{Polygon: &display.PolygonShutter{Vertices: []display.ShutterPoint{{Row: 2, Col: 2}, {Row: 2, Col: 60}, {Row: 38, Col: 30}}}}

	voiLUT := base()
	lutEntries := make([]uint16, 2048)
	for i := range lutEntries {
		lutEntries[i] = uint16(i * 32)
	}
	voiLUT.SoftcopyVOI = SoftcopyVOI{LUT: &display.LUT{Entries: lutEntries, FirstMapped: -1024, BitsPerEntry: 16}, Explanation: "Ramp"}
	transform(voiLUT, SpatialTransform{FlipHorizontal: true})
	return map[string]*State{"identity": identity, "turned": turned, "upside-down": upsideDown, "unwindowed": unwindowed, "voi-lut": voiLUT}
}

func writeDICOM(t *testing.T, path string, file *object.File) {
	t.Helper()
	var encoded bytes.Buffer
	if err := object.WriteFile(&encoded, file); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestRenderMatchesDCMP2PGM renders synthetic presentation states with
// Render and with DCMTK dcmp2pgm and compares the bitmaps (modality, VOI,
// Presentation LUT, rotation and flip); dcmpschk must accept every state,
// shutters and overlays included. Enable with DICOMGO_DCMTK_INTEGRATION=1 and the DCMTK
// tools on PATH.
func TestRenderMatchesDCMP2PGM(t *testing.T) {
	dcmpschk := dcmtkoracle.Tool(t, "dcmpschk")
	dcmp2pgm := dcmtkoracle.Tool(t, "dcmp2pgm")
	dir := t.TempDir()
	imagePath := filepath.Join(dir, "image.dcm")
	imageFile, err := derivedio.File("1.2.840.10008.5.1.4.1.1.2", syntheticImage, syntheticImageObject())
	if err != nil {
		t.Fatal(err)
	}
	writeDICOM(t, imagePath, imageFile)
	for name, state := range renderCases(t) {
		t.Run(name, func(t *testing.T) {
			file, err := Write(state)
			if err != nil {
				t.Fatal(err)
			}
			statePath := filepath.Join(dir, name+".dcm")
			writeDICOM(t, statePath, file)
			if out := dcmtkoracle.Run(t, dcmtkoracle.Accepted, dcmpschk, statePath); !strings.Contains(string(out), "Test passed") {
				t.Fatalf("dcmpschk:\n%s", out)
			}
			pgmPath := filepath.Join(dir, name+".pgm")
			dcmtkoracle.Run(t, "within 1 grey level per pixel (shutters and overlays not burned in)", dcmp2pgm, "-p", statePath, imagePath, pgmPath)
			want := readPGM(t, pgmPath)
			read, err := Read(file.Dataset)
			if err != nil {
				t.Fatal(err)
			}
			// dcmp2pgm burns neither display shutters nor graphic overlays
			// (DICOMscope draws them over the bitmap), so compare without.
			read.Shutter, read.Overlays, read.OverlayActivations = nil, nil, nil
			got, err := Render(syntheticFrame(), read, ImageState{SOPInstanceUID: syntheticImage, FrameNumber: 1}, RenderOptions{WholeImage: true, NoAnnotations: true})
			if err != nil {
				t.Fatal(err)
			}
			if got.Rect != want.Rect {
				t.Fatalf("size %v, dcmp2pgm %v", got.Rect, want.Rect)
			}
			worst, differing := 0, 0
			for i := range got.Pix {
				if d := abs(int(got.Pix[i]) - int(want.Pix[i])); d > 0 {
					differing++
					worst = max(worst, d)
				}
			}
			if dump := os.Getenv("DICOMGO_RENDER_DUMP"); dump != "" {
				writePGM(t, filepath.Join(dump, name+"-go.pgm"), got)
				writePGM(t, filepath.Join(dump, name+"-dcmtk.pgm"), want)
			}
			if worst > 1 {
				t.Fatalf("%d pixels differ, by up to %d\n%s", differing, worst, diffMap(got, want))
			}
		})
	}
}

func diffMap(got, want *image.Gray) string {
	var b strings.Builder
	for y := 0; y < got.Rect.Dy(); y++ {
		for x := 0; x < got.Rect.Dx(); x++ {
			d := int(got.Pix[y*got.Stride+x]) - int(want.Pix[y*want.Stride+x])
			switch {
			case d > 1:
				b.WriteByte('+')
			case d < -1:
				b.WriteByte('-')
			default:
				b.WriteByte('.')
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// readPGM reads a binary 8-bit PGM (P5).
func readPGM(t *testing.T, path string) *image.Gray {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(bytes.NewReader(data))
	var fields []int
	var magic string
	for len(fields) < 3 {
		var token string
		if _, err := fmt.Fscan(r, &token); err != nil {
			t.Fatalf("PGM header: %v", err)
		}
		if strings.HasPrefix(token, "#") {
			_, _ = r.ReadString('\n')
			continue
		}
		if magic == "" {
			magic = token
			continue
		}
		var value int
		if _, err := fmt.Sscan(token, &value); err != nil {
			t.Fatalf("PGM header value %q", token)
		}
		fields = append(fields, value)
	}
	if magic != "P5" || fields[2] != 255 {
		t.Fatalf("PGM %s maxval %d", magic, fields[2])
	}
	if _, err := r.ReadByte(); err != nil {
		t.Fatal(err)
	}
	out := image.NewGray(image.Rect(0, 0, fields[0], fields[1]))
	if _, err := io.ReadFull(r, out.Pix); err != nil {
		t.Fatalf("PGM pixels: %v", err)
	}
	return out
}

func writePGM(t *testing.T, path string, img *image.Gray) {
	t.Helper()
	header := fmt.Sprintf("P5\n%d %d\n255\n", img.Rect.Dx(), img.Rect.Dy())
	if err := os.WriteFile(path, append([]byte(header), img.Pix...), 0o600); err != nil {
		t.Fatal(err)
	}
}
