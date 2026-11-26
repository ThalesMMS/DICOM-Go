package pixeldata_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"strconv"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestFrameReaderSeparatesBigEndianOWByteLanes(t *testing.T) {
	// Three pixels per frame. The middle OW word holds one sample from each
	// frame, with first frame's last sample in its low byte.
	wire := []byte{11, 10, 13, 12, 15, 14}
	path := sharedUnitsNativeFile(t, transfer.ExplicitVRBigEndian, core.VROW, 8, 1, 3, 2, wire)
	want := [][]byte{{10, 11, 12}, {13, 14, 15}}
	wantRaw := [][]byte{{11, 10, 0, 12}, {14, 13, 0, 15}}
	assertSharedUnitWholeFrames(t, path, want)
	withSharedUnitReaders(t, path, func(t *testing.T, r *pixeldata.FrameReader) {
		for i := range want {
			assertSharedUnitFrame(t, r, i, want[i], wantRaw[i])
			assertSharedUnitRegion(t, r, i, image.Rect(1, 0, 3, 1), want[i][1:])
		}
	})
}

func TestFrameReaderPackedBitsAcrossByteAndWordSeams(t *testing.T) {
	// Round trip to Part 10 so the deferred source is the same real wire
	// as the materialized reader. Pixel bits are 101, 011, 101 in LSB order.
	for _, tc := range []struct {
		name   string
		syntax transfer.Syntax
		vr     core.VR
		wire   []byte
		raw    [][]byte
	}{
		{"le-ob", transfer.ExplicitVRLittleEndian, core.VROB, []byte{0x75, 0xf9}, [][]byte{{5}, {6}, {5}}},
		{"le-ow", transfer.ExplicitVRLittleEndian, core.VROW, []byte{0x75, 0xf9}, [][]byte{{5}, {6}, {5}}},
		{"be-ob", transfer.ExplicitVRBigEndian, core.VROB, []byte{0x75, 0xf9}, [][]byte{{5}, {6}, {5}}},
		{"be-ow", transfer.ExplicitVRBigEndian, core.VROW, []byte{0xf9, 0x75}, [][]byte{{0, 5}, {0, 6}, {0, 5}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := sharedUnitsNativeFile(t, tc.syntax, tc.vr, 1, 1, 3, 3, tc.wire)
			want := [][]byte{{5}, {6}, {5}}
			assertSharedUnitWholeFrames(t, path, want)
			withSharedUnitReaders(t, path, func(t *testing.T, r *pixeldata.FrameReader) {
				for i, cropped := range []byte{2, 3, 2} {
					assertSharedUnitFrame(t, r, i, want[i], tc.raw[i])
					assertSharedUnitRegion(t, r, i, image.Rect(1, 0, 3, 1), []byte{cropped})
				}
				_, err := r.DecodePartial(context.Background(), pixeldata.PartialDecodeRequest{Frame: 1, ReductionLevel: 1})
				if !errors.Is(err, pixeldata.ErrInvalidSampleFormat) {
					t.Fatalf("packed-bit averaging = %v, want unsupported sample format", err)
				}
			})
		})
	}
}

func TestFrameReaderPackedRowsAndScratchBoundary(t *testing.T) {
	const rows, columns, frames = 9, 65535, 2
	const frameBits = rows * columns
	wire := make([]byte, (frameBits*frames+15)/16*2)
	wants := make([][]byte, frames)
	for frame := 0; frame < frames; frame++ {
		want := make([]byte, (frameBits+7)/8)
		for sample := 0; sample < frameBits; sample++ {
			if (sample*17+sample/11+frame*3)%29 >= 11 {
				want[sample/8] |= 1 << (sample % 8)
				index := frame*frameBits + sample
				// An OW word places its low logical byte at the odd wire
				// index, even when the frame starts in that byte.
				wire[(index/8)^1] |= 1 << (index % 8)
			}
		}
		wants[frame] = want
	}
	path := sharedUnitsNativeFile(t, transfer.ExplicitVRBigEndian, core.VROW, 1, rows, columns, frames, wire)
	withSharedUnitReaders(t, path, func(t *testing.T, r *pixeldata.FrameReader) {
		for i, want := range wants {
			got, _, err := r.Frame(context.Background(), i)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("frame %d: equality=%t, bytes=%d, error=%v", i, bytes.Equal(got, want), len(got), err)
			}
			region := image.Rect(13, 2, 22, 5)
			cropped := make([]byte, 4)
			for y := region.Min.Y; y < region.Max.Y; y++ {
				for x := region.Min.X; x < region.Max.X; x++ {
					src := y*columns + x
					dst := (y-region.Min.Y)*region.Dx() + x - region.Min.X
					cropped[dst/8] |= ((want[src/8] >> (src % 8)) & 1) << (dst % 8)
				}
			}
			assertSharedUnitRegion(t, r, i, region, cropped)
		}
	})
}

