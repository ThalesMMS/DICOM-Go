package valueencode

import (
	"encoding/binary"
	"github.com/ThalesMMS/dicom-go/core"
	"math"
	"testing"
)

func TestNumericLengthSharesEncodingGuard(t *testing.T) {
	values := []core.Value{core.Uint16Value{1}, core.Int16Value{-1}, core.Uint32Value{3}, core.Int32Value{-3}, core.Uint64Value{4}, core.Int64Value{-4}, core.Float32Value{float32(math.Inf(1)), float32(math.NaN())}, core.Float64Value{math.Inf(-1), math.NaN()}, core.TagValue{core.NewTag(1, 2)}}
	for _, value := range values {
		length, err := NumericLength(value)
		if err != nil {
			t.Fatal(err)
		}
		for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
			bytes, written, err := Numeric(value, order)
			if err != nil || int(length) != len(bytes) || written != length {
				t.Fatalf("%T length%d written%d err%v", value, length, written, err)
			}
		}
	}
	for _, c := range []struct{ n, width int }{{-1, 4}, {2, 0}, {math.MaxInt, 8}, {int(core.UndefinedLength/8) + 1, 8}} {
		if _, err := fixedLength(c.n, c.width); err == nil {
			t.Fatalf("invalid size admitted %+v", c)
		}
	}
}
