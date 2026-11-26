package printmgmt

import (
	"fmt"
	"testing"
)

func TestDisplayFormatRejectsOverflow(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	for _, layout := range []string{
		fmt.Sprintf(`STANDARD\%d,2`, maxInt),
		fmt.Sprintf(`STANDARD\2,%d`, maxInt),
		`STANDARD\17,1`, `STANDARD\1,17`, `STANDARD\5,4`,
		`STANDARD\0,1`, `STANDARD\1,0`, `STANDARD\-1,2`,
	} {
		if _, _, err := parseDisplayFormat(layout, 16); err == nil {
			t.Errorf("accepted %s", layout)
		}
	}
	if columns, rows, err := parseDisplayFormat(`STANDARD\4,4`, 16); err != nil || columns != 4 || rows != 4 {
		t.Fatalf("valid layout: %d,%d, %v", columns, rows, err)
	}
}
