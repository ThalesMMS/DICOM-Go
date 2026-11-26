package iconimage

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image/color"
	"io"
	"math"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpeglossless"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func generateFile(columns, rows int, data []byte, extra ...core.Element) *object.File {
	item := iconItem(columns, rows, data)
	return &object.File{Dataset: object.FromElements(append(item.Elements, extra...), nil), TransferSyntax: transfer.ExplicitVRLittleEndian}
}

func generatedItem(t *testing.T, file *object.File, opts GenerateOptions) *object.Object {
	t.Helper()
	element, err := Generate(context.Background(), file, opts)
	if err != nil {
		t.Fatal(err)
	}
	itemsRaw := element.Value.(core.SequenceValue).Items
	for i := 1; i < len(itemsRaw[0].Elements); i++ {
		if !itemsRaw[0].Elements[i-1].Tag().Less(itemsRaw[0].Elements[i].Tag()) {
			t.Fatal("generated item tags are not strictly increasing")
		}
	}
	order := opts.ByteOrder
	if order == nil {
		order = file.Dataset.ValueByteOrder()
	}
	dataset := object.FromElements([]core.Element{element}, nil)
	dataset.SetValueByteOrder(order)
	items, ok := dataset.GetSequence(tagIconImageSequence)
	if !ok || len(items) != 1 {
		t.Fatalf("sequence items=%d present=%v", len(items), ok)
	}
	item := items[0]
	m, err := pixeldata.ExtractMetadata(item)
	if err != nil {
		t.Fatal(err)
	}
	if m.BitsAllocated != 8 || m.BitsStored != 8 || m.HighBit != 7 || m.SamplesPerPixel != 1 || m.PixelRepresentation != 0 || m.PlanarConfigurationPresent {
		t.Fatalf("invalid icon metadata %+v", m)
	}
	if m.PhotometricInterpretation != "MONOCHROME2" && m.PhotometricInterpretation != "PALETTE COLOR" {
		t.Fatal(m.PhotometricInterpretation)
	}
	ratio, err := item.GetInts(core.NewTag(0x0028, 0x0034))
	if err != nil || !reflect.DeepEqual(ratio, []int64{1, 1}) {
		t.Fatalf("ratio=%v %v", ratio, err)
	}
	return item
}

func generatedPixels(t *testing.T, item *object.Object) []byte {
	t.Helper()
	e, ok := item.Get(core.TagPixelData)
	if !ok {
		t.Fatal("no PixelData")
	}
	raw, ok := e.RawBytes()
	if !ok || e.VR() != core.VROB {
		t.Fatal("not native OB")
	}
	return raw
}

func TestGenerateGrayWindowDomains(t *testing.T) {
	for _, tc := range []struct {
		name             string
		slope, intercept string
		photo            string
		window           WindowMode
		min, max         float64
		data, want       []byte
	}{
		{"fractional", "0.1", "0", "MONOCHROME2", WindowMinMax, 0, 1, []byte{0, 5, 10}, []byte{0, 128, 255}},
		{"negative slope automatic", "-2", "20", "MONOCHROME2", WindowAutomatic, 0, 0, []byte{0, 5, 10}, []byte{255, 128, 0}},
		{"M1 exactly once", "1", "0", "MONOCHROME1", WindowMinMax, 0, 10, []byte{0, 5, 10}, []byte{255, 127, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := generateFile(3, 1, tc.data, text(core.NewTag(0x0028, 0x1053), core.VRDS, tc.slope), text(core.NewTag(0x0028, 0x1052), core.VRDS, tc.intercept), text(core.NewTag(0x0028, 0x0004), core.VRCS, tc.photo))
			before := bytes.Clone(generatedPixels(t, f.Dataset))
			item := generatedItem(t, f, GenerateOptions{Width: 3, Height: 1, Window: tc.window, Min: tc.min, Max: tc.max})
			if got := generatedPixels(t, item); !bytes.Equal(got, tc.want) {
				t.Fatalf("pixels %v want %v", got, tc.want)
			}
			if !bytes.Equal(generatedPixels(t, f.Dataset), before) {
				t.Fatal("source changed")
			}
		})
	}
	// Default stored VOI survives; Automatic explicitly overrides it.
	f := generateFile(3, 1, []byte{0, 5, 10}, text(core.NewTag(0x0028, 0x1050), core.VRDS, "10"), text(core.NewTag(0x0028, 0x1051), core.VRDS, "1"))
	if got := generatedPixels(t, generatedItem(t, f, GenerateOptions{Width: 3, Height: 1})); !bytes.Equal(got, []byte{0, 0, 255}) {
		t.Fatal(got)
	}
	if got := generatedPixels(t, generatedItem(t, f, GenerateOptions{Width: 3, Height: 1, Window: WindowAutomatic})); !bytes.Equal(got, []byte{0, 128, 255}) {
		t.Fatal(got)
	}
}

