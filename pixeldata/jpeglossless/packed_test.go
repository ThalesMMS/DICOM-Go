package jpeglossless

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestPackedSV1MatchesPlanarPrediction(t *testing.T) {
	const width, height = 19, 7
	for _, bits := range []uint16{8, 16} {
		for _, representation := range []uint16{0, 1} {
			for _, restart := range []int{0, 1, width - 1, width, width + 1} {
				for _, shift := range []int{0, 2} {
					name := fmt.Sprintf("%d-bit/representation-%d/restart-%d/transform-%d", bits, representation, restart, shift)
					t.Run(name, func(t *testing.T) {
						metadata := encoderTestMetadata(height, width, 1, bits, bits, representation, "MONOCHROME2")
						values := make([]int32, width*height)
						expected := make([]byte, metadata.FrameSize())
						mask := int32((1 << bits) - 1)
						for i := range values {
							value := (int32(i*971) ^ int32((i%13)<<7)) & mask
							if bits == 16 {
								// The simple test encoder writes difference
								// magnitudes. Keep them below category 16
								// while crossing the signed storage boundary.
								value = 0x4000 + value%0x7000
							}
							value = value >> shift << shift
							values[i] = value
							if bits == 8 {
								expected[i] = byte(value)
							} else {
								binary.LittleEndian.PutUint16(expected[i*2:], uint16(value))
							}
						}
						stream := encodeLosslessWithOptions(width, height, int(bits), 1, shift, values, restart)
						legacy, err := decodeFrame(stream, 1, metadata)
						if err != nil || !bytes.Equal(legacy.toInterleavedBytes(bits), expected) {
							t.Fatalf("planar output mismatch: %v", err)
						}
						ctx := context.Background()
						packed, _, err := decodeFrameWithRecovery(ctx, stream, 1, metadata, encoderQuirks{}, false)
						if err != nil || packed.packedMono == nil || packed.planes[0] != nil || !bytes.Equal(packed.toInterleavedBytes(bits), expected) {
							t.Fatalf("packed output mismatch or full work plane allocated: %v", err)
						}
					})
				}
			}
		}
	}
}

func TestPackedSV1DecodeAgainstDCMTK(t *testing.T) {
	dcmcjpeg := dcmtkoracle.Tool(t, "dcmcjpeg")
	for _, sample := range []struct{ bits, representation uint16 }{{8, 0}, {8, 1}, {16, 0}, {16, 1}} {
		for _, shift := range []int{0, 2} {
			t.Run(fmt.Sprintf("bits-%d/representation-%d/transform-%d", sample.bits, sample.representation, shift), func(t *testing.T) {
				metadata := encoderTestMetadata(17, 31, 1, sample.bits, sample.bits, sample.representation, "MONOCHROME2")
				pixels := encoderTestFrame(metadata, 19)
				for offset := 0; offset < len(pixels); offset += int(sample.bits / 8) {
					pixels[offset] &^= byte((1 << shift) - 1)
				}
				dir := t.TempDir()
				input, compressed := filepath.Join(dir, "native.dcm"), filepath.Join(dir, "dcmtk-sv1.dcm")
				writer, err := os.Create(input)
				if err != nil {
					t.Fatal(err)
				}
				err = object.WriteFile(writer, &object.File{Dataset: nativeObject(metadata, pixels), TransferSyntax: transfer.ExplicitVRLittleEndian})
				if err = errors.Join(err, writer.Close()); err != nil {
					t.Fatal(err)
				}
				dcmtkoracle.Run(t, "bit-identical stored samples", dcmcjpeg, "+e1", "+tl", "+pt", strconv.Itoa(shift), input, compressed)
				file, err := object.OpenFile(compressed)
				if err != nil {
					t.Fatal(err)
				}
				defer file.Close()
				pixel, err := pixeldata.ExtractView(file.Dataset)
				if err != nil || len(pixel.Sequence.Fragments) != 1 {
					t.Fatalf("DCMTK fragment: %v (%d fragments)", err, len(pixel.Sequence.Fragments))
				}
				legacy, err := decodeFrame(pixel.Sequence.Fragments[0], 1, metadata)
				if err != nil || legacy.pointTransform[0] != shift {
					t.Fatalf("DCMTK point transform %d did not decode: %v", shift, err)
				}
				decoded, err := newSV1().Decode(pixel, file.Dataset)
				if err != nil || len(decoded.Data) != 1 || !bytes.Equal(decoded.Data[0], pixels) || !bytes.Equal(decoded.Data[0], legacy.toInterleavedBytes(sample.bits)) {
					t.Fatalf("DCMTK output differs: %v", err)
				}
			})
		}
	}
}

type cancelPackedDecode struct {
	context.Context
	checks int
}

func (c *cancelPackedDecode) Err() error {
	c.checks--
	if c.checks <= 0 {
		return context.Canceled
	}
	return nil
}

func TestPackedSV1CancelDuringRestartMarkerFill(t *testing.T) {
	ctx := &cancelPackedDecode{Context: context.Background(), checks: 1}
	reader := &bitReader{data: append(bytes.Repeat([]byte{0xff}, 8192), 0xd0)}
	if err := reader.alignToRestart(ctx, 0xd0); !errors.Is(err, context.Canceled) {
		t.Fatalf("restart marker fill: %v", err)
	}
}

