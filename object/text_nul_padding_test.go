package object

import (
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
)

// TestGetStringTrimsTrailingNULPadding pins #2032: CS, LO, SH, DA, TM, PN, IS
// and DS values padded with one or more NULs read as the unpadded value, also
// under an ISO 2022 character set, where NUL is otherwise a forbidden control
// character. The raw bytes stay as read.
func TestGetStringTrimsTrailingNULPadding(t *testing.T) {
	tests := []struct {
		tag  core.Tag
		vr   core.VR
		raw  string
		want []string
	}{
		{core.NewTag(0x0008, 0x0060), core.VRCS, "CT\x00", []string{"CT"}},
		{core.NewTag(0x0008, 0x0008), core.VRCS, "ORIGINAL\x00\\PRIMARY \x00\x00", []string{"ORIGINAL", "PRIMARY"}},
		{core.NewTag(0x0008, 0x0080), core.VRLO, "General Hospital\x00\x00", []string{"General Hospital"}},
		{core.NewTag(0x0008, 0x0050), core.VRSH, "ACC1\x00 \x00", []string{"ACC1"}},
		{core.NewTag(0x0008, 0x0020), core.VRDA, "20260101\x00\x00", []string{"20260101"}},
		{core.NewTag(0x0008, 0x0030), core.VRTM, "101010\x00", []string{"101010"}},
		{core.NewTag(0x0010, 0x0010), core.VRPN, "Yamada^Tarou=\x1b$B;3ED\x1b(B\x00\x00", []string{"Yamada^Tarou=山田"}},
		{core.NewTag(0x0028, 0x0008), core.VRIS, "12\x00", []string{"12"}},
		{core.NewTag(0x0018, 0x0050), core.VRDS, "1.5\x00\\2.5\x00", []string{"1.5", "2.5"}},
	}
	elements := []core.Element{core.NewRawElement(core.NewTag(0x0008, 0x0005), core.VRCS, []byte(`\ISO 2022 IR 87`))}
	for _, test := range tests {
		elements = append(elements, core.NewRawElement(test.tag, test.vr, []byte(test.raw)))
	}
	obj := FromElements(elements, std.Dictionary)
	for _, test := range tests {
		values, err := obj.LookupStrings(test.tag)
		if err != nil {
			t.Errorf("LookupStrings(%s %s) error = %v", test.tag, test.vr, err)
			continue
		}
		if len(values) != len(test.want) {
			t.Errorf("LookupStrings(%s %s) = %q, want %q", test.tag, test.vr, values, test.want)
			continue
		}
		for i := range values {
			if values[i] != test.want[i] {
				t.Errorf("LookupStrings(%s %s) = %q, want %q", test.tag, test.vr, values, test.want)
				break
			}
		}
		if value, ok := obj.GetString(test.tag); !ok || value != test.want[0] {
			t.Errorf("GetString(%s %s) = %q, %t; want %q", test.tag, test.vr, value, ok, test.want[0])
		}
		if raw, ok := obj.GetRaw(test.tag); !ok || string(raw) != test.raw {
			t.Errorf("GetRaw(%s %s) = %q, want the original %q", test.tag, test.vr, raw, test.raw)
		}
	}
}
