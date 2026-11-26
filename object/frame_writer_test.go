package object

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// memoryFrames is a FrameSource over in-memory frames that records the
// order it is read in.
type memoryFrames struct {
	frames [][]byte
	reads  []int
	short  bool
}

func (m *memoryFrames) Frames() int                      { return len(m.frames) }
func (m *memoryFrames) FrameLength(i int) (int64, error) { return int64(len(m.frames[i])), nil }
func (m *memoryFrames) WriteFrameTo(i int, w io.Writer) (int64, error) {
	m.reads = append(m.reads, i)
	data := m.frames[i]
	if m.short {
		data = data[:len(data)-1]
	}
	n, err := w.Write(data)
	return int64(n), err
}

func frameTestDataSet(frames int, bits uint16) []core.Element {
	us := func(tag core.Tag, v uint16) core.Element {
		b := make([]byte, 2)
		binary.LittleEndian.PutUint16(b, v)
		return core.NewRawElement(tag, core.VRUS, b)
	}
	return []core.Element{
		core.NewRawElement(core.NewTag(0x0008, 0x0016), core.VRUI, []byte("1.2.840.10008.5.1.4.1.1.7\x00")),
		core.NewRawElement(core.NewTag(0x0008, 0x0018), core.VRUI, []byte("1.2.826.0.1.3680043.10.1.1040\x00")),
		us(core.NewTag(0x0028, 0x0002), 1),
		core.NewRawElement(core.NewTag(0x0028, 0x0004), core.VRCS, []byte("MONOCHROME2 ")),
		core.NewRawElement(core.NewTag(0x0028, 0x0008), core.VRIS, []byte(strconv.Itoa(frames)+map[bool]string{true: " ", false: ""}[len(strconv.Itoa(frames))%2 == 1])),
		us(core.NewTag(0x0028, 0x0010), 1),
		us(core.NewTag(0x0028, 0x0011), 3),
		us(core.NewTag(0x0028, 0x0100), bits),
		us(core.NewTag(0x0028, 0x0101), bits),
		us(core.NewTag(0x0028, 0x0102), bits-1),
		us(core.NewTag(0x0028, 0x0103), 0),
		// An element after Pixel Data is written after the frames.
		core.NewRawElement(core.NewTag(0xfffc, 0xfffc), core.VROB, []byte{0, 0}),
	}
}

func writeBoth(t *testing.T, elements []core.Element, pixel core.Element, syntax transfer.Syntax, frames *memoryFrames, policy OffsetTablePolicy) (want, got []byte) {
	t.Helper()
	whole := FromElements(append(append([]core.Element(nil), elements...), pixel), nil)
	var a, b bytes.Buffer
	if err := WriteFile(&a, &File{Dataset: whole, TransferSyntax: syntax}); err != nil {
		t.Fatal(err)
	}
	// The frame writer ignores the data set's own Pixel Data.
	stale := FromElements(append(append([]core.Element(nil), elements...), core.NewRawElement(core.TagPixelData, pixel.VR(), []byte{1, 2})), nil)
	if err := WriteFileFrames(&b, &File{Dataset: stale, TransferSyntax: syntax}, frames, FrameWriteOptions{OffsetTable: policy}); err != nil {
		t.Fatal(err)
	}
	return a.Bytes(), b.Bytes()
}