func TestPackedSV1KeepsFailedOutputPrivate(t *testing.T) {
	ctx := context.Background()
	metadata := encoderTestMetadata(32, 32, 1, 16, 16, 0, "MONOCHROME2")
	pixels := encoderTestFrame(metadata, 7)
	encoded, err := NewSV1Encoder().EncodeFrame(ctx, pixels, metadata)
	if err != nil {
		t.Fatal(err)
	}
	stream := encoded.Data
	truncated := append(bytes.Clone(stream[:len(stream)/2]), 0xff, 0xd9)
	for _, tc := range []struct {
		name   string
		ctx    context.Context
		stream []byte
		want   error
	}{
		{"missing EOI", ctx, stream[:len(stream)-2], ErrInvalidStream},
		{"short entropy", ctx, truncated, ErrInvalidStream},
		{"cancel after rows", &cancelPackedDecode{Context: ctx, checks: 12}, stream, context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := decodeFrameWithRecovery(tc.ctx, tc.stream, 1, metadata, encoderQuirks{}, false)
			if got != nil || !errors.Is(err, tc.want) {
				t.Fatalf("private output published=%t, error=%v, want %v", got != nil, err, tc.want)
			}
		})
	}
	decoders, encoders := transcodeRegistries(t)
	native := &object.File{Dataset: nativeObject(metadata, pixels), TransferSyntax: transfer.ExplicitVRLittleEndian}
	compressed, _, err := pixeldata.TranscodeFile(ctx, native, transfer.JPEGLosslessSV1, pixeldata.TranscodeOptions{
		DecoderRegistry: decoders, EncoderRegistry: encoders,
	})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := pixeldata.NewFrameReader(ctx, compressed, nil, pixeldata.FrameReaderOptions{Registry: decoders})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	dst := bytes.Repeat([]byte{0xdd}, len(pixels)+1)
	size := len(pixels) - 1
	if result, err := reader.FrameInto(ctx, 0, dst[:size]); result.Written != 0 || !errors.Is(err, pixeldata.ErrBufferTooSmall) || !bytes.Equal(dst, bytes.Repeat([]byte{0xdd}, len(dst))) {
		t.Fatalf("short destination touched: %+v, %v", result, err)
	}
	result, err := reader.FrameInto(ctx, 0, dst)
	if err != nil || result.Written != len(pixels) || result.Layout.Mode != pixeldata.BufferFrameCopy || !bytes.Equal(dst[:len(pixels)], pixels) || dst[len(pixels)] != 0xdd {
		t.Fatalf("caller buffer: %+v, %v", result, err)
	}

	broken := nativeObject(metadata, pixels)
	broken.Put(dicomtest.NewFragmentSequenceElement(core.TagPixelData, nil, truncated))
	failed, err := pixeldata.NewFrameReader(ctx, &object.File{Dataset: broken, TransferSyntax: transfer.JPEGLosslessSV1}, nil,
		pixeldata.FrameReaderOptions{Registry: decoders})
	if err != nil {
		t.Fatal(err)
	}
	defer failed.Close()
	sentinel := bytes.Repeat([]byte{0xdc}, len(dst))
	copy(dst, sentinel)
	if result, err := failed.FrameInto(ctx, 0, dst); result.Written != 0 || !errors.Is(err, ErrInvalidStream) || !bytes.Equal(dst, sentinel) {
		t.Fatalf("malformed stream touched caller buffer: %+v, %v", result, err)
	}
}

func BenchmarkPackedSV1(b *testing.B) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		side uint16
	}{{"CT512", 512}, {"large2048", 2048}} {
		meta := encoderTestMetadata(tc.side, tc.side, 1, 16, 12, 0, "MONOCHROME2")
		frame := smoothTestFrame(meta)
		encoded, err := NewSV1Encoder().EncodeFrame(ctx, frame, meta)
		if err != nil {
			b.Fatal(err)
		}
		for _, packed := range []bool{false, true} {
			method := "planes"
			peakRaster := len(frame) + int(tc.side)*int(tc.side)*4
			if packed {
				method = "packed"
				peakRaster = len(frame)
			}
			decode := func() ([]byte, error) {
				decoded, _, err := decodeFrameState(ctx, encoded.Data, 1, meta, encoderQuirks{}, packed)
				if err != nil {
					return nil, err
				}
				return decoded.toInterleavedBytes(meta.BitsAllocated), nil
			}
			got, err := decode()
			if err != nil || !bytes.Equal(got, frame) {
				b.Fatalf("%s %s mismatch: %v", tc.name, method, err)
			}
			for _, parallel := range []bool{false, true} {
				mode := "single"
				if parallel {
					mode = "parallel"
				}
				b.Run(tc.name+"/"+method+"/"+mode, func(b *testing.B) {
					b.ReportAllocs()
					b.SetBytes(int64(len(frame)))
					b.ResetTimer()
					if parallel {
						b.RunParallel(func(pb *testing.PB) {
							for pb.Next() {
								if _, err := decode(); err != nil {
									b.Error(err)
									return
								}
							}
						})
					} else {
						for i := 0; i < b.N; i++ {
							if _, err := decode(); err != nil {
								b.Fatal(err)
							}
						}
					}
					b.ReportMetric(float64(peakRaster), "peak-raster-B/frame")
				})
			}
		}
	}
}
