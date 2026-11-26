package dicomdir_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dicomdir"
	"github.com/ThalesMMS/dicom-go/iconimage"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestWriteDICOMDIRUsesPublicGenerate(t *testing.T) {
	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ExplicitVRBigEndian, transfer.JPEGLosslessSV1} {
		spec := profileImage{fileID: dicomdir.FileID{"CT", "IMG00001"}, sopClass: profileCTClass, modality: "CT", syntax: syntax, rows: 2, columns: 4, stored: 12, frames: []func(int, int) int{func(x, _ int) int { return x * 100 }}, extra: []core.Element{
			filesetStringElement(core.NewTag(0x0028, 0x1053), core.VRDS, "0.5"), filesetStringElement(core.NewTag(0x0028, 0x1052), core.VRDS, "-50"),
			filesetStringElement(core.NewTag(0x0028, 0x0030), core.VRDS, "2", "1"), profileUS(core.NewTag(0x0028, 0x0120), 0),
		}}
		// profileWriteImage authors LE raw values; obtain BE through the public
		// transcode primitive so the fixture's numeric bytes match its syntax.
		if syntax == transfer.ExplicitVRBigEndian {
			spec.syntax = transfer.ExplicitVRLittleEndian
		}
		root, fileSet := profileFileSet(t, spec)
		path := filepath.Join(root, "CT", "IMG00001")
		f, err := object.OpenFile(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if syntax == transfer.ExplicitVRBigEndian {
			converted, _, err := pixeldata.TranscodeFile(context.Background(), f, syntax, pixeldata.TranscodeOptions{})
			if err != nil {
				t.Fatal(err)
			}
			var encoded bytes.Buffer
			if err := object.WriteFile(&encoded, converted); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, encoded.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}
			f = converted
			fileSet, err = dicomdir.NewFileSet(root, dicomdir.Options{FileSetUID: filesetFixedUID, FileSetID: "ICONS"})
			if err != nil {
				t.Fatal(err)
			}
			if err := fileSet.Add(context.Background(), spec.fileID); err != nil {
				t.Fatal(err)
			}
		}
		public, err := iconimage.Generate(context.Background(), f, iconimage.GenerateOptions{Width: 32, Height: 32, Monochrome: true, IgnorePadding: true, ByteOrder: binary.LittleEndian})
		if err != nil {
			t.Fatal(err)
		}
		container := object.FromElements([]core.Element{public}, nil)
		items, _ := container.GetSequence(profileTagIconSequence)
		e, _ := items[0].Get(core.TagPixelData)
		expected, _ := e.RawBytes()
		report, _, images := writeDirectory(t, fileSet, dicomdir.WriteOptions{Icons: true, IconSize: 32})
		if report.Icons != 1 || len(images) != 1 {
			t.Fatal(report)
		}
		_, got := iconOf(t, images[0])
		if !bytes.Equal(got, expected) {
			t.Fatal("DICOMDIR differs from public API")
		}
		icon, err := iconimage.Read(images[0])
		if err != nil {
			t.Fatal(err)
		}
		if icon.Image.Bounds().Dx() != 32 || icon.Image.Bounds().Dy() != 32 {
			t.Fatal(icon.Image.Bounds())
		}
		ratio, err := items[0].GetInts(core.NewTag(0x0028, 0x0034))
		if err != nil || len(ratio) != 2 || ratio[0] != 1 || ratio[1] != 1 {
			t.Fatal(ratio, err)
		}
		// Pixel Spacing makes the 4x2 matrix physically square: no bars.
		if got[31] != 255 || got[31*32+31] != 255 {
			t.Fatal("physical aspect not preserved")
		}
	}
}

type cancelIconCodec struct{ cancel context.CancelFunc }

func (c cancelIconCodec) Decode(pixeldata.PixelData, *object.Object) (pixeldata.Frames, error) {
	c.cancel()
	return pixeldata.Frames{}, context.Canceled
}

