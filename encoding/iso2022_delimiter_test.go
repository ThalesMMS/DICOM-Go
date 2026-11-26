package encoding

import (
	"errors"
	"testing"
)

// TestISO2022YenSignIsNotWrittenAsDelimiter checks that YEN SIGN, which JIS
// X 0201 Romaji codes at 5C, is not written where 5C is the value delimiter
// (#2044), while a single-valued text VR keeps it.
func TestISO2022YenSignIsNotWrittenAsDelimiter(t *testing.T) {
	characterSet, err := ParseCharacterSet("ISO 2022 IR 13", "ISO 2022 IR 87")
	if err != nil {
		t.Fatal(err)
	}
	if encoded, err := characterSet.Encode("A¥B"); !errors.Is(err, ErrUnrepresentableCharacter) {
		t.Fatalf("Encode(A¥B) = % X, %v; want ErrUnrepresentableCharacter", encoded, err)
	}
	if encoded, err := characterSet.EncodePersonName("A¥B"); !errors.Is(err, ErrUnrepresentableCharacter) {
		t.Fatalf("EncodePersonName(A¥B) = % X, %v; want ErrUnrepresentableCharacter", encoded, err)
	}
	encoded, err := characterSet.EncodeSingleValue("A¥B")
	if err != nil || string(encoded) != "A\x5cB" {
		t.Fatalf("EncodeSingleValue(A¥B) = % X, %v; want 41 5C 42", encoded, err)
	}
	if decoded, err := characterSet.DecodeSingleValue(encoded); err != nil || decoded != "A¥B" {
		t.Fatalf("DecodeSingleValue = %q, %v; want A¥B", decoded, err)
	}
	// The delimiter itself still separates values.
	encoded, err = characterSet.Encode("A\\B")
	if err != nil || string(encoded) != "A\x5cB" {
		t.Fatalf("Encode(A\\B) = % X, %v", encoded, err)
	}
}

// TestISO2022PersonNameValuesStartAlphabetic checks that every value of a
// multi-valued PN starts in the alphabetic component group, where code
// extensions are not permitted (#2045).
func TestISO2022PersonNameValuesStartAlphabetic(t *testing.T) {
	characterSet, err := ParseCharacterSet("", "ISO 2022 IR 87")
	if err != nil {
		t.Fatal(err)
	}
	// 山田 is JIS X 0208 3B33 4544.
	const kanji = "\x1b$B;3ED\x1b(B"
	valid := "Yamada=" + kanji + "\\Suzuki=" + kanji
	decoded, err := characterSet.DecodePersonName([]byte(valid))
	if err != nil || decoded != "Yamada=山田\\Suzuki=山田" {
		t.Fatalf("DecodePersonName(valid) = %q, %v", decoded, err)
	}
	if encoded, err := characterSet.EncodePersonName("Yamada=山田\\Suzuki=山田"); err != nil || string(encoded) != valid {
		t.Fatalf("EncodePersonName(valid) = % X, %v", encoded, err)
	}

	var extension *CodeExtensionError
	if decoded, err := characterSet.DecodePersonName([]byte("Yamada=" + kanji + "\\" + kanji)); !errors.As(err, &extension) {
		t.Fatalf("DecodePersonName(escape in second alphabetic group) = %q, %v; want CodeExtensionError", decoded, err)
	}
	if encoded, err := characterSet.EncodePersonName("Yamada=山田\\山田"); !errors.Is(err, ErrUnrepresentableCharacter) {
		t.Fatalf("EncodePersonName(kanji in second alphabetic group) = % X, %v; want ErrUnrepresentableCharacter", encoded, err)
	}
}