func generateLUT(tag core.Tag, values ...uint16) core.Element {
	descriptor := make([]byte, 6)
	binary.LittleEndian.PutUint16(descriptor, uint16(len(values)))
	binary.LittleEndian.PutUint16(descriptor[4:], 16)
	data := make([]byte, 2*len(values))
	for i, v := range values {
		binary.LittleEndian.PutUint16(data[2*i:], v)
	}
	return sequence(tag, core.DataSet{Elements: []core.Element{core.NewRawElement(core.NewTag(0x0028, 0x3002), core.VRUS, descriptor), core.NewRawElement(core.NewTag(0x0028, 0x3006), core.VROW, data)}})
}

func TestGenerateModalityAndVOILUT(t *testing.T) {
	// The largest modality value occurs between the stored endpoints.
	f := generateFile(3, 1, []byte{0, 1, 2}, generateLUT(core.NewTag(0x0028, 0x3000), 0, 1000, 100))
	if got := generatedPixels(t, generatedItem(t, f, GenerateOptions{Width: 3, Height: 1, Window: WindowAutomatic})); !bytes.Equal(got, []byte{0, 255, 26}) {
		t.Fatal(got)
	}
	// VOI LUT output is scaled by declared bit depth, not by observed extrema.
	f = generateFile(3, 1, []byte{0, 1, 2}, generateLUT(core.NewTag(0x0028, 0x3010), 0, 32768, 65535))
	if got := generatedPixels(t, generatedItem(t, f, GenerateOptions{Width: 3, Height: 1})); !bytes.Equal(got, []byte{0, 128, 255}) {
		t.Fatal(got)
	}
	f.Dataset.Put(sequence(core.NewTag(0x0028, 0x3000), core.DataSet{}))
	if _, err := Generate(context.Background(), f, GenerateOptions{}); !errors.Is(err, display.ErrInvalidLUTDescriptor) {
		t.Fatalf("malformed modality: %v", err)
	}
}

