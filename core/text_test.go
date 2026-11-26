package core

import "testing"

func TestCleanTextValue(t *testing.T) {
	tests := []struct {
		name  string
		vr    VR
		value string
		want  string
	}{
		{name: "removes NUL bytes and trims whitespace", vr: VRLO, value: " \x00 General Hospital \x00 ", want: "General Hospital"},
		{name: "keeps interior spacing", vr: VRLO, value: "LEFT  RIGHT  ", want: "LEFT  RIGHT"},
		{name: "cleans person name without changing separators", vr: VRPN, value: "\x00DOE^JANE \x00", want: "DOE^JANE"},
		{name: "uses UID padding rules", vr: VRUI, value: " 1.2.3\x00 ", want: "1.2.3"},
		{name: "empty", vr: VRLO, value: " \x00 ", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CleanTextValue(tt.vr, tt.value); got != tt.want {
				t.Fatalf("CleanTextValue(%s, %q) = %q, want %q", tt.vr, tt.value, got, tt.want)
			}
		})
	}
}

// TestTrimTextValueTrailingNULPadding pins #2032: one or more trailing NULs,
// alone or mixed with spaces, are padding in every text VR, not only UI.
func TestTrimTextValueTrailingNULPadding(t *testing.T) {
	vrs := []VR{VRAE, VRAS, VRCS, VRDA, VRDS, VRDT, VRIS, VRLO, VRLT, VRPN, VRSH, VRST, VRTM, VRUC, VRUR, VRUT}
	tests := []struct {
		value string
		want  string
	}{
		{value: "CT\x00", want: "CT"},
		{value: "CT\x00\x00\x00", want: "CT"},
		{value: "CT \x00", want: "CT"},
		{value: "CT\x00 ", want: "CT"},
		{value: "\x00 \x00", want: ""},
		{value: "  CT\x00", want: "  CT"},
		{value: "\x00CT", want: "\x00CT"},
		{value: "C\x00T\x00", want: "C\x00T"},
	}
	for _, vr := range vrs {
		for _, tt := range tests {
			if got := TrimTextValue(vr, tt.value); got != tt.want {
				t.Errorf("TrimTextValue(%s, %q) = %q, want %q", vr, tt.value, got, tt.want)
			}
			if got := string(TrimTextValueBytes(vr, []byte(tt.value))); got != tt.want {
				t.Errorf("TrimTextValueBytes(%s, %q) = %q, want %q", vr, tt.value, got, tt.want)
			}
		}
	}
	if got := SplitTextMultiplicity(VRCS, "CT\x00\\MR \x00\x00"); len(got) != 2 || got[0] != "CT" || got[1] != "MR" {
		t.Errorf("SplitTextMultiplicity(CS) = %q, want [CT MR]", got)
	}

	raw := []byte("PATIENT\x00")
	element := NewRawElement(NewTag(0x0004, 0x1430), VRCS, raw)
	if got := element.StringValue(); got != "PATIENT" {
		t.Errorf("StringValue() = %q, want PATIENT", got)
	}
	if got := element.StringValues(); len(got) != 1 || got[0] != "PATIENT" {
		t.Errorf("StringValues() = %q, want [PATIENT]", got)
	}
	if got, _ := element.RawBytes(); string(got) != "PATIENT\x00" {
		t.Errorf("RawBytes() = %q, want the original NUL-padded bytes", got)
	}
}

func TestNormalizeUID(t *testing.T) {
	tests := []struct {
		name string
		uid  string
		want string
	}{
		{name: "trailing space and NUL padding", uid: "1.2.3 \x00", want: "1.2.3"},
		{name: "mixed trailing padding", uid: "1.2.3\x00 \x00 ", want: "1.2.3"},
		{name: "blank padding only", uid: " \x00", want: ""},
		{name: "leading padding is preserved", uid: " 1.2.3", want: " 1.2.3"},
		{name: "interior padding bytes are preserved", uid: "1.2\x003 4", want: "1.2\x003 4"},
		{name: "empty", uid: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeUID(tt.uid); got != tt.want {
				t.Fatalf("NormalizeUID(%q) = %q, want %q", tt.uid, got, tt.want)
			}
		})
	}
}
