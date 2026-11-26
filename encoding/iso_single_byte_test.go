package encoding

import (
	"errors"
	"testing"
)

// ISO_IR 148 and 166 follow ISO 8859-9 and TIS 620, not the Windows code
// pages the WHATWG index substitutes for them.
func TestISOSingleByteRepertoires(t *testing.T) {
	cases := []struct {
		term            string
		text            string
		encoded         []byte
		unrepresentable []string
		invalid         []byte
	}{
		{"ISO_IR 148", "Ğüş İı", []byte{0xD0, 0xFC, 0xFE, 0x20, 0xDD, 0xFD}, []string{"Œ", "Š", "€", "™", "“", "…"}, nil},
		{"ISO_IR 166", "ภาษาไทย", []byte{0xC0, 0xD2, 0xC9, 0xD2, 0xE4, 0xB7, 0xC2}, []string{"€", "…", "‘"}, []byte{0xDB}},
	}
	for _, c := range cases {
		charset, err := ParseCharacterSet(c.term)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := charset.Encode(c.text)
		if err != nil || string(encoded) != string(c.encoded) {
			t.Fatalf("%s: encode %q = % X, %v", c.term, c.text, encoded, err)
		}
		if decoded, err := charset.Decode(encoded); err != nil || decoded != c.text {
			t.Fatalf("%s: decode %q, %v", c.term, decoded, err)
		}
		for _, text := range c.unrepresentable {
			if _, err := charset.Encode(text); !errors.Is(err, ErrUnrepresentableCharacter) {
				t.Errorf("%s: %q encoded: %v", c.term, text, err)
			}
		}
		// C1 bytes are C1 controls, as in ISO/IEC 8859.
		if decoded, err := charset.Decode([]byte{0x80, 0x9F}); err != nil || decoded != "\u0080\u009f" {
			t.Errorf("%s: C1 decode %q, %v", c.term, decoded, err)
		}
		if len(c.invalid) > 0 {
			if _, err := charset.Decode(c.invalid); err == nil {
				t.Errorf("%s: undefined byte decoded", c.term)
			}
		}
	}
}