func TestGeneratePaddingAndConstant(t *testing.T) {
	// Stored padding 0 is not the rescaled modality padding -100.
	f := generateFile(4, 1, []byte{0, 1, 5, 10}, us(core.NewTag(0x0028, 0x0120), 0), us(core.NewTag(0x0028, 0x0121), 1), text(core.NewTag(0x0028, 0x1052), core.VRDS, "-100"))
	if got := generatedPixels(t, generatedItem(t, f, GenerateOptions{Width: 4, Height: 1, IgnorePadding: true})); !bytes.Equal(got, []byte{0, 0, 0, 255}) {
		t.Fatal(got)
	}
	f.Dataset.Put(text(core.NewTag(0x0028, 0x0004), core.VRCS, "MONOCHROME1"))
	if got := generatedPixels(t, generatedItem(t, f, GenerateOptions{Width: 4, Height: 1, IgnorePadding: true})); !bytes.Equal(got, []byte{0, 0, 255, 0}) {
		t.Fatal(got)
	}
	f = generateFile(2, 1, []byte{5, 5})
	if got := generatedPixels(t, generatedItem(t, f, GenerateOptions{Width: 2, Height: 1})); !bytes.Equal(got, []byte{128, 128}) {
		t.Fatal(got)
	}
	padding := &pixeldata.PixelPadding{Value: 5}
	if _, err := Generate(context.Background(), f, GenerateOptions{Padding: padding}); !errors.Is(err, ErrNoUsablePixels) {
		t.Fatal(err)
	}
	padding.Value = 256
	if _, err := Generate(context.Background(), f, GenerateOptions{Padding: padding}); !errors.Is(err, pixeldata.ErrInvalidPixelPadding) {
		t.Fatal(err)
	}
	// Signed padding and HighBit interpretation use the shared sample reader.
	f = generateFile(3, 1, []byte{0xf0, 0xff, 0, 0, 0xa0, 0}, us(core.NewTag(0x0028, 0x0100), 16), us(core.NewTag(0x0028, 0x0101), 12), us(core.NewTag(0x0028, 0x0102), 15), us(core.NewTag(0x0028, 0x0103), 1), core.NewRawElement(core.NewTag(0x0028, 0x0120), core.VRSS, []byte{0xff, 0xff}))
	if got := generatedPixels(t, generatedItem(t, f, GenerateOptions{Width: 3, Height: 1, IgnorePadding: true})); !bytes.Equal(got, []byte{0, 0, 255}) {
		t.Fatal(got)
	}
}

func TestGeneratePhysicalAspectAndFrames(t *testing.T) {
	for _, extra := range []core.Element{
		{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0030), VR: core.VRDS}, Value: core.StringValue{"2", "1"}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0034), VR: core.VRIS}, Value: core.StringValue{"2", "1"}},
	} {
		f := generateFile(2, 1, []byte{0, 255}, extra)
		if got := generatedPixels(t, generatedItem(t, f, GenerateOptions{Width: 4, Height: 4, Window: WindowMinMax, Min: 0, Max: 255})); !bytes.Equal(got, []byte{0, 0, 255, 255, 0, 0, 255, 255, 0, 0, 255, 255, 0, 0, 255, 255}) {
			t.Fatal(got)
		}
	}
	// A 2:1 matrix with square pixels fills the middle two of four rows.
	f := generateFile(2, 1, []byte{255, 255})
	if got := generatedPixels(t, generatedItem(t, f, GenerateOptions{Width: 4, Height: 4, Window: WindowMinMax, Min: 0, Max: 255})); !bytes.Equal(got, []byte{0, 0, 0, 0, 255, 255, 255, 255, 255, 255, 255, 255, 0, 0, 0, 0}) {
		t.Fatal(got)
	}
	f = generateFile(1, 1, []byte{0, 64, 128, 255}, text(core.NewTag(0x0028, 0x0008), core.VRIS, "4"), us(core.NewTag(0x0028, 0x6010), 4))
	opts := GenerateOptions{Width: 1, Height: 1, Window: WindowMinMax, Min: 0, Max: 255}
	if got := generatedPixels(t, generatedItem(t, f, opts)); got[0] != 255 {
		t.Fatal(got)
	}
	zero := 0
	opts.FrameIndex = &zero
	if got := generatedPixels(t, generatedItem(t, f, opts)); got[0] != 0 {
		t.Fatal(got)
	}
	opts.FrameIndex = nil
	f.Dataset.Remove(core.NewTag(0x0028, 0x6010))
	if got := generatedPixels(t, generatedItem(t, f, opts)); got[0] != 64 {
		t.Fatal(got)
	}
	for _, invalid := range [][]uint16{{0}, {5}, {4, 1}} {
		f.Dataset.SetUS(tagRepresentativeFrameNumber, invalid...)
		if got := generatedPixels(t, generatedItem(t, f, opts)); got[0] != 64 {
			t.Fatalf("fallback for invalid representative number %v = %d, want 64", invalid, got[0])
		}
	}
}

