package pixeldata_test

import (
	"bytes"
	"context"
	"image"
	"strconv"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestFrameReaderOddNativePayloadMatchesSyntheticSamples(t *testing.T) {
	ctx := context.Background()
	for _, shape := range []struct {
		name                    string
		rows, columns, channels int
		frames, planar          int
	}{
		{"gray-with-final-pad", 3, 9, 1, 3, 0},
		{"rgb-interleaved-with-final-pad", 5, 3, 3, 3, 0},
		{"rgb-planar-with-no-final-pad", 3, 5, 3, 2, 1},
	} {
		size := shape.rows * shape.columns * shape.channels
		// Distinct values across frames, rows and components make the first
		// byte after an odd frame or plane boundary identifiable.
		source := make([]byte, size*shape.frames)
		for frame := 0; frame < shape.frames; frame++ {
			for y := 0; y < shape.rows; y++ {
				for x := 0; x < shape.columns; x++ {
					pixel := y*shape.columns + x
					for channel := 0; channel < shape.channels; channel++ {
						i := pixel*shape.channels + channel
						if shape.planar == 1 {
							i = channel*shape.rows*shape.columns + pixel
						}
						source[frame*size+i] = byte(11 + frame*size + channel*shape.rows*shape.columns + pixel)
					}
				}
			}
		}
		for _, syntax := range []transfer.Syntax{
			transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian, transfer.ExplicitVRBigEndian,
		} {
			t.Run(shape.name+"/"+syntax.Name, func(t *testing.T) {
				dataset := streamTestSource(shape.rows, shape.columns, shape.frames, shape.channels, 8)
				dataset.Put(core.NewRawElement(core.TagPixelData, core.VROB, bytes.Clone(source)))
				if shape.planar == 1 {
					dataset.Put(wideU16(core.NewTag(0x0028, 0x0006), 1))
				}
				file := &object.File{Dataset: dataset, TransferSyntax: transfer.ExplicitVRLittleEndian}
				if syntax.UID != file.TransferSyntax.UID {
					var err error
					file, _, err = pixeldata.TranscodeFile(ctx, file, syntax, pixeldata.TranscodeOptions{})
					if err != nil {
						t.Fatal(err)
					}
				}
				path := writeStreamTestFile(t, t.TempDir(), "odd-frames.dcm", file)
				readOptions := pixeldata.FrameReaderOptions{}
				deferred, err := pixeldata.OpenFrameReader(ctx, path, readOptions)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = deferred.Close() })
				materialized, err := object.OpenFile(path)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = materialized.Close() })
				pixel, ok := materialized.Dataset.Get(core.TagPixelData)
				wire, rawOK := pixel.RawBytes()
				wantLength := len(source) + len(source)%2
				if !ok || !rawOK || (syntax.ExplicitVR && pixel.VR() != core.VROB) || len(wire) != wantLength ||
					!bytes.Equal(wire[:len(source)], source) || (len(wire) > len(source) && wire[len(source)] != 0) {
					t.Fatalf("wire Pixel Data VR = %v, length = %d, want %d, samples = %x", pixel.VR(), len(wire), wantLength, wire)
				}
				inMemory, err := pixeldata.NewFrameReader(ctx, materialized, nil, readOptions)
				if err != nil {
					t.Fatal(err)
				}
				full, _ := fullDecode(t, path, nil)
				if len(full) != shape.frames {
					t.Fatalf("full decode returned %d frames, want %d", len(full), shape.frames)
				}
				for i := shape.frames - 1; i >= 0; i-- {
					want := source[i*size : (i+1)*size]
					if !bytes.Equal(full[i], want) {
						t.Fatalf("full decode frame %d: got %x, want %x", i, full[i], want)
					}
					for _, route := range []struct {
						name   string
						reader *pixeldata.FrameReader
					}{{"deferred", deferred}, {"materialized", inMemory}} {
						frame, metadata, err := route.reader.Frame(ctx, i)
						if err != nil {
							t.Fatalf("%s frame %d: %v", route.name, i, err)
						}
						if metadata.Rows != uint16(shape.rows) || metadata.Columns != uint16(shape.columns) ||
							metadata.SamplesPerPixel != uint16(shape.channels) || metadata.NumberOfFrames != 1 ||
							metadata.PlanarConfiguration != uint16(shape.planar) {
							t.Fatalf("%s frame %d: unexpected metadata %+v", route.name, i, metadata)
						}
						if !bytes.Equal(frame, want) {
							t.Fatalf("%s frame %d: got %x, want %x", route.name, i, frame, want)
						}
						const sentinel = 0xEE
						dst := bytes.Repeat([]byte{sentinel}, size+1)
						buffer, err := route.reader.FrameInto(ctx, i, dst)
						if err != nil || buffer.Written != size || dst[size] != sentinel || !bytes.Equal(dst[:size], want) {
							t.Fatalf("%s frame %d: FrameInto = %x, wrote %d, error = %v", route.name, i, dst, buffer.Written, err)
						}
						// Bottom-right pixel belongs to the final row and each last
						// sample just touches the end of its odd-sized plane.
						roi := image.Rect(shape.columns-1, shape.rows-1, shape.columns, shape.rows)
						got, regionMeta, err := route.reader.Region(ctx, i, roi)
						var wantPixel []byte
						if shape.planar == 1 {
							for c := 1; c <= shape.channels; c++ {
								wantPixel = append(wantPixel, want[c*size/shape.channels-1])
							}
						} else {
							wantPixel = want[size-shape.channels:]
						}
						if err != nil || !bytes.Equal(got, wantPixel) || regionMeta.Rows != 1 || regionMeta.Columns != 1 {
							t.Fatalf("%s frame %d region = %x, metadata = %+v, error = %v, want %x", route.name, i, got, regionMeta, err, wantPixel)
						}
					}
				}
			})
		}
	}
}

