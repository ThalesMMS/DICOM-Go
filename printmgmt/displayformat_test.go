package printmgmt

import (
	"errors"
	"math"
	"strconv"
	"testing"
)

// STANDARD\C,R is parsed with each count bounded before the product is formed,
// so layouts whose product wraps int (64-bit or 32-bit) are refused with 0106H.
func TestParseDisplayFormatBounds(t *testing.T) {
	maxInt := strconv.Itoa(math.MaxInt)
	tests := []struct {
		name       string
		value      string
		maxBoxes   int
		wantColumn int
		wantRow    int
	}{
		{name: "1x1", value: `STANDARD\1,1`, maxBoxes: 64, wantColumn: 1, wantRow: 1},
		{name: "exact limit", value: `STANDARD\8,8`, maxBoxes: 64, wantColumn: 8, wantRow: 8},
		{name: "exact limit one column", value: `STANDARD\64,1`, maxBoxes: 64, wantColumn: 64, wantRow: 1},
		{name: "exact limit one row", value: `STANDARD\1,64`, maxBoxes: 64, wantColumn: 1, wantRow: 64},
		{name: "padded and leading zeros", value: `STANDARD\ 02 , 003 `, maxBoxes: 64, wantColumn: 2, wantRow: 3},
		{name: "ceiling", value: `STANDARD\64,64`, maxBoxes: MaxImageBoxesCeiling, wantColumn: 64, wantRow: 64},
		{name: "max int limit", value: `STANDARD\` + maxInt + `,1`, maxBoxes: math.MaxInt, wantColumn: math.MaxInt, wantRow: 1},

		{name: "one over limit", value: `STANDARD\13,5`, maxBoxes: 64},
		{name: "column over limit", value: `STANDARD\65,1`, maxBoxes: 64},
		{name: "row over limit", value: `STANDARD\1,65`, maxBoxes: 64},
		{name: "64-bit wrap to zero", value: `STANDARD\4611686018427387904,4`, maxBoxes: 64},
		{name: "64-bit wrap to zero under max int limit", value: `STANDARD\4611686018427387904,4`, maxBoxes: math.MaxInt},
		{name: "64-bit wrap to small", value: `STANDARD\4611686018427387905,4`, maxBoxes: 64},
		{name: "max int squared", value: `STANDARD\` + maxInt + `,` + maxInt, maxBoxes: math.MaxInt},
		{name: "max int by two", value: `STANDARD\` + maxInt + `,2`, maxBoxes: math.MaxInt},
		{name: "32-bit wrap to zero", value: `STANDARD\65536,65536`, maxBoxes: 64},
		{name: "32-bit wrap under max int32 limit", value: `STANDARD\65536,65536`, maxBoxes: math.MaxInt32},
		{name: "32-bit wrap to small", value: `STANDARD\65537,65535`, maxBoxes: 64},
		{name: "above uint64", value: `STANDARD\99999999999999999999999999,1`, maxBoxes: math.MaxInt},
		{name: "zero columns", value: `STANDARD\0,4`, maxBoxes: 64},
		{name: "zero rows", value: `STANDARD\4,0`, maxBoxes: 64},
		{name: "negative columns", value: `STANDARD\-1,4`, maxBoxes: 64},
		{name: "negative both", value: `STANDARD\-8,-8`, maxBoxes: 64},
		{name: "min int", value: `STANDARD\` + strconv.Itoa(math.MinInt) + `,-1`, maxBoxes: 64},
		{name: "plus sign", value: `STANDARD\+2,2`, maxBoxes: 64},
		{name: "non-numeric", value: `STANDARD\A,B`, maxBoxes: 64},
		{name: "hex", value: `STANDARD\0x2,2`, maxBoxes: 64},
		{name: "fraction", value: `STANDARD\1.5,2`, maxBoxes: 64},
		{name: "empty columns", value: `STANDARD\,2`, maxBoxes: 64},
		{name: "empty rows", value: `STANDARD\2,`, maxBoxes: 64},
		{name: "no comma", value: `STANDARD\4`, maxBoxes: 64},
		{name: "three counts", value: `STANDARD\1,2,3`, maxBoxes: 64},
		{name: "no layout", value: `STANDARD`, maxBoxes: 64},
		{name: "empty", value: ``, maxBoxes: 64},
		{name: "ROW", value: `ROW\2,1`, maxBoxes: 64},
		{name: "COL", value: `COL\2,1`, maxBoxes: 64},
		{name: "SLIDE", value: `SLIDE`, maxBoxes: 64},
		{name: "SUPERSLIDE", value: `SUPERSLIDE`, maxBoxes: 64},
		{name: "CUSTOM", value: `CUSTOM\1`, maxBoxes: 64},
		{name: "lower case", value: `standard\1,1`, maxBoxes: 64},
		{name: "zero limit", value: `STANDARD\1,1`, maxBoxes: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			columns, rows, err := parseDisplayFormat(test.value, test.maxBoxes)
			if test.wantColumn == 0 {
				var statusErr *StatusError
				if !errors.As(err, &statusErr) || statusErr.Status != StatusInvalidAttributeValue || len(statusErr.Tags) != 1 || statusErr.Tags[0] != TagImageDisplayFormat {
					t.Fatalf("parseDisplayFormat(%q, %d) = %d, %d, %v; want 0106H on Image Display Format", test.value, test.maxBoxes, columns, rows, err)
				}
				return
			}
			if err != nil || columns != test.wantColumn || rows != test.wantRow {
				t.Fatalf("parseDisplayFormat(%q, %d) = %d, %d, %v; want %d, %d", test.value, test.maxBoxes, columns, rows, err, test.wantColumn, test.wantRow)
			}
		})
	}
}

func TestBoxCountWithin(t *testing.T) {
	tests := []struct {
		columns, rows, maxBoxes int
		want                    bool
	}{
		{1, 1, 1, true},
		{8, 8, 64, true},
		{8, 9, 64, false},
		{math.MaxInt/2 + 1, 4, math.MaxInt, false}, // 2^62 (2^30 on 32-bit) x 4 wraps to 0
		{math.MaxInt, 1, math.MaxInt, true},
		{math.MaxInt, 2, math.MaxInt, false},
		{math.MaxInt, math.MaxInt, math.MaxInt, false},
		{1 << 16, 1 << 16, math.MaxInt32, false},
		{1 << 15, 1 << 16, math.MaxInt32, false},
		{0, 1, 64, false},
		{1, 0, 64, false},
		{-1, -1, 64, false},
		{math.MinInt, -1, 64, false},
		{1, 1, 0, false},
	}
	for _, test := range tests {
		if got := boxCountWithin(test.columns, test.rows, test.maxBoxes); got != test.want {
			t.Errorf("boxCountWithin(%d, %d, %d) = %v, want %v", test.columns, test.rows, test.maxBoxes, got, test.want)
		}
	}
}
