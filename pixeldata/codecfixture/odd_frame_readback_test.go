package codecfixture

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/builtin"
	"github.com/ThalesMMS/dicom-go/pixeldata/deflateframe"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpeg"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpeglossless"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpegls"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpegxl"
	"github.com/ThalesMMS/dicom-go/pixeldata/rle"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// An even-length encapsulated Item must never force an odd-sized decoded
// frame to acquire an extra byte or shift the start of the next frame.
func TestPart10CodecReadbackKeepsExactFrameBoundaries(t *testing.T) {
	ctx := context.Background()
	decoders, err := builtin.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if err := jpegxl.Register(decoders); err != nil {
		t.Fatal(err)
	}
	encoders := pixeldata.NewMemoryEncoderRegistry()
	const quality = 100
	if err := jpeg.RegisterEncoder(encoders, quality); err != nil {
		t.Fatal(err)
	}
	for _, register := range []func(pixeldata.EncoderRegistry) error{
		jpeglossless.RegisterEncoder, jpegls.RegisterEncoder, rle.RegisterEncoder,
		deflateframe.RegisterEncoder, jpegxl.RegisterLosslessEncoder,
	} {
		if err := register(encoders); err != nil {
			t.Fatal(err)
		}
	}
	profiles := []struct {
		syntax    transfer.Syntax
		tolerance int
	}{
		{transfer.JPEGBaseline, 2}, {transfer.JPEGLosslessSV1, 0}, {transfer.JPEGLSLossless, 0},
		{transfer.RLELossless, 0}, {transfer.DeflatedImageFrameCompression, 0}, {transfer.JPEGXLLossless, 0},
	}
	shapes := []struct {
		name                   string
		rows, columns, samples int
		frames                 int
	}{
		{"one-odd-gray", 3, 9, 1, 1},
		{"three-odd-gray", 5, 3, 1, 3},
		{"three-odd-rgb", 3, 7, 3, 3},
		{"three-even-gray", 4, 5, 1, 3},
	}
	for _, profile := range profiles {
		for _, shape := range shapes {
			if profile.tolerance != 0 && shape.samples == 3 {
				continue // This lossy profile uses a grayscale sample oracle.
			}
			t.Run(profile.syntax.UID+"/"+shape.name, func(t *testing.T) {
				elements := metadataElements(t.Name(), shape.rows, shape.columns, shape.frames, fragmentElement())
				sopClass := "1.2.840.10008.5.1.4.1.1.7" // Secondary Capture
				photometric := "MONOCHROME2"
				switch {
				case shape.samples == 3:
					sopClass = "1.2.840.10008.5.1.4.1.1.7.4" // Multi-frame True Color Secondary Capture
					photometric = "RGB"
					elements = append(elements, uint16Element(tagPlanarConfiguration, 0))
				case shape.frames > 1:
					sopClass = "1.2.840.10008.5.1.4.1.1.7.2" // Multi-frame Grayscale Byte Secondary Capture
				}
				elements = replaceElement(elements, stringElement(tagSOPClassUID, core.VRUI, sopClass))
				elements = replaceElement(elements, stringElement(tagModality, core.VRCS, "OT"))
				elements = replaceElement(elements, stringElement(tagPhotometricInterpretation, core.VRCS, photometric))
				elements = replaceElement(elements, uint16Element(tagSamplesPerPixel, uint16(shape.samples)))
				dataset := object.FromElements(elements, nil)
				metadata, err := pixeldata.ExtractMetadata(dataset)
				if err != nil {
					t.Fatal(err)
				}
				size := shape.rows * shape.columns * shape.samples
				want := make([][]byte, shape.frames)
				items := make([][]byte, shape.frames)
				for frame := range want {
					pixels := make([]byte, size)
					for i := range pixels {
						pixel := i / shape.samples
						channel := i % shape.samples
						pixels[i] = byte(19 + (pixel%shape.columns)*4 + (pixel/shape.columns)*7 + channel*41 + frame*49)
					}
					want[frame] = pixels
					item, err := encoders.EncodeFrame(ctx, profile.syntax.UID, pixels, metadata)
					if err != nil {
						t.Fatal(err)
					}
					items[frame] = item.Data
					if len(items[frame])%2 != 0 {
						items[frame] = append(items[frame], 0)
					}
				}
				dataset.Put(fragmentElement(items...))
				var wire bytes.Buffer
				if err := object.WriteFile(&wire, &object.File{Dataset: dataset, TransferSyntax: profile.syntax}); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(t.TempDir(), "capture.dcm")
				if err := os.WriteFile(path, wire.Bytes(), 0o600); err != nil {
					t.Fatal(err)
				}
				reader, err := pixeldata.OpenFrameReader(ctx, path, pixeldata.FrameReaderOptions{Registry: decoders})
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := reader.Close(); err != nil {
						t.Errorf("close: %v", err)
					}
				}()
				// Reverse reads catch a reader that consumes encoded or decoded
				// frames sequentially instead of honoring the requested index.
				for index := shape.frames - 1; index >= 0; index-- {
					got, output, err := reader.Frame(ctx, index)
					if err != nil {
						t.Fatalf("frame %d: %v", index, err)
					}
					if len(got) != size || output.Rows != uint16(shape.rows) || output.Columns != uint16(shape.columns) ||
						output.SamplesPerPixel != uint16(shape.samples) || output.NumberOfFrames != 1 || output.PhotometricInterpretation != photometric ||
						output.FrameSize() != int64(size) || (shape.samples == 3 && (!output.PlanarConfigurationPresent || output.PlanarConfiguration != 0)) {
						t.Fatalf("frame %d returned %d bytes and metadata %+v, want %d bytes and %s %dx%d", index, len(got), output, size, photometric, shape.columns, shape.rows)
					}
					for i, sample := range got {
						delta := int(sample) - int(want[index][i])
						if delta < -profile.tolerance || delta > profile.tolerance {
							t.Fatalf("frame %d sample %d = %d, want %d (+/-%d)", index, i, sample, want[index][i], profile.tolerance)
						}
					}
					dst := bytes.Repeat([]byte{0xb7}, size+3)
					result, err := reader.FrameInto(ctx, index, dst)
					if err != nil || result.Written != size || !bytes.Equal(dst[:size], got) || !bytes.Equal(dst[size:], []byte{0xb7, 0xb7, 0xb7}) {
						t.Fatalf("frame %d caller buffer result %+v, error %v, tail %x", index, result, err, dst[size:])
					}
				}
			})
		}
	}
}