func TestGenerateColorAndByteOrderRoundTrip(t *testing.T) {
	for _, planar := range []int{0, 1} {
		for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
			f := generateFile(3, 1, []byte{255, 0, 0, 0, 255, 0, 0, 0, 255}, us(core.NewTag(0x0028, 0x0002), 3), us(core.NewTag(0x0028, 0x0006), planar), text(core.NewTag(0x0028, 0x0004), core.VRCS, "RGB"))
			if planar == 1 {
				f.Dataset.Put(core.NewRawElement(core.TagPixelData, core.VROB, []byte{255, 0, 0, 0, 255, 0, 0, 0, 255}))
			} // identity primary matrix is the same planar/interleaved.
			if order == binary.BigEndian {
				for _, e := range f.Dataset.Elements() {
					if e.VR() == core.VRUS {
						raw, _ := e.RawBytes()
						raw = bytes.Clone(raw)
						for i := 0; i < len(raw); i += 2 {
							raw[i], raw[i+1] = raw[i+1], raw[i]
						}
						f.Dataset.Put(core.NewRawElement(e.Tag(), e.VR(), raw))
					}
				}
				f.Dataset.SetValueByteOrder(order)
				f.TransferSyntax = transfer.ExplicitVRBigEndian
			}
			item := generatedItem(t, f, GenerateOptions{Width: 3, Height: 1})
			for ch := range 3 {
				desc, _ := item.GetInts(core.NewTag(0x0028, uint16(0x1101+ch)))
				if !reflect.DeepEqual(desc, []int64{256, 0, 8}) {
					t.Fatal(desc)
				}
				e, _ := item.Get(core.NewTag(0x0028, uint16(0x1201+ch)))
				raw, _ := e.RawBytes()
				if e.VR() != core.VROW || len(raw) != 256 {
					t.Fatal("palette layout")
				}
			}
			img, err := RenderItem(item)
			if err != nil {
				t.Fatal(err)
			}
			for x, want := range []color.RGBA{{255, 0, 0, 255}, {0, 255, 0, 255}, {0, 0, 255, 255}} {
				if got := color.RGBAModel.Convert(img.At(x, 0)).(color.RGBA); got != want {
					t.Fatalf("%v planar%d pixel%d=%v want%v", order, planar, x, got, want)
				}
			}
			// Insert public output into a same-order object and write/read Part10.
			element, err := Generate(context.Background(), f, GenerateOptions{Width: 3, Height: 1})
			if err != nil {
				t.Fatal(err)
			}
			container := object.FromElements([]core.Element{text(core.NewTag(0x0008, 0x0016), core.VRUI, "1.2.840.10008.5.1.4.1.1.7"), text(core.NewTag(0x0008, 0x0018), core.VRUI, "2.25.1324"), element}, nil)
			container.SetValueByteOrder(order)
			var buf bytes.Buffer
			if err := object.WriteFile(&buf, &object.File{Dataset: container, TransferSyntax: f.TransferSyntax}); err != nil {
				t.Fatal(err)
			}
			read, err := object.ReadFile(bytes.NewReader(buf.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			icon, err := Read(read.Dataset)
			if err != nil {
				t.Fatal(err)
			}
			if icon.Image.Bounds() != img.Bounds() {
				t.Fatal(icon.Image.Bounds())
			}
			// Palette input follows the same public color path, with padding ignored.
			f = &object.File{Dataset: item, TransferSyntax: transfer.ExplicitVRLittleEndian}
			if order == binary.BigEndian {
				f.TransferSyntax = transfer.ExplicitVRBigEndian
			}
			item2 := generatedItem(t, f, GenerateOptions{Width: 3, Height: 1, IgnorePadding: true})
			if !bytes.Equal(generatedPixels(t, item), generatedPixels(t, item2)) {
				t.Fatal("palette changed")
			}
		}
	}
}

type generateCodec struct {
	calls  atomic.Int32
	cancel context.CancelFunc
}

func (c *generateCodec) Decode(pixeldata.PixelData, *object.Object) (pixeldata.Frames, error) {
	c.calls.Add(1)
	if c.cancel != nil {
		c.cancel()
	}
	return pixeldata.Frames{Rows: 1, Columns: 2, Data: [][]byte{{0, 255}}}, nil
}

func TestGenerateRegistryLimitsAndCancel(t *testing.T) {
	f := generateFile(2, 1, []byte{0, 255})
	f.TransferSyntax = transfer.RLELossless
	f.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB}, Value: core.FragmentSequence{Fragments: [][]byte{{1, 2}}}})
	registry := pixeldata.NewMemoryRegistry()
	codec := &generateCodec{}
	if err := registry.RegisterCodec(f.TransferSyntax.UID, codec); err != nil {
		t.Fatal(err)
	}
	for _, limits := range []pixeldata.DecompressLimits{{MaxPixels: 1}, {MaxFrames: 1}, {MaxNativeBytes: 1}, {MaxInputBytes: 1}} {
		if limits.MaxFrames == 1 {
			f.Dataset.Put(text(core.NewTag(0x0028, 0x0008), core.VRIS, "2"))
		}
		_, err := Generate(context.Background(), f, GenerateOptions{Registry: registry, Limits: limits})
		if !errors.Is(err, pixeldata.ErrDecompressResourceLimit) {
			t.Fatalf("%+v: %v", limits, err)
		}
		f.Dataset.Remove(core.NewTag(0x0028, 0x0008))
	}
	if codec.calls.Load() != 0 {
		t.Fatal("limits invoked codec")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Generate(ctx, f, GenerateOptions{Registry: registry}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	codec.cancel = cancel
	if element, err := Generate(ctx, f, GenerateOptions{Registry: registry}); !errors.Is(err, context.Canceled) || element.Value != nil {
		t.Fatalf("%v %v", element, err)
	}
	codec.cancel = nil
	item := generatedItem(t, f, GenerateOptions{Width: 2, Height: 1, Registry: registry, Limits: pixeldata.DecompressLimits{MaxInputBytes: 2}})
	if got := generatedPixels(t, item); !bytes.Equal(got, []byte{0, 255}) {
		t.Fatal(got)
	}
	if codec.calls.Load() != 2 {
		t.Fatal(codec.calls.Load())
	}
	if e, _ := f.Dataset.Get(core.TagPixelData); e.Value == nil {
		t.Fatal("source lost compressed data")
	} else if _, ok := e.Value.(core.FragmentSequence); !ok {
		t.Fatal("source became native")
	}
	// Limits reject native sources before frame-view/scaled allocations too.
	native := generateFile(2, 1, []byte{0, 255})
	if _, err := Generate(context.Background(), native, GenerateOptions{Limits: pixeldata.DecompressLimits{MaxPixels: 1}}); !errors.Is(err, pixeldata.ErrDecompressResourceLimit) {
		t.Fatal(err)
	}
}

func TestGenerateBuiltinDecodeAndOwnership(t *testing.T) {
	f := generateFile(2, 1, []byte{0, 255})
	metadata, err := pixeldata.ExtractMetadata(f.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := jpeglossless.NewSV1Encoder().EncodeFrame(context.Background(), []byte{0, 255}, metadata)
	if err != nil {
		t.Fatal(err)
	}
	f.TransferSyntax = transfer.JPEGLosslessSV1
	f.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB}, Value: core.FragmentSequence{Fragments: [][]byte{encoded.Data}}})
	before := bytes.Clone(encoded.Data)
	item := generatedItem(t, f, GenerateOptions{Width: 2, Height: 1})
	if got := generatedPixels(t, item); !bytes.Equal(got, []byte{0, 255}) {
		t.Fatal(got)
	}
	generatedPixels(t, item)[0] = 127
	if !bytes.Equal(encoded.Data, before) {
		t.Fatal("source aliased result")
	}
	other := generatedItem(t, f, GenerateOptions{Width: 2, Height: 1})
	if got := generatedPixels(t, other); got[0] != 0 {
		t.Fatal("result reused")
	}
	// FrameReader resolves UID-only and File-Meta-only source syntax. The
	// public generator must neither misclassify fragments nor bypass builtin.
	f.TransferSyntax = transfer.Syntax{UID: transfer.JPEGLosslessSV1.UID}
	if got := generatedPixels(t, generatedItem(t, f, GenerateOptions{Width: 2, Height: 1})); !bytes.Equal(got, []byte{0, 255}) {
		t.Fatal(got)
	}
	f.TransferSyntax = transfer.Syntax{}
	f.Meta = object.FromElements([]core.Element{text(core.NewTag(0x0002, 0x0010), core.VRUI, transfer.JPEGLosslessSV1.UID)}, nil)
	if got := generatedPixels(t, generatedItem(t, f, GenerateOptions{Width: 2, Height: 1})); !bytes.Equal(got, []byte{0, 255}) {
		t.Fatal(got)
	}
}

