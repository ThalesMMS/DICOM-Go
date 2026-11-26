package display_test

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
	"github.com/ThalesMMS/dicom-go/transfer"
)

const overlayOracleRows, overlayOracleColumns = 32, 48

// overlayOraclePlane is a 1-bit overlay plane with its origin.
type overlayOraclePlane struct {
	group                uint16
	rows, columns        int
	originRow, originCol int
	set                  func(row, col int) bool
}

func (p overlayOraclePlane) data() []byte {
	out := make([]byte, (p.rows*p.columns+15)/16*2)
	for i := 0; i < p.rows*p.columns; i++ {
		if p.set(i/p.columns, i%p.columns) {
			out[i/8] |= 1 << (i % 8)
		}
	}
	return out
}

func overlayOracleObject(pixels []byte, planes []overlayOraclePlane) *object.Object {
	str := func(g, e uint16, vr core.VR, value string) core.Element {
		if len(value)%2 != 0 {
			pad := " "
			if vr == core.VRUI {
				pad = "\x00"
			}
			value += pad
		}
		return core.NewRawElement(core.NewTag(g, e), vr, []byte(value))
	}
	us := func(g, e uint16, values ...uint16) core.Element {
		raw := make([]byte, 2*len(values))
		for i, v := range values {
			binary.LittleEndian.PutUint16(raw[2*i:], v)
		}
		return core.NewRawElement(core.NewTag(g, e), core.VRUS, raw)
	}
	elements := []core.Element{
		str(0x0008, 0x0016, core.VRUI, "1.2.840.10008.5.1.4.1.1.7"),
		str(0x0008, 0x0018, core.VRUI, "1.2.826.0.1.3680043.10.1.953.4"),
		us(0x0028, 0x0002, 1),
		str(0x0028, 0x0004, core.VRCS, "MONOCHROME2"),
		us(0x0028, 0x0010, overlayOracleRows),
		us(0x0028, 0x0011, overlayOracleColumns),
		us(0x0028, 0x0100, 8),
		us(0x0028, 0x0101, 8),
		us(0x0028, 0x0102, 7),
		us(0x0028, 0x0103, 0),
		str(0x0028, 0x1050, core.VRDS, "100"),
		str(0x0028, 0x1051, core.VRDS, "200"),
		core.NewRawElement(core.TagPixelData, core.VROB, pixels),
	}
	for _, p := range planes {
		origin := make([]byte, 4)
		binary.LittleEndian.PutUint16(origin, uint16(int16(p.originRow)))
		binary.LittleEndian.PutUint16(origin[2:], uint16(int16(p.originCol)))
		elements = append(elements,
			us(p.group, 0x0010, uint16(p.rows)),
			us(p.group, 0x0011, uint16(p.columns)),
			str(p.group, 0x0040, core.VRCS, "G"),
			core.NewRawElement(core.NewTag(p.group, 0x0050), core.VRSS, origin),
			us(p.group, 0x0100, 1),
			us(p.group, 0x0102, 0),
			core.NewRawElement(core.NewTag(p.group, 0x3000), core.VROW, p.data()),
		)
	}
	return object.FromElements(elements, nil)
}

// readOverlayOraclePGM reads a binary 8-bit PGM (P5).
func readOverlayOraclePGM(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(bytes.NewReader(data))
	var magic string
	var width, height, maxValue int
	if _, err := fmt.Fscan(r, &magic, &width, &height, &maxValue); err != nil || magic != "P5" || maxValue != 255 {
		t.Fatalf("PGM header %q %d %v", magic, maxValue, err)
	}
	if _, err := r.ReadByte(); err != nil {
		t.Fatal(err)
	}
	out := make([]byte, width*height)
	if _, err := io.ReadFull(r, out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestOverlaysAgainstDCM2PNM burns graphic overlay planes, one clipped by
// the image edge and one negative-origin plane, into a windowed frame and
// compares every pixel with DCMTK dcm2pnm, which shows all overlays in
// Replace mode at full foreground density by default. Enable with
// DICOMGO_DCMTK_INTEGRATION=1.
func TestOverlaysAgainstDCM2PNM(t *testing.T) {
	dcm2pnm := dcmtkoracle.Tool(t, "dcm2pnm")
	pixels := make([]byte, overlayOracleRows*overlayOracleColumns)
	for i := range pixels {
		pixels[i] = byte(i * 7 % 251)
	}
	planes := []overlayOraclePlane{
		{group: 0x6000, rows: 10, columns: 24, originRow: 5, originCol: 30, set: func(r, c int) bool { return (r+c)%3 == 0 }},
		{group: 0x6002, rows: 20, columns: 20, originRow: -4, originCol: -2, set: func(r, c int) bool { return r == c || r == 19-c }},
	}
	dataset := overlayOracleObject(pixels, planes)
	dir := t.TempDir()
	input, output := filepath.Join(dir, "overlays.dcm"), filepath.Join(dir, "overlays.pgm")
	var encoded bytes.Buffer
	if err := object.WriteFile(&encoded, &object.File{Dataset: dataset, TransferSyntax: transfer.ExplicitVRLittleEndian}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(input, encoded.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	const tolerance = "overlays exact on dcm2pnm's own windowed image; window within 1 level"
	dcmtkoracle.Run(t, tolerance, dcm2pnm, "+op", "+Wi", "1", input, output)
	want := readOverlayOraclePGM(t, output)
	// The same image without overlays: the overlays are burned onto it, so
	// the window's rounding (dcm2pnm truncates, RenderGray rounds) stays
	// out of the overlay comparison.
	plain := filepath.Join(dir, "plain.pgm")
	dcmtkoracle.Run(t, tolerance, dcm2pnm, "+op", "+Wi", "1", "-O", input, plain)
	base := readOverlayOraclePGM(t, plain)
	if len(want) != overlayOracleRows*overlayOracleColumns || len(base) != len(want) {
		t.Fatalf("dcm2pnm wrote %d and %d pixels", len(want), len(base))
	}

	windowed, err := display.RenderGray(display.Frame{Rows: overlayOracleRows, Columns: overlayOracleColumns, Pixels: pixels,
		Format: display.PixelFormat{BitsAllocated: 8, BitsStored: 8, HighBit: 7}, VOI: display.VOILUT{Center: 100, Width: 200}})
	if err != nil {
		t.Fatal(err)
	}
	for i := range base {
		if d := int(base[i]) - int(windowed.Pix[i]); d > 1 || d < -1 {
			t.Fatalf("windowed pixel %d is %d, dcm2pnm %d", i, windowed.Pix[i], base[i])
		}
	}
	img := image.NewGray(windowed.Rect)
	copy(img.Pix, base)
	overlays, err := display.OverlaysFromObject(dataset)
	if err != nil || len(overlays) != 2 {
		t.Fatalf("overlays %d: %v", len(overlays), err)
	}
	for _, overlay := range overlays {
		overlay.Composite(img, 255)
	}
	burned := 0
	for i := range want {
		if want[i] != img.Pix[i] {
			t.Fatalf("pixel (%d, %d) is %d, dcm2pnm %d", i%overlayOracleColumns, i/overlayOracleColumns, img.Pix[i], want[i])
		}
		if want[i] != base[i] {
			burned++
		}
	}
	if burned == 0 {
		t.Fatal("dcm2pnm burned no overlay pixel")
	}
}
