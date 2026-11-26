package object

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/transfer"
)

type singlePassFrames struct {
	values         [][]byte
	reads, lengths []int
}

func (s *singlePassFrames) Frames() int { return len(s.values) }
func (s *singlePassFrames) FrameLength(i int) (int64, error) {
	s.lengths[i]++
	return int64(len(s.values[i])), nil
}
func (s *singlePassFrames) WriteFrameTo(i int, w io.Writer) (int64, error) {
	s.reads[i]++
	if s.reads[i] != 1 {
		return 0, fmt.Errorf("frame replay is forbidden")
	}
	n, err := w.Write(s.values[i])
	return int64(n), err
}

func TestFrameGroupLengthDoesNotReplaySource(t *testing.T) {
	for _, c := range []struct {
		name      string
		syntax    transfer.Syntax
		frames    [][]byte
		policy    OffsetTablePolicy
		cap       int64
		fragments int
	}{
		{"native odd", transfer.ExplicitVRLittleEndian, [][]byte{{1, 2, 3}, {4, 5, 6}, {7, 8, 9}}, OffsetTableAuto, 0, 0},
		{"BOT", transfer.JPEGBaseline, [][]byte{{1, 2, 3, 4, 5}, {6, 7, 8, 9, 10, 11}}, OffsetTableBasic, 0, 2},
		{"empty", transfer.JPEGBaseline, [][]byte{{1, 2, 3, 4, 5}, {6, 7, 8, 9, 10, 11}}, OffsetTableEmpty, 0, 2},
		{"EOT", transfer.JPEGBaseline, [][]byte{{1, 2, 3, 4, 5}, {6, 7, 8, 9, 10, 11}}, OffsetTableExtended, 0, 2},
		{"split final odd", transfer.JPEG2000LosslessOnly, [][]byte{{1, 2, 3, 4, 5}, {6, 7, 8, 9, 10, 11, 12}}, OffsetTableEmpty, 4, 4},
		{"split cap two", transfer.JPEG2000LosslessOnly, [][]byte{{1, 2, 3, 4}}, OffsetTableEmpty, 2, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, policy := range []GroupLengthPolicy{GroupLengthCreate, GroupLengthRecalculate} {
				elements := frameTestDataSet(len(c.frames), 8)
				if policy == GroupLengthRecalculate {
					elements = append(elements, core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x7fe0, 0), VR: core.VRUL}, Value: core.Uint32Value{999}})
				}
				source := &singlePassFrames{values: c.frames, reads: make([]int, len(c.frames)), lengths: make([]int, len(c.frames))}
				var out bytes.Buffer
				err := WriteFileFrames(&out, &File{Dataset: FromElements(elements, nil), TransferSyntax: c.syntax}, source, FrameWriteOptions{WriteFileOptions: WriteFileOptions{GroupLength: policy, SequenceLength: SequenceLengthDefined}, OffsetTable: c.policy, MaxFragmentBytes: c.cap})
				if err != nil {
					t.Fatal(err)
				}
				for i := range c.frames {
					if source.reads[i] != 1 || source.lengths[i] != 1 {
						t.Fatalf("frame %d: reads=%d metadata=%d", i, source.reads[i], source.lengths[i])
					}
				}
				back, err := ReadFile(bytes.NewReader(out.Bytes()))
				if err != nil {
					t.Fatal(err)
				}
				gl, err := back.Dataset.GetInt(core.NewTag(0x7fe0, 0))
				if err != nil {
					t.Fatal(err)
				}
				var want int64
				if c.syntax.Encapsulated {
					lengths := make([]int64, len(c.frames))
					for i, f := range c.frames {
						lengths[i] = int64(len(f))
					}
					bot := 0
					if c.policy == OffsetTableBasic {
						bot = 4 * len(c.frames)
					}
					value, err := frameEncodedValueLength(lengths, bot, c.fragments)
					if err != nil {
						t.Fatal(err)
					}
					want = 12 + value
					if c.policy == OffsetTableExtended {
						want += 2 * (12 + 8*int64(len(c.frames)))
					}
				} else {
					for _, f := range c.frames {
						want += int64(len(f))
					}
					want += want % 2
					want += 12
				}
				if int64(gl) != want {
					t.Fatalf("Group Length=%d want %d", gl, want)
				}
			}
		})
	}
}

func TestFrameEncodedLengthGuardsWithoutLargeAllocation(t *testing.T) {
	for _, c := range []struct {
		lengths        []int64
		bot, fragments int
	}{
		{nil, -1, 0}, {nil, 0, -1}, {[]int64{-1}, 0, 1}, {[]int64{math.MaxInt64}, 0, 1}, {[]int64{math.MaxInt64 - 1}, 0, 1},
	} {
		if _, err := frameEncodedValueLength(c.lengths, c.bot, c.fragments); !errors.Is(err, ErrFrameSource) {
			t.Fatalf("%+v: %v", c, err)
		}
	}
	got, err := frameEncodedValueLength([]int64{3, 4}, 8, 2)
	if err != nil || got != 48 {
		t.Fatalf("length=%d err=%v", got, err)
	}
}