func TestWriteDICOMDIRIconFallbackAndCancellation(t *testing.T) {
	spec := profileImage{fileID: dicomdir.FileID{"CT", "IMG00001"}, sopClass: profileCTClass, modality: "CT", syntax: transfer.ExplicitVRLittleEndian, rows: 1, columns: 2, stored: 8, frames: []func(int, int) int{constant(5)}, extra: []core.Element{profileUS(core.NewTag(0x0028, 0x0120), 5)}}
	_, fileSet := profileFileSet(t, spec)
	var buf bytes.Buffer
	if _, err := dicomdir.WriteDICOMDIR(context.Background(), &buf, fileSet, dicomdir.WriteOptions{Icons: true}); !errors.Is(err, dicomdir.ErrIconUnavailable) || !errors.Is(err, iconimage.ErrNoUsablePixels) {
		t.Fatal(err)
	}
	report, _, images := writeDirectory(t, fileSet, dicomdir.WriteOptions{Icons: true, BlankIconOnFailure: true})
	_, pixels := iconOf(t, images[0])
	if report.Icons != 1 || len(report.Diagnostics) != 1 || !bytes.Equal(pixels, make([]byte, len(pixels))) {
		t.Fatal(report)
	}
	items, _ := images[0].GetSequence(profileTagIconSequence)
	ratio, err := items[0].GetInts(core.NewTag(0x0028, 0x0034))
	if err != nil || len(ratio) != 2 || ratio[0] != 1 || ratio[1] != 1 {
		t.Fatal(ratio, err)
	}
	spec.syntax = transfer.JPEGLosslessSV1
	spec.extra = nil
	_, fileSet = profileFileSet(t, spec)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := pixeldata.NewMemoryRegistry()
	if err := r.RegisterCodec(spec.syntax.UID, cancelIconCodec{cancel}); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	if _, err := dicomdir.WriteDICOMDIR(ctx, &buf, fileSet, dicomdir.WriteOptions{Icons: true, BlankIconOnFailure: true, Decoders: r}); !errors.Is(err, context.Canceled) || buf.Len() != 0 {
		t.Fatalf("err=%v bytes=%d", err, buf.Len())
	}
}

type largeIconCodec struct{ calls atomic.Int32 }

func (c *largeIconCodec) Decode(pixeldata.PixelData, *object.Object) (pixeldata.Frames, error) {
	c.calls.Add(1)
	return pixeldata.Frames{Rows: 1024, Columns: 1024, Data: [][]byte{make([]byte, 1<<20)}}, nil
}

func TestWriteDICOMDIRKeepsPriorCompressedSourceLimits(t *testing.T) {
	// A controlled codec avoids allocating/encoding all 17 Mi pixels of the
	// source while proving that DICOMDIR retains the prior total admission.
	fragments := make([][]byte, 17)
	for i := range fragments {
		fragments[i] = make([]byte, 1024)
	}
	spec := profileImage{fileID: dicomdir.FileID{"CT", "IMG00001"}, sopClass: profileCTClass, modality: "CT", syntax: transfer.RLELossless, rows: 1, columns: 1, stored: 8, frames: []func(int, int) int{constant(0)}, extra: []core.Element{
		profileUS(core.NewTag(0x0028, 0x0010), 1024), profileUS(core.NewTag(0x0028, 0x0011), 1024), filesetStringElement(core.NewTag(0x0028, 0x0008), core.VRIS, "17"),
		{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB}, Value: core.FragmentSequence{Fragments: fragments}},
	}}
	root, fileSet := profileFileSet(t, spec)
	f, err := object.OpenFile(filepath.Join(root, "CT", "IMG00001"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	registry := pixeldata.NewMemoryRegistry()
	codec := &largeIconCodec{}
	if err := registry.RegisterCodec(spec.syntax.UID, codec); err != nil {
		t.Fatal(err)
	}
	if _, err := iconimage.Generate(context.Background(), f, iconimage.GenerateOptions{Registry: registry}); !errors.Is(err, pixeldata.ErrDecompressResourceLimit) {
		t.Fatal(err)
	}
	if codec.calls.Load() != 0 {
		t.Fatal("public default failed after decode")
	}
	report, _, images := writeDirectory(t, fileSet, dicomdir.WriteOptions{Icons: true, Decoders: registry})
	if report.Icons != 1 || codec.calls.Load() != 1 {
		t.Fatal(report, codec.calls.Load())
	}
	_, pixels := iconOf(t, images[0])
	if !bytes.Equal(pixels, bytes.Repeat([]byte{128}, 64*64)) {
		t.Fatal("constant icon")
	}
}