func TestFrameReaderNativeIndexBeyondSixteenBits(t *testing.T) {
	const count = 1<<16 + 2
	ctx := context.Background()
	pixels := make([]byte, count*3)
	for i := 0; i < count; i++ {
		// Distinguish both 16-bit boundaries, with a second byte of evidence
		// around each one to catch accidental sign extension or truncation.
		pixels[3*i] = byte(i)
		pixels[3*i+1] = byte(i / 256)
		pixels[3*i+2] = byte(i / 65536)
	}
	dataset := streamTestSource(1, 3, count, 1, 8)
	dataset.Put(core.NewRawElement(core.TagPixelData, core.VROB, pixels))
	path := writeStreamTestFile(t, t.TempDir(), "high-index.dcm", &object.File{
		Dataset: dataset, TransferSyntax: transfer.ExplicitVRLittleEndian,
	})
	reader, err := pixeldata.OpenFrameReader(ctx, path, pixeldata.FrameReaderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	if got := reader.Metadata().NumberOfFrames; got != count {
		t.Fatalf("declared frame count = %d, want %d", got, count)
	}
	for _, tc := range []struct {
		index int
		want  []byte
	}{
		{65537, []byte{1, 0, 1}},
		{32767, []byte{255, 127, 0}},
		{65536, []byte{0, 0, 1}},
		{32768, []byte{0, 128, 0}},
		{65535, []byte{255, 255, 0}},
		{0, []byte{0, 0, 0}},
	} {
		t.Run(strconv.Itoa(tc.index), func(t *testing.T) {
			frame, metadata, err := reader.Frame(ctx, tc.index)
			if err != nil || !bytes.Equal(frame, tc.want) || metadata.NumberOfFrames != 1 {
				t.Fatalf("frame = %v, metadata = %+v, error = %v, want %v", frame, metadata, err, tc.want)
			}
		})
	}
}