func TestGenerateInvalidOptionsAndRepresentations(t *testing.T) {
	f := generateFile(1, 1, []byte{1})
	for _, opts := range []GenerateOptions{{Width: -1}, {Width: 1025}, {Height: 1025}, {Window: 99}, {Window: WindowMinMax, Min: 1, Max: 1}, {Window: WindowMinMax, Min: math.NaN(), Max: 1}, {Window: WindowMinMax, Min: -math.MaxFloat64, Max: math.MaxFloat64}, {Limits: pixeldata.DecompressLimits{MaxPixels: -1}}, {FrameIndex: new(int)}} {
		if opts.FrameIndex != nil {
			*opts.FrameIndex = -1
		}
		if _, err := Generate(context.Background(), f, opts); !errors.Is(err, ErrInvalidOptions) {
			t.Fatalf("%+v: %v", opts, err)
		}
	}
	for _, extra := range []core.Element{us(core.NewTag(0x0028, 0x0100), 32), text(core.NewTag(0x0028, 0x0004), core.VRCS, "UNKNOWN"), us(core.NewTag(0x0028, 0x0002), 3)} {
		if _, err := Generate(context.Background(), generateFile(1, 1, []byte{1}, extra), GenerateOptions{}); !errors.Is(err, ErrUnsupportedSource) {
			t.Fatal(err)
		}
	}
	if _, err := Generate(nil, nil, GenerateOptions{}); !errors.Is(err, ErrUnsupportedSource) {
		t.Fatal(err)
	}
	item := generatedItem(t, f, GenerateOptions{})
	m, _ := pixeldata.ExtractMetadata(item)
	if m.Rows != 64 || m.Columns != 64 {
		t.Fatal(m)
	}
	f.Dataset.Put(text(core.NewTag(0x0028, 0x0030), core.VRDS, "invalid"))
	if _, err := Generate(context.Background(), f, GenerateOptions{}); !errors.Is(err, object.ErrMalformedSpacing) {
		t.Fatal(err)
	}
}