func TestWriteFileFramesMatchesWriteFile(t *testing.T) {
	three := [][]byte{{1, 0, 2, 0, 3, 0}, {4, 0, 5, 0, 6, 0}, {7, 0, 8, 0, 9, 0}}
	odd := [][]byte{{1, 2, 3}}
	fragments := [][]byte{{0xff, 0xd8, 1, 0xff, 0xd9}, {0xff, 0xd8, 2, 3, 0xff, 0xd9}, {0xff, 0xd8, 0xff, 0xd9}}
	offsets := func(frames [][]byte, width int) []byte {
		out := make([]byte, 0, width*len(frames))
		var next uint64
		for _, f := range frames {
			b := make([]byte, width)
			if width == 4 {
				binary.LittleEndian.PutUint32(b, uint32(next))
			} else {
				binary.LittleEndian.PutUint64(b, next)
			}
			out = append(out, b...)
			next += 8 + uint64(len(f)+len(f)%2)
		}
		return out
	}
	lengths := make([]byte, 0, 8*len(fragments))
	for _, f := range fragments {
		lengths = binary.LittleEndian.AppendUint64(lengths, uint64(len(f)))
	}
	for _, c := range []struct {
		name     string
		elements []core.Element
		pixel    core.Element
		syntax   transfer.Syntax
		frames   [][]byte
		policy   OffsetTablePolicy
	}{
		{"native OW", frameTestDataSet(3, 16), core.NewRawElement(core.TagPixelData, core.VROW, bytes.Join(three, nil)), transfer.ExplicitVRLittleEndian, three, OffsetTableAuto},
		{"native odd implicit", frameTestDataSet(1, 8), core.NewRawElement(core.TagPixelData, core.VROB, []byte{1, 2, 3, 0}), transfer.ImplicitVRLittleEndian, odd, OffsetTableAuto},
		{"encapsulated BOT", frameTestDataSet(3, 8), core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB, Length: core.UndefinedLength, LengthSet: true},
			Value: core.FragmentSequence{OffsetTable: offsets(fragments, 4), Fragments: fragments}}, transfer.JPEGBaseline, fragments, OffsetTableAuto},
		{"encapsulated empty", frameTestDataSet(3, 8), core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB, Length: core.UndefinedLength, LengthSet: true},
			Value: core.FragmentSequence{Fragments: fragments}}, transfer.JPEGBaseline, fragments, OffsetTableEmpty},
		{"encapsulated EOT", append(frameTestDataSet(3, 8), core.NewRawElement(tagFrameExtendedOffsetTable, core.VROV, offsets(fragments, 8)), core.NewRawElement(tagFrameExtendedOffsetTableLengths, core.VROV, lengths)),
			core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB, Length: core.UndefinedLength, LengthSet: true},
				Value: core.FragmentSequence{Fragments: fragments}}, transfer.JPEGBaseline, fragments, OffsetTableExtended},
	} {
		t.Run(c.name, func(t *testing.T) {
			frames := &memoryFrames{frames: c.frames}
			want, got := writeBoth(t, c.elements, c.pixel, c.syntax, frames, c.policy)
			if !bytes.Equal(want, got) {
				t.Fatalf("WriteFileFrames differs from WriteFile:\n got %x\nwant %x", got, want)
			}
			if fmt.Sprint(frames.reads) != fmt.Sprint([]int{0, 1, 2}[:len(c.frames)]) {
				t.Fatalf("frames read in order %v", frames.reads)
			}
			back, err := ReadFile(bytes.NewReader(got))
			if err != nil {
				t.Fatalf("read back: %v", err)
			}
			if !back.Dataset.Has(core.NewTag(0xfffc, 0xfffc)) {
				t.Fatal("element after Pixel Data lost")
			}
		})
	}
}

func TestWriteFileFramesRejectsInconsistentFrames(t *testing.T) {
	file := &File{Dataset: FromElements(frameTestDataSet(2, 8), nil), TransferSyntax: transfer.ExplicitVRLittleEndian}
	for name, c := range map[string]struct {
		frames *memoryFrames
		opts   FrameWriteOptions
		syntax transfer.Syntax
	}{
		"frame count":        {&memoryFrames{frames: [][]byte{{1, 2, 3}}}, FrameWriteOptions{}, transfer.ExplicitVRLittleEndian},
		"native lengths":     {&memoryFrames{frames: [][]byte{{1, 2, 3}, {1, 2}}}, FrameWriteOptions{}, transfer.ExplicitVRLittleEndian},
		"short frame":        {&memoryFrames{frames: [][]byte{{1, 2, 3}, {4, 5, 6}}, short: true}, FrameWriteOptions{}, transfer.ExplicitVRLittleEndian},
		"short encapsulated": {&memoryFrames{frames: [][]byte{{1, 2, 3}, {4, 5, 6}}, short: true}, FrameWriteOptions{}, transfer.JPEGBaseline},
		"unknown policy":     {&memoryFrames{frames: [][]byte{{1, 2, 3}, {4, 5, 6}}}, FrameWriteOptions{OffsetTable: 9}, transfer.JPEGBaseline},
	} {
		file.TransferSyntax = c.syntax
		if err := WriteFileFrames(io.Discard, file, c.frames, c.opts); !errors.Is(err, ErrFrameSource) {
			t.Errorf("%s: %v, want ErrFrameSource", name, err)
		}
	}
}
