package object

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/transfer"
)

type packedTestFrames struct {
	memoryFrames
	bits int64
}

type ignoringPackedOutputError struct{ memoryFrames }

func (s ignoringPackedOutputError) WriteFrameTo(i int, w io.Writer) (int64, error) {
	_, _ = w.Write(s.frames[i])
	return int64(len(s.frames[i])), nil
}

type shortPackedOutput struct{}

func (shortPackedOutput) Write(data []byte) (int, error) { return len(data) - 1, nil }
func TestWriteFilePackedBitFramesKeepsDestinationFailure(t *testing.T) {
	source := ignoringPackedOutputError{memoryFrames{frames: [][]byte{make([]byte, 33000)}}}
	p := &frameValueProvider{frames: &source, lengths: []int64{33000}, packedBits: 33000 * 8}
	if _, err := p.copyPackedBits(shortPackedOutput{}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("source ignored destination failure: %v", err)
	}
}

func (s *packedTestFrames) FrameBits(int) (int64, error) { return s.bits, nil }

func TestWriteFilePackedBitFramesContinuousAcrossOddFrames(t *testing.T) {
	for _, bits := range []int{1, 3, 7, 8, 9, 15, 257, 65003} {
		t.Run(fmt.Sprint(bits), func(t *testing.T) {
			const count = 5
			source := &packedTestFrames{bits: int64(bits)}
			joint := make([]byte, (bits*count+7)/8)
			for frame := 0; frame < count; frame++ {
				data := make([]byte, (bits+7)/8)
				for bit := 0; bit < bits; bit++ {
					if (frame+bit)%3 == 0 {
						data[bit/8] |= 1 << uint(bit%8)
						absolute := frame*bits + bit
						joint[absolute/8] |= 1 << uint(absolute%8)
					}
				}
				source.frames = append(source.frames, data)
			}
			elements := frameTestDataSet(count, 1)
			columns := make([]byte, 2)
			binary.LittleEndian.PutUint16(columns, uint16(bits))
			header := FromElements(elements, nil)
			header.Put(core.NewRawElement(core.NewTag(0x0028, 0x0011), core.VRUS, columns))
			whole := FromElements(header.Elements(), nil)
			whole.Put(core.NewRawElement(core.TagPixelData, core.VROB, joint))
			var want, got bytes.Buffer
			if err := WriteFile(&want, &File{Dataset: whole, TransferSyntax: transfer.ExplicitVRLittleEndian}); err != nil {
				t.Fatal(err)
			}
			if err := WriteFileFrames(&got, &File{Dataset: header, TransferSyntax: transfer.ExplicitVRLittleEndian}, source, FrameWriteOptions{}); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Bytes(), want.Bytes()) || len(source.reads) != count {
				t.Fatal("continuous bitstream differs from whole Pixel Data or pulled a frame twice")
			}
		})
	}
}

func TestWriteFilePackedBitFramesRefusesInvalidPayload(t *testing.T) {
	encapsulated := &File{Dataset: FromElements(frameTestDataSet(1, 1), nil), TransferSyntax: transfer.JPEGBaseline}
	if err := WriteFileFrames(io.Discard, encapsulated, &packedTestFrames{memoryFrames: memoryFrames{frames: [][]byte{{1}}}, bits: 3}, FrameWriteOptions{}); !errors.Is(err, ErrFrameSource) {
		t.Fatalf("encapsulated packed source: %v", err)
	}
	for _, tc := range []struct {
		name  string
		bits  int64
		data  byte
		short bool
	}{{"wrong bits", 2, 1, false}, {"unused bits", 3, 128, false}, {"short source", 3, 1, true}} {
		t.Run(tc.name, func(t *testing.T) {
			source := &packedTestFrames{memoryFrames: memoryFrames{frames: [][]byte{{tc.data}}, short: tc.short}, bits: tc.bits}
			file := &File{Dataset: FromElements(frameTestDataSet(1, 1), nil), TransferSyntax: transfer.ExplicitVRLittleEndian}
			if err := WriteFileFrames(io.Discard, file, source, FrameWriteOptions{}); !errors.Is(err, ErrFrameSource) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}
