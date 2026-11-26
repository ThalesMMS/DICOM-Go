package pixeldata_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestDecodeFileFrameKeepsNativePacking(t *testing.T) {
	file := nativePackedTestFile(t, 3, 3, transfer.ExplicitVRBigEndian, core.VROW, []byte{0xff, 0xcd})
	metadata, frames, encapsulated, err := pixeldata.DecodeFileFramesView(context.Background(), file, nil)
	if err != nil || encapsulated || len(frames) != 3 {
		t.Fatalf("batch: metadata=%+v, frames=%x, encapsulated=%v, err=%v", metadata, frames, encapsulated, err)
	}
	for i, want := range []byte{5, 1, 7} {
		got, frame, encapsulated, err := pixeldata.DecodeFileFrame(context.Background(), file, i, nil)
		if err != nil || encapsulated || got != metadata || !bytes.Equal(frame, []byte{want}) {
			t.Fatalf("frame %d: metadata=%+v, frame=%x, encapsulated=%v, err=%v", i, got, frame, encapsulated, err)
		}
		frame[0] = 0xff
		if !bytes.Equal(frames[i], []byte{want}) {
			t.Fatalf("frame %d shares storage with batch output", i)
		}
	}
	file.Dataset.Put(core.NewRawElement(core.TagPixelData, core.VROW, []byte{0xff}))
	if _, frame, _, err := pixeldata.DecodeFileFrame(context.Background(), file, 0, nil); frame != nil || !errors.Is(err, pixeldata.ErrPixelDataSizeMismatch) {
		t.Fatalf("truncated packed data: frame=%x, err=%v", frame, err)
	}
}

func TestDecodeFileFramesPreserveExternalRoutes(t *testing.T) {
	for _, test := range []struct {
		syntax transfer.Syntax
		want   error
	}{
		{transfer.JPIPReferenced, pixeldata.ErrJPIPRetrievalRequired},
		{transfer.MPEG4HP41F, pixeldata.ErrMediaPayloadNotRenderable},
	} {
		t.Run(test.syntax.UID, func(t *testing.T) {
			file := nativePackedTestFile(t, 3, 3, transfer.ExplicitVRLittleEndian, core.VROB, []byte{0xcd, 0xff})
			file.TransferSyntax = test.syntax
			file.Dataset.Put(dicomtest.StringElement(core.NewTag(0x0028, 0x7fe0), core.VRUR, "https://example.test/jpip"))
			_, all, special, err := pixeldata.DecodeFileFramesView(context.Background(), file, nil)
			if len(all) != 0 || !special || !errors.Is(err, test.want) {
				t.Fatalf("batch: frames=%x, special=%v, err=%v, want %v", all, special, err, test.want)
			}
			_, frame, special, err := pixeldata.DecodeFileFrame(context.Background(), file, 1, nil)
			if len(frame) != 0 || !special || !errors.Is(err, test.want) {
				t.Fatalf("selected: frame=%x, special=%v, err=%v, want %v", frame, special, err, test.want)
			}
		})
	}
}

func TestDecodeFileFramePreservesEffectiveColorAndContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	syntax := transfer.RLELossless
	order := syntax.ByteOrder
	file := &object.File{
		TransferSyntax: syntax,
		Dataset: object.FromElements([]core.Element{
			dicomtest.Uint16Element(core.NewTag(0x0028, 0x0002), core.VRUS, order, 3),
			dicomtest.StringElement(core.NewTag(0x0028, 0x0004), core.VRCS, "YBR_FULL"),
			dicomtest.Uint16Element(core.NewTag(0x0028, 0x0006), core.VRUS, order, 1),
			dicomtest.StringElement(core.NewTag(0x0028, 0x0008), core.VRIS, "2"),
			dicomtest.Uint16Element(core.NewTag(0x0028, 0x0010), core.VRUS, order, 1),
			dicomtest.Uint16Element(core.NewTag(0x0028, 0x0011), core.VRUS, order, 2),
			dicomtest.Uint16Element(core.NewTag(0x0028, 0x0100), core.VRUS, order, 8),
			dicomtest.Uint16Element(core.NewTag(0x0028, 0x0101), core.VRUS, order, 8),
			dicomtest.Uint16Element(core.NewTag(0x0028, 0x0102), core.VRUS, order, 7),
			dicomtest.Uint16Element(core.NewTag(0x0028, 0x0103), core.VRUS, order, 0),
			{
				Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB, Length: core.UndefinedLength, LengthSet: true},
				Value:  core.FragmentSequence{Fragments: [][]byte{{1, 0}, {2, 0}}},
			},
		}, nil),
	}
	var seen []byte
	registry := pixeldata.NewMemoryRegistry()
	if err := registry.RegisterCodec(syntax.UID, fileFrameTestCodec(func(callCtx context.Context, pixels pixeldata.PixelData, obj *object.Object) (pixeldata.Frames, error) {
		metadata, err := pixeldata.ExtractMetadata(obj)
		if err != nil {
			return pixeldata.Frames{}, err
		}
		if metadata.NumberOfFrames != len(pixels.Sequence.Fragments) {
			t.Fatal("codec frame metadata and input differ")
		}
		result := pixeldata.Frames{Rows: 1, Columns: 2, PhotometricInterpretation: "RGB"}
		for _, fragment := range pixels.Sequence.Fragments {
			seen = append(seen, fragment[0])
			result.Data = append(result.Data, []byte{fragment[0], 10, 20, 30, 40, 50})
		}
		if callCtx == ctx {
			cancel()
		}
		return result, nil
	})); err != nil {
		t.Fatal(err)
	}
	metadata, all, encapsulated, err := pixeldata.DecodeFileFramesView(context.Background(), file, registry)
	if err != nil || !encapsulated || !bytes.Equal(seen, []byte{1, 2}) || len(all) != 2 {
		t.Fatalf("batch: frames=%x, seen=%v, encapsulated=%v, err=%v", all, seen, encapsulated, err)
	}
	if metadata.PhotometricInterpretation != "RGB" || metadata.PlanarConfiguration != 0 || !metadata.PlanarConfigurationPresent {
		t.Fatalf("batch effective metadata = %+v", metadata)
	}
	seen = nil
	got, frame, encapsulated, err := pixeldata.DecodeFileFrame(context.Background(), file, 1, registry)
	if err != nil || !encapsulated || got != metadata || !bytes.Equal(seen, []byte{2}) || !bytes.Equal(frame, all[1]) {
		t.Fatalf("selected: metadata=%+v, frame=%x, seen=%v, encapsulated=%v, err=%v", got, frame, seen, encapsulated, err)
	}
	frame[0] = 0xff
	if all[1][0] != 2 {
		t.Fatal("selected frame shares storage with batch output")
	}
	seen = nil
	_, frame, _, err = pixeldata.DecodeFileFrame(ctx, file, 0, registry)
	if !errors.Is(err, context.Canceled) || len(frame) != 0 || !bytes.Equal(seen, []byte{1}) {
		t.Fatalf("canceled decode: frame=%x, seen=%v, err=%v", frame, seen, err)
	}
}

type fileFrameTestCodec func(context.Context, pixeldata.PixelData, *object.Object) (pixeldata.Frames, error)

func (codec fileFrameTestCodec) Decode(pixels pixeldata.PixelData, obj *object.Object) (pixeldata.Frames, error) {
	return codec(context.Background(), pixels, obj)
}

func (codec fileFrameTestCodec) DecodeContext(ctx context.Context, pixels pixeldata.PixelData, obj *object.Object) (pixeldata.Frames, error) {
	return codec(ctx, pixels, obj)
}