func TestGenerateConcurrentRequests(t *testing.T) {
	f := generateFile(3, 1, []byte{0, 1, 2}, generateLUT(core.NewTag(0x0028, 0x3000), 0, 1000, 100))
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			element, err := Generate(context.Background(), f, GenerateOptions{Width: 3, Height: 1})
			if err != nil || element.Value == nil {
				t.Errorf("%v %v", element, err)
			}
		}()
	}
	wg.Wait()
	if !bytes.Equal(generatedPixels(t, f.Dataset), []byte{0, 1, 2}) {
		t.Fatal("source changed")
	}
}

type iconContextCodec func(context.Context, pixeldata.PixelData, *object.Object) (pixeldata.Frames, error)

func (c iconContextCodec) Decode(pixeldata.PixelData, *object.Object) (pixeldata.Frames, error) {
	return pixeldata.Frames{}, errors.New("ContextCodec was bypassed")
}
func (c iconContextCodec) DecodeContext(ctx context.Context, p pixeldata.PixelData, o *object.Object) (pixeldata.Frames, error) {
	return c(ctx, p, o)
}

func TestGenerateDecodesOnlySelectedFrame(t *testing.T) {
	f := generateFile(2, 1, nil, text(core.NewTag(0x0028, 0x0008), core.VRIS, "3"), us(core.NewTag(0x0028, 0x6010), 2))
	f.TransferSyntax = transfer.RLELossless
	fragments := [][]byte{{10, 20}, {30, 40}, {50, 60}}
	f.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB}, Value: core.FragmentSequence{Fragments: fragments}})
	groups := make([]core.DataSet, 3)
	groups[1] = core.DataSet{Elements: []core.Element{
		sequence(core.NewTag(0x0028, 0x9145), core.DataSet{Elements: []core.Element{text(core.NewTag(0x0028, 0x1053), core.VRDS, "2"), text(core.NewTag(0x0028, 0x1052), core.VRDS, "10")}}),
		sequence(core.NewTag(0x0028, 0x9110), core.DataSet{Elements: []core.Element{{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0030), VR: core.VRDS}, Value: core.StringValue{"2", "1"}}}}),
	}}
	f.Dataset.Put(sequence(core.NewTag(0x5200, 0x9230), groups...))
	registry := pixeldata.NewMemoryRegistry()
	calls := 0
	if err := registry.RegisterCodec(f.TransferSyntax.UID, iconContextCodec(func(_ context.Context, p pixeldata.PixelData, o *object.Object) (pixeldata.Frames, error) {
		calls++
		m, err := pixeldata.ExtractMetadata(o)
		if err != nil {
			return pixeldata.Frames{}, err
		}
		if m.NumberOfFrames != 1 || len(p.Sequence.Fragments) != 1 || !bytes.Equal(p.Sequence.Fragments[0], []byte{30, 40}) {
			t.Errorf("codec input metadata=%+v payload=%v", m, p.Sequence.Fragments)
		}
		return pixeldata.Frames{Rows: 1, Columns: 2, Data: [][]byte{bytes.Clone(p.Sequence.Fragments[0])}}, nil
	})); err != nil {
		t.Fatal(err)
	}
	item := generatedItem(t, f, GenerateOptions{Width: 2, Height: 2, Window: WindowMinMax, Min: 0, Max: 100, Registry: registry})
	if got := generatedPixels(t, item); !bytes.Equal(got, []byte{179, 230, 179, 230}) {
		t.Fatal(got)
	}
	if calls != 1 {
		t.Fatalf("decodes=%d want1", calls)
	}
	if !reflect.DeepEqual(fragments, [][]byte{{10, 20}, {30, 40}, {50, 60}}) {
		t.Fatal("compressed source changed")
	}
	if count, _ := f.Dataset.GetString(core.NewTag(0x0028, 0x0008)); count != "3" {
		t.Fatal("source frame count changed")
	}
	// Frame's effective metadata controls decoded RGB even when the source
	// labels encoded samples YBR_FULL_422.
	f = generateFile(2, 1, nil, us(core.NewTag(0x0028, 0x0002), 3), us(core.NewTag(0x0028, 0x0006), 0), text(core.NewTag(0x0028, 0x0004), core.VRCS, "YBR_FULL_422"))
	f.TransferSyntax = transfer.RLELossless
	f.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB}, Value: core.FragmentSequence{Fragments: [][]byte{{1, 2}}}})
	if err := registry.RegisterCodec(f.TransferSyntax.UID, iconContextCodec(func(context.Context, pixeldata.PixelData, *object.Object) (pixeldata.Frames, error) {
		return pixeldata.Frames{Rows: 1, Columns: 2, Data: [][]byte{{255, 0, 0, 0, 0, 255}}, PhotometricInterpretation: "RGB"}, nil
	})); err != nil {
		t.Fatal(err)
	}
	img, err := RenderItem(generatedItem(t, f, GenerateOptions{Width: 2, Height: 1, Registry: registry}))
	if err != nil {
		t.Fatal(err)
	}
	if got := color.RGBAModel.Convert(img.At(0, 0)).(color.RGBA); got != (color.RGBA{255, 0, 0, 255}) {
		t.Fatal(got)
	}
}