func TestDecodeNativeHonorsStorageSyntaxAndSingleFramePackedTail(t *testing.T) {
	ctx := context.Background()
	t.Run("explicit-big-endian-syntax-with-typed-metadata", func(t *testing.T) {
		elements := streamTestSource(1, 3, 2, 1, 8).Elements()
		for i := range elements {
			if elements[i].VR() == core.VRUS {
				raw, _ := elements[i].RawBytes()
				elements[i].Value = core.Uint16Value{binary.LittleEndian.Uint16(raw)}
			}
		}
		dataset := object.FromElements(elements, nil)
		dataset.Put(core.NewRawElement(core.TagPixelData, core.VROW, []byte{11, 10, 13, 12, 15, 14}))
		pixel, err := pixeldata.ExtractView(dataset)
		if err != nil {
			t.Fatal(err)
		}
		frames, err := pixeldata.DecodeFramesContext(ctx, transfer.ExplicitVRBigEndian.UID, pixel, dataset)
		if err != nil || len(frames.Data) != 2 || !bytes.Equal(frames.Data[0], []byte{10, 11, 12}) ||
			!bytes.Equal(frames.Data[1], []byte{13, 14, 15}) {
			t.Fatalf("decoded OW frames = %x, %v", frames.Data, err)
		}
	})
	t.Run("single-frame-view-preserves-spare-bits", func(t *testing.T) {
		path := sharedUnitsNativeFile(t, transfer.ExplicitVRLittleEndian, core.VROB, 1, 1, 3, 1, []byte{0xfd, 0})
		file, err := object.OpenFile(path)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		pixel, err := pixeldata.ExtractView(file.Dataset)
		if err != nil {
			t.Fatal(err)
		}
		view, err := pixeldata.ExtractNativeFramesView(file.Dataset)
		if err != nil || len(view.Data) != 1 || !bytes.Equal(view.Data[0], []byte{0xfd}) {
			t.Fatalf("stored view = %+v, %v", view, err)
		}
		frames, err := pixeldata.DecodeFramesContext(ctx, file.TransferSyntax.UID, pixel, file.Dataset)
		if err != nil || len(frames.Data) != 1 || !bytes.Equal(frames.Data[0], []byte{5}) || pixel.Raw[0] != 0xfd {
			t.Fatalf("decoded frame = %x, %v; stored bytes = %x", frames.Data, err, pixel.Raw)
		}
	})
}

func sharedUnitsNativeFile(t *testing.T, syntax transfer.Syntax, vr core.VR, bits, rows, cols uint16, frames int, wire []byte) string {
	t.Helper()
	elements := decompressMetadataElementsWithOrder(syntax.ByteOrder, rows, cols, bits, bits, bits-1)
	elements = append(elements,
		core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0008), VR: core.VRIS}, Value: core.StringValue{strconv.Itoa(frames)}},
		core.NewRawElement(core.TagPixelData, vr, wire),
	)
	dataset := object.FromElements(elements, nil)
	dataset.SetValueByteOrder(syntax.ByteOrder)
	return writeStreamTestFile(t, t.TempDir(), "native.dcm", &object.File{Dataset: dataset, TransferSyntax: syntax})
}

func withSharedUnitReaders(t *testing.T, path string, check func(*testing.T, *pixeldata.FrameReader)) {
	t.Helper()
	t.Run("deferred", func(t *testing.T) {
		r, err := pixeldata.OpenFrameReader(context.Background(), path, pixeldata.FrameReaderOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		check(t, r)
	})
	t.Run("materialized", func(t *testing.T) {
		file, err := object.OpenFile(path)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		r, err := pixeldata.NewFrameReader(context.Background(), file, nil, pixeldata.FrameReaderOptions{})
		if err != nil {
			t.Fatal(err)
		}
		check(t, r)
	})
}

func assertSharedUnitWholeFrames(t *testing.T, path string, want [][]byte) {
	t.Helper()
	got, _ := fullDecode(t, path, nil)
	if len(got) != len(want) {
		t.Fatalf("bulk returned %d frames, want %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Fatalf("bulk frame %d = %x, want %x", i, got[i], want[i])
		}
	}
}

func assertSharedUnitFrame(t *testing.T, r *pixeldata.FrameReader, i int, want, wantRaw []byte) {
	t.Helper()
	ctx := context.Background()
	got, m, err := r.Frame(ctx, i)
	if err != nil || m.NumberOfFrames != 1 || !bytes.Equal(got, want) {
		t.Fatalf("Frame(%d) = %x, %+v, %v; want %x", i, got, m, err, want)
	}
	dst := bytes.Repeat([]byte{0xee}, len(want)+1)
	result, err := r.FrameInto(ctx, i, dst)
	if err != nil || result.Written != len(want) || !bytes.Equal(dst[:len(want)], want) || dst[len(want)] != 0xee {
		t.Fatalf("FrameInto(%d) = %x, %+v, %v; want %x followed by sentinel", i, dst, result, err, want)
	}
	size, err := r.RawFrameLength(i)
	if err != nil || size != int64(len(wantRaw)) {
		t.Fatalf("RawFrameLength(%d) = %d, %v; want %d", i, size, err, len(wantRaw))
	}
	got, err = r.RawFrame(ctx, i)
	if err != nil || !bytes.Equal(got, wantRaw) {
		t.Fatalf("RawFrame(%d) = %x, %v; want %x", i, got, err, wantRaw)
	}
}

func assertSharedUnitRegion(t *testing.T, r *pixeldata.FrameReader, i int, region image.Rectangle, want []byte) {
	t.Helper()
	ctx := context.Background()
	got, m, err := r.Region(ctx, i, region)
	if err != nil || int(m.Rows) != region.Dy() || int(m.Columns) != region.Dx() || !bytes.Equal(got, want) {
		t.Fatalf("Region(%d, %v) = %x, %+v, %v; want %x", i, region, got, m, err, want)
	}
	dst := bytes.Repeat([]byte{0xee}, len(want)+1)
	result, err := r.RegionInto(ctx, i, region, dst)
	if err != nil || result.Written != len(want) || !bytes.Equal(dst[:len(want)], want) || dst[len(want)] != 0xee {
		t.Fatalf("RegionInto(%d, %v) = %x, %+v, %v; want %x followed by sentinel", i, region, dst, result, err, want)
	}
}
