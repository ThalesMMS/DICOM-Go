package encapimage

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"testing"

	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/validation/iod"
)

func pngInput(t testing.TB, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func grayInput(t testing.TB) []byte {
	g := image.NewGray(image.Rect(0, 0, 2, 2))
	copy(g.Pix, []byte{0, 23, 128, 255})
	return pngInput(t, g)
}
func imported(t *testing.T, b [][]byte, o ImageOptions) (*object.File, ImportReport) {
	t.Helper()
	f, r, e := FromImages(context.Background(), b, o)
	if e != nil {
		t.Fatal(e)
	}
	checkImportIOD(t, f)
	return f, r
}
func checkImportIOD(t testing.TB, f *object.File) {
	t.Helper()
	r, e := iod.Validate(f.Dataset, iod.Options{CheckValues: true})
	if e != nil || len(r.Errors()) != 0 {
		t.Fatalf("IOD: err=%v findings=%+v", e, r.Errors())
	}
}
func part10Roundtrip(t *testing.T, f *object.File) *object.File {
	t.Helper()
	var b bytes.Buffer
	if e := object.WriteFile(&b, f); e != nil {
		t.Fatal(e)
	}
	out, e := object.ReadFile(bytes.NewReader(b.Bytes()))
	if e != nil {
		t.Fatal(e)
	}
	if out.TransferSyntax.UID != f.TransferSyntax.UID {
		t.Fatal("syntax changed")
	}
	checkImportIOD(t, out)
	return out
}
func nativeBytes(t *testing.T, f *object.File) []byte {
	t.Helper()
	p, e := pixeldata.ExtractView(f.Dataset)
	if e != nil || p.Encapsulated {
		t.Fatalf("native pixels: %v %+v", e, p)
	}
	return p.Raw
}
func TestFromImagesPNGGrayAndRGB16SampleExact(t *testing.T) {
	f, r := imported(t, [][]byte{grayInput(t)}, ImageOptions{})
	if r.Decoded != true || r.Encapsulated || r.Rows != 2 || r.Columns != 2 || r.Frames != 1 || r.BitsStored != 8 {
		t.Fatalf("report: %+v", r)
	}
	if got := nativeBytes(t, part10Roundtrip(t, f)); !bytes.Equal(got, []byte{0, 23, 128, 255}) {
		t.Fatalf("pixels %x", got)
	}
	rgb := image.NewNRGBA64(image.Rect(0, 0, 2, 1))
	rgb.SetNRGBA64(0, 0, color.NRGBA64{R: 1, G: 65535, B: 1024, A: 65535})
	rgb.SetNRGBA64(1, 0, color.NRGBA64{R: 456, G: 789, B: 321, A: 65535})
	f, r = imported(t, [][]byte{pngInput(t, rgb)}, ImageOptions{})
	want := make([]byte, 12)
	for i, v := range []uint16{1, 65535, 1024, 456, 789, 321} {
		binary.LittleEndian.PutUint16(want[2*i:], v)
	}
	if !bytes.Equal(nativeBytes(t, part10Roundtrip(t, f)), want) || r.BitsStored != 16 || getString(f.Dataset, tagPhotometric) != "RGB" {
		t.Fatal("RGB16 samples changed")
	}
}
func TestFromImagesTransparentPNGRefusesWithoutMutation(t *testing.T) {
	g := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	g.SetNRGBA(0, 0, color.NRGBA{R: 255, A: 128})
	data := pngInput(t, g)
	before := bytes.Clone(data)
	f, r, e := FromImages(context.Background(), [][]byte{data}, ImageOptions{})
	if e == nil || f != nil || r.Frames != 0 || !bytes.Equal(before, data) {
		t.Fatalf("transparent input accepted/mutated: %v", e)
	}
}

func TestFromImagesConcurrentInputsRemainImmutable(t *testing.T) {
	input := grayInput(t)
	before := bytes.Clone(input)
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			f, _, e := FromImages(context.Background(), [][]byte{input}, ImageOptions{})
			if e == nil && f == nil {
				e = fmt.Errorf("nil success")
			}
			results <- e
		}()
	}
	for i := 0; i < 8; i++ {
		if e := <-results; e != nil {
			t.Fatal(e)
		}
	}
	if !bytes.Equal(input, before) {
		t.Fatal("shared input mutated")
	}
}
