package iconimage

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image/color"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func us(tag core.Tag, value int) core.Element {
	raw := make([]byte, 2)
	binary.LittleEndian.PutUint16(raw, uint16(value))
	return core.NewRawElement(tag, core.VRUS, raw)
}

func text(tag core.Tag, vr core.VR, value string) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: vr}, Value: core.StringValue{value}}
}

// iconItem is an 8-bit MONOCHROME2 icon whose left half is black and right
// half white; pixels overrides the Pixel Data.
func iconItem(columns, rows int, pixels []byte) core.DataSet {
	if pixels == nil {
		pixels = make([]byte, columns*rows)
		for y := range rows {
			for x := columns / 2; x < columns; x++ {
				pixels[y*columns+x] = 255
			}
		}
	}
	return core.DataSet{Elements: []core.Element{
		us(core.NewTag(0x0028, 0x0002), 1),
		text(core.NewTag(0x0028, 0x0004), core.VRCS, "MONOCHROME2"),
		us(core.NewTag(0x0028, 0x0010), rows),
		us(core.NewTag(0x0028, 0x0011), columns),
		us(core.NewTag(0x0028, 0x0100), 8),
		us(core.NewTag(0x0028, 0x0101), 8),
		us(core.NewTag(0x0028, 0x0102), 7),
		us(core.NewTag(0x0028, 0x0103), 0),
		core.NewRawElement(core.TagPixelData, core.VROB, pixels),
	}}
}

func sequence(tag core.Tag, items ...core.DataSet) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: core.VRSQ}, Value: core.SequenceValue{Items: items}}
}

// roundTrip writes the elements as an Explicit VR Little Endian Part 10 file
// and reads it back, so private sequences are parsed as a reader sees them.
func roundTrip(t *testing.T, elements ...core.Element) *object.Object {
	t.Helper()
	base := []core.Element{
		text(core.NewTag(0x0008, 0x0016), core.VRUI, "1.2.840.10008.5.1.4.1.1.7"),
		text(core.NewTag(0x0008, 0x0018), core.VRUI, "2.25.1209001"),
	}
	dataset := object.FromElements(append(base, elements...), std.Dictionary)
	var encoded bytes.Buffer
	if err := object.WriteFile(&encoded, &object.File{Dataset: dataset, TransferSyntax: transfer.ExplicitVRLittleEndian}); err != nil {
		t.Fatal(err)
	}
	file, err := object.ReadFile(bytes.NewReader(encoded.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	return file.Dataset
}

func assertHalfBlackHalfWhite(t *testing.T, icon Icon, columns, rows int) {
	t.Helper()
	bounds := icon.Image.Bounds()
	if bounds.Dx() != columns || bounds.Dy() != rows {
		t.Fatalf("icon is %v, want %dx%d", bounds, columns, rows)
	}
	left := color.GrayModel.Convert(icon.Image.At(bounds.Min.X, bounds.Min.Y)).(color.Gray).Y
	right := color.GrayModel.Convert(icon.Image.At(bounds.Max.X-1, bounds.Max.Y-1)).(color.Gray).Y
	if left > 5 || right < 250 {
		t.Fatalf("icon luminance left %d right %d, want black and white", left, right)
	}
}

func TestReadIconImageSequence(t *testing.T) {
	dataset := roundTrip(t, sequence(tagIconImageSequence, iconItem(64, 48, nil)))
	icon, err := Read(dataset)
	if err != nil {
		t.Fatal(err)
	}
	if icon.Source != SourceIconImageSequence {
		t.Fatalf("source = %q", icon.Source)
	}
	assertHalfBlackHalfWhite(t, icon, 64, 48)
}

// The GE private icons are found through their private creators, in
// whichever block the creator reserved.
func TestReadGEPrivateIcons(t *testing.T) {
	for _, tt := range []struct {
		name    string
		group   uint16
		block   uint16
		creator string
		want    Source
	}{
		{name: "GEIIS", group: 0x0009, block: 0x10, creator: "GEIIS", want: SourceGEIIS},
		{name: "GE ultrasound", group: 0x6003, block: 0x11, creator: "GEMS_Ultrasound_ImageGroup_001", want: SourceGEUltrasound},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dataset := roundTrip(t,
				text(core.NewTag(tt.group, tt.block), core.VRLO, tt.creator),
				sequence(core.NewTag(tt.group, tt.block<<8|0x10), iconItem(32, 32, nil)),
			)
			icon, err := Read(dataset)
			if err != nil {
				t.Fatal(err)
			}
			if icon.Source != tt.want {
				t.Fatalf("source = %q, want %q", icon.Source, tt.want)
			}
			assertHalfBlackHalfWhite(t, icon, 32, 32)
		})
	}
}

// A truncated (or compressed) standard icon falls through to a private one;
// without any renderable icon Read reports ErrNoIcon.
func TestReadSkipsIconsThatDoNotRender(t *testing.T) {
	dataset := roundTrip(t,
		text(core.NewTag(0x0009, 0x0010), core.VRLO, "GEIIS"),
		sequence(core.NewTag(0x0009, 0x1010), iconItem(16, 16, nil)),
		sequence(tagIconImageSequence, iconItem(64, 64, make([]byte, 100))),
	)
	icon, err := Read(dataset)
	if err != nil || icon.Source != SourceGEIIS {
		t.Fatalf("Read = %+v, %v; want the GEIIS icon", icon.Source, err)
	}

	broken := roundTrip(t, sequence(tagIconImageSequence, iconItem(64, 64, make([]byte, 100))))
	if _, err := Read(broken); !errors.Is(err, ErrNoIcon) {
		t.Fatalf("Read of a truncated icon = %v, want ErrNoIcon", err)
	}
	if _, err := Read(roundTrip(t)); !errors.Is(err, ErrNoIcon) {
		t.Fatalf("Read without icons = %v, want ErrNoIcon", err)
	}
}
