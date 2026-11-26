//go:build (jpegls_charls || codecfull) && (darwin || linux || windows)

package jpegls

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	puregojpegls "github.com/ThalesMMS/dicom-go/pixeldata/jpegls"
)

func TestCharLSDecodesPureGoLosslessEncoderOutput(t *testing.T) {
	decoder := NewCharLSDecoder()
	for _, test := range []struct {
		name     string
		metadata pixeldata.Metadata
		frame    []byte
	}{
		{
			name: "monochrome 8-bit runs and gradients",
			metadata: pixeldata.Metadata{
				Rows: 2, Columns: 4, SamplesPerPixel: 1,
				BitsAllocated: 8, BitsStored: 8, HighBit: 7,
				NumberOfFrames: 1, PhotometricInterpretation: "MONOCHROME2",
			},
			frame: []byte{0, 0, 32, 64, 128, 192, 255, 255},
		},
		{
			name: "monochrome 16-bit",
			metadata: pixeldata.Metadata{
				Rows: 2, Columns: 3, SamplesPerPixel: 1,
				BitsAllocated: 16, BitsStored: 16, HighBit: 15,
				NumberOfFrames: 1, PhotometricInterpretation: "MONOCHROME2",
			},
			frame: []byte{0, 0, 1, 0, 255, 0, 0, 128, 254, 255, 255, 255},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := puregojpegls.NewEncoder().EncodeFrame(context.Background(), test.frame, test.metadata)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := decoder.DecodeJPEGLS(encoded.Data, DecoderInput{Metadata: test.metadata})
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(decoded, test.frame) {
				t.Fatalf("CharLS decoded pixels differ from pure-Go encoder input")
			}
		})
	}
}

// interleaveInteropFrame is a deterministic RGB frame: gradients, flat runs
// and a noisy band, little-endian when wider than 8 bits.
func interleaveInteropFrame(rows, columns, bits int) []byte {
	top := uint32(1)<<bits - 1
	step := 1
	if bits > 8 {
		step = 2
	}
	out := make([]byte, rows*columns*3*step)
	state := uint32(2463534242)
	i := 0
	for y := 0; y < rows; y++ {
		for x := 0; x < columns; x++ {
			for c := 0; c < 3; c++ {
				state ^= state << 13
				state ^= state >> 17
				state ^= state << 5
				v := (uint32(x*(c+3)+y*5) + state%5) & top
				if y%9 == 4 || x > columns*3/4 {
					v = top / uint32(c+2)
				}
				if step == 2 {
					binary.LittleEndian.PutUint16(out[i:], uint16(v))
				} else {
					out[i] = byte(v)
				}
				i += step
			}
		}
	}
	return out
}

// TestCharLSDecodesPureGoInterleavedEncoderOutput decodes the pure-Go
// encoder's line (ILV=1) and sample (ILV=2) interleaved RGB streams with
// CharLS: lossless streams return the source samples and near-lossless ones
// the pure-Go decoder's reconstruction. It also encodes the same samples with
// CharLS and logs both stream sizes and encode times; the pure-Go stream must
// stay within 1% of CharLS's size.
func TestCharLSDecodesPureGoInterleavedEncoderOutput(t *testing.T) {
	if err := ValidateRuntime(); err != nil {
		t.Fatal(err)
	}
	const rows, columns = 192, 256
	for _, bits := range []int{8, 12} {
		allocated := 8
		if bits > 8 {
			allocated = 16
		}
		frame := interleaveInteropFrame(rows, columns, bits)
		metadata := pixeldata.Metadata{
			Rows: rows, Columns: columns, SamplesPerPixel: 3,
			BitsAllocated: uint16(allocated), BitsStored: uint16(bits), HighBit: uint16(bits - 1),
			NumberOfFrames: 1, PhotometricInterpretation: "RGB", PlanarConfigurationPresent: true,
		}
		for _, near := range []int{0, 2} {
			for _, ilv := range []puregojpegls.InterleaveMode{puregojpegls.InterleaveLine, puregojpegls.InterleaveSample} {
				t.Run(fmt.Sprintf("bits%d/near%d/ilv%d", bits, near, ilv), func(t *testing.T) {
					encoder, err := puregojpegls.NewEncoderWithOptions(puregojpegls.EncoderOptions{Near: near, AllowLossy: near > 0, Interleave: ilv})
					if err != nil {
						t.Fatal(err)
					}
					start := time.Now()
					encoded, err := encoder.EncodeFrame(context.Background(), frame, metadata)
					pureGoTime := time.Since(start)
					if err != nil {
						t.Fatal(err)
					}
					decoded, mode, err := decodeCharLSFrameForTest(encoded.Data)
					if err != nil {
						t.Fatal(err)
					}
					if mode != int32(ilv) {
						t.Fatalf("CharLS read interleave mode %d, want %d", mode, ilv)
					}
					want := frame
					if near > 0 {
						frames, err := puregojpegls.NewNearLossless().Decode(pixeldata.PixelData{Encapsulated: true, Sequence: core.FragmentSequence{Fragments: [][]byte{encoded.Data}}}, interleaveInteropObject(metadata))
						if err != nil || len(frames.Data) != 1 {
							t.Fatalf("pure-Go decode: %v", err)
						}
						want = frames.Data[0]
					}
					if !bytes.Equal(decoded, want) {
						t.Fatalf("CharLS decoded samples differ from the pure-Go encoder's (near=%d)", near)
					}
					start = time.Now()
					charls, err := encodeCharLSFrameForTest(charlsFrameSpec{rows: rows, columns: columns, bitsAllocated: bits, samplesPerPixel: 3, nearLossless: near, interleave: int(ilv)}, frame)
					charlsTime := time.Since(start)
					if err != nil {
						t.Fatal(err)
					}
					ratio := float64(len(encoded.Data)) / float64(len(charls))
					t.Logf("pure-Go %d bytes in %v, CharLS %d bytes in %v (size ratio %.4f)", len(encoded.Data), pureGoTime, len(charls), charlsTime, ratio)
					if ratio > 1.01 {
						t.Fatalf("pure-Go stream is %.2f%% larger than CharLS's", (ratio-1)*100)
					}
				})
			}
		}
	}
}

func interleaveInteropObject(metadata pixeldata.Metadata) *object.Object {
	us := func(element, value uint16) core.Element {
		return core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0028, element), VR: core.VRUS}, Value: core.Uint16Value{value}}
	}
	return object.FromElements([]core.Element{
		us(0x0002, metadata.SamplesPerPixel),
		{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0004), VR: core.VRCS}, Value: core.StringValue{metadata.PhotometricInterpretation}},
		us(0x0006, 0),
		{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0008), VR: core.VRIS}, Value: core.StringValue{"1"}},
		us(0x0010, metadata.Rows),
		us(0x0011, metadata.Columns),
		us(0x0100, metadata.BitsAllocated),
		us(0x0101, metadata.BitsStored),
		us(0x0102, metadata.HighBit),
		us(0x0103, 0),
	}, nil)
}