type iconReadAt struct {
	*bytes.Reader
	reads int
}

func (r *iconReadAt) ReadAt(p []byte, off int64) (int, error) {
	r.reads++
	return r.Reader.ReadAt(p, off)
}

func TestGenerateDeferredPixelData(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		f := generateFile(2, 1, []byte{10, 20, 30, 40, 50, 60}, text(core.NewTag(0x0028, 0x0008), core.VRIS, "3"), us(core.NewTag(0x0028, 0x6010), 2))
		f.Dataset.Put(text(core.NewTag(0x0008, 0x0016), core.VRUI, "1.2.840.10008.5.1.4.1.1.7"))
		f.Dataset.Put(text(core.NewTag(0x0008, 0x0018), core.VRUI, "2.25.13241"))
		var registry pixeldata.Registry
		if compressed {
			f.TransferSyntax = transfer.RLELossless
			f.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB}, Value: core.FragmentSequence{Fragments: [][]byte{{10, 20}, {30, 40}, {50, 60}}}})
			r := pixeldata.NewMemoryRegistry()
			registry = r
			if err := r.RegisterCodec(f.TransferSyntax.UID, iconContextCodec(func(_ context.Context, p pixeldata.PixelData, _ *object.Object) (pixeldata.Frames, error) {
				return pixeldata.Frames{Rows: 1, Columns: 2, Data: [][]byte{bytes.Clone(p.Sequence.Fragments[0])}}, nil
			})); err != nil {
				t.Fatal(err)
			}
		}
		var encoded bytes.Buffer
		if err := object.WriteFile(&encoded, f); err != nil {
			t.Fatal(err)
		}
		before := bytes.Clone(encoded.Bytes())
		source := &iconReadAt{Reader: bytes.NewReader(encoded.Bytes())}
		deferred, err := object.ReadFileWithOptions(source, object.ReadFileOptions{DeferPixelData: true})
		if err != nil {
			t.Fatal(err)
		}
		defer deferred.Close()
		if e, _ := deferred.Dataset.Get(core.TagPixelData); e.Value != nil {
			t.Fatal("not deferred")
		}
		if _, err := Generate(context.Background(), deferred, GenerateOptions{Registry: registry}); !errors.Is(err, pixeldata.ErrFrameLayout) {
			t.Fatal(err)
		}
		if _, err := Generate(context.Background(), deferred, GenerateOptions{Source: source, Registry: registry, Limits: pixeldata.DecompressLimits{MaxInputBytes: 1}}); !errors.Is(err, pixeldata.ErrDecompressResourceLimit) {
			t.Fatal(err)
		}
		if source.reads != 0 {
			t.Fatal("rejected source was read")
		}
		item := generatedItem(t, deferred, GenerateOptions{Width: 2, Height: 1, Window: WindowMinMax, Min: 0, Max: 255, Source: source, Registry: registry})
		if got := generatedPixels(t, item); !bytes.Equal(got, []byte{30, 40}) {
			t.Fatal(got)
		}
		if e, _ := deferred.Dataset.Get(core.TagPixelData); e.Value != nil {
			t.Fatal("source materialized")
		}
		if !bytes.Equal(encoded.Bytes(), before) {
			t.Fatal("source bytes changed")
		}
		if source.reads == 0 {
			t.Fatal("Source not consumed")
		}
		// Generate borrows its source; the caller retains reader lifetime.
		if _, err := source.ReadAt(make([]byte, 1), 0); err != nil && err != io.EOF {
			t.Fatal(err)
		}
	}
}
