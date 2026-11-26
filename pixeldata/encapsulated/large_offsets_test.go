package encapsulated

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
)

type selectedFragmentReader struct {
	value  []byte
	offset int64
	reads  int
}

func (r *selectedFragmentReader) ReadAt(dst []byte, offset int64) (int, error) {
	r.reads++
	if offset != r.offset || len(dst) != len(r.value) {
		return 0, fmt.Errorf("unexpected read of %d bytes at offset %d", len(dst), offset)
	}
	return copy(dst, r.value), nil
}

func TestExtendedOffsetsAboveFourGiBReadOnlySelectedFragment(t *testing.T) {
	// The first two Items exist only as ranges. Together with their headers
	// they place the third Item beyond the 32-bit Basic Offset Table range.
	const earlierLength = 1<<31 - 2
	const valueStart = 373
	const step = earlierLength + 8
	const selectedOffset = 2 * step
	value := []byte{0xff, 0xd8, 0xff, 0xd9}
	items := []ItemRange{{valueStart, earlierLength}, {valueStart + step, earlierLength}, {valueStart + selectedOffset, uint64(len(value))}}
	for _, tc := range []struct {
		name   string
		offset uint64
		bytes  uint64
		err    error
	}{
		{"exact", selectedOffset, 2*earlierLength + 4, nil},
		{"truncated-to-32-bits", selectedOffset % (1 << 32), 2*earlierLength + 4, ErrLayout},
		{"aggregate-budget", selectedOffset, 2*earlierLength + 3, ErrResourceLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &selectedFragmentReader{value: value, offset: items[2].Offset}
			source, err := ReaderAt(reader, items)
			if err != nil {
				t.Fatal(err)
			}
			tables := Tables{
				ExtendedPresent: true,
				LengthsPresent:  true,
				Extended:        []uint64{0, step, tc.offset},
				Lengths:         []uint64{earlierLength, earlierLength, uint64(len(value))},
			}
			ctx := context.Background()
			plan, err := New(ctx, source, tables, len(items), JPEG, Limits{MaxBytes: tc.bytes, MaxFrameBytes: earlierLength})
			if !errors.Is(err, tc.err) || reader.reads != 0 {
				t.Fatalf("index error = %v (want %v), payload reads = %d", err, tc.err, reader.reads)
			}
			if tc.err != nil {
				if plan != nil {
					t.Fatal("index returned for a rejected table")
				}
				return
			}
			size, ok := plan.FrameLength(2)
			if !ok || size != uint64(len(value)) || reader.reads != 0 {
				t.Fatalf("selected frame length = %d, exists = %t, payload reads = %d", size, ok, reader.reads)
			}
			frame, err := plan.Frame(ctx, 2)
			if err != nil || reader.reads != 1 || frame.Borrowed || !bytes.Equal(frame.Data, value) {
				t.Fatalf("selected frame = %+v, error = %v, payload reads = %d", frame, err, reader.reads)
			}
		})
	}
}
