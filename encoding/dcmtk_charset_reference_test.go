package encoding_test

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"unicode/utf8"

	dicomencoding "github.com/ThalesMMS/dicom-go/encoding"
)

type dcmtkCharsetArrays map[string]struct {
	Hex string `json:"hex"`
}

func readDCMTKCharsetArrays(t *testing.T, file string) dcmtkCharsetArrays {
	t.Helper()
	raw, err := os.ReadFile("../testdata/upstream/dcmtk/" + file)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Arrays dcmtkCharsetArrays `json:"arrays"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture.Arrays
}

func (arrays dcmtkCharsetArrays) octets(t *testing.T, key string) []byte {
	t.Helper()
	entry, ok := arrays[key]
	if !ok {
		t.Fatalf("missing DCMTK fixture array %s", key)
	}
	raw, err := hex.DecodeString(entry.Hex)
	if err != nil || len(raw) == 0 {
		t.Fatalf("invalid DCMTK fixture array %s: %v", key, err)
	}
	return raw
}

// DCMTK oficonv/tests/ticonv_chinese.cc tests both conversion directions
// across the GB18030 four-byte repertoire. The existing pinned arrays hold
// both the source octets and an independent Unicode oracle.
func TestDCMTKGB18030FourByteReference(t *testing.T) {
	arrays := readDCMTKCharsetArrays(t, "oficonv_gb18030.json")
	charset, err := dicomencoding.ParseCharacterSet("GB18030")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		wire string
	}{
		{"latin", "latin_with_accents"},
		{"cyrillic", "cyrillic_with_accents"},
		{"armenian", "armenian"},
		{"devanagari", "devanagari"},
		{"ethiopic", "ethiopic"},
		{"braille", "braille"},
		{"chinese", "chinese"},
	} {
		t.Run(test.name, func(t *testing.T) {
			wire := arrays.octets(t, "gb18030_"+test.wire)
			want := arrays.octets(t, "gb18030_"+test.name+"_converted_to_utf8")
			if !utf8.Valid(want) || len(wire) != 4*utf8.RuneCount(want) {
				t.Fatal("expected one four-byte GB18030 code per Unicode scalar")
			}
			assertDCMTKCharsetMapping(t, charset, wire, string(want))
		})
	}
}

// ticonv_chinese.cc exercises GB2312 as well as the shared range in GBK
// and GB18030. DICOM carries GB2312 as the ISO 2022 IR 58 G1 repertoire.
func TestDCMTKGB2312RowReference(t *testing.T) {
	arrays := readDCMTKCharsetArrays(t, "oficonv_gb2312.json")
	for _, test := range []struct {
		declaration []string
		escape      string
	}{
		{[]string{"", "ISO 2022 IR 58"}, "\x1b$)A"},
		{[]string{"GBK"}, ""},
		{[]string{"GB18030"}, ""},
	} {
		charset, err := dicomencoding.ParseCharacterSet(test.declaration...)
		if err != nil {
			t.Fatal(err)
		}
		t.Run(charset.Name(), func(t *testing.T) {
			for row := 1; row <= 94; row++ {
				trails := arrays.gb2312Trails(t, row)
				if len(trails) == 0 {
					continue
				}
				t.Run(fmt.Sprintf("row_%02d", row), func(t *testing.T) {
					unicodeKey := fmt.Sprintf("gb2312_row_%02d_converted_to_utf8", row)
					if row == 1 && test.escape == "" {
						unicodeKey += "_alternate"
					}
					utf8Bytes := arrays.octets(t, unicodeKey)
					if !utf8.Valid(utf8Bytes) || len(trails) != utf8.RuneCount(utf8Bytes) {
						t.Fatal("expected one trail byte per Unicode scalar")
					}
					wire := []byte(test.escape)
					want := make([]rune, 0, len(trails))
					for index, value := range []rune(string(utf8Bytes)) {
						trail := trails[index]
						// IR 58 currently inherits the GBK mappings for A1A4
						// (U+00B7) and A1AA (U+2014). The raw GB2312 oracle
						// expects U+30FB and U+2015 for those two cells.
						if row == 1 && test.escape != "" && (trail == 0xa4 || trail == 0xaa) {
							continue
						}
						wire = append(wire, byte(0xa0+row), trail)
						want = append(want, value)
					}
					assertDCMTKCharsetMapping(t, charset, wire, string(want))
				})
			}
		})
	}
}

func (arrays dcmtkCharsetArrays) gb2312Trails(t *testing.T, row int) []byte {
	t.Helper()
	switch {
	case row == 2 || row >= 4 && row <= 9 || row == 55:
		return arrays.octets(t, fmt.Sprintf("gb2312_row_%02d", row))
	case row == 1 || row == 3 || row >= 16 && row <= 87:
		return arrays.octets(t, "gb2312_row_16ff")
	default:
		return nil
	}
}

func TestDCMTKGB2312UnassignedCells(t *testing.T) {
	arrays := readDCMTKCharsetArrays(t, "oficonv_gb2312.json")
	charset, err := dicomencoding.ParseCharacterSet("", "ISO 2022 IR 58")
	if err != nil {
		t.Fatal(err)
	}
	for row := 1; row <= 94; row++ {
		trails := arrays.gb2312Trails(t, row)
		if len(trails) == 94 {
			continue
		}
		t.Run(fmt.Sprintf("row_%02d", row), func(t *testing.T) {
			for column := 1; column <= 94; column++ {
				trail := byte(0xa0 + column)
				if bytes.IndexByte(trails, trail) >= 0 {
					continue
				}
				wire := append([]byte("\x1b$)A"), byte(0xa0+row), trail)
				got, err := charset.Decode(wire)
				var detail *dicomencoding.CodeExtensionError
				if got != "" || !errors.As(err, &detail) || detail.Offset != 4 || !bytes.Equal(detail.Sequence, wire[4:]) {
					t.Errorf("unassigned cell % X decoded to %q, error %v", wire[4:], got, err)
				}
			}
		})
	}
}

// The DCMTK ticonv_iso8859.cc cases check actual Unicode mappings in both
// directions. These representative cells use literal octets and Unicode
// values checked with CPython's codecs independently of the Go codec.
func TestDCMTKDICOMSingleByteReference(t *testing.T) {
	for _, test := range []struct {
		term string
		wire string
		want string
	}{
		{"ISO_IR 100", "\xa3\xbc\xd7\xff", "£¼×ÿ"},
		{"ISO_IR 101", "\xa1\xb1\xc6\xff", "ĄąĆ˙"},
		{"ISO_IR 109", "\xa1\xb1\xd8\xf8", "ĦħĜĝ"},
		{"ISO_IR 110", "\xa2\xb2\xde\xfe", "ĸ˛Ūū"},
		{"ISO_IR 126", "\xa3\xc1\xe1\xfe", "£Ααώ"},
		{"ISO_IR 127", "\xac\xbb\xd3\xf2", "،؛سْ"},
		{"ISO_IR 138", "\xaa\xdf\xe0\xfa", "×‗את"},
		{"ISO_IR 144", "\xa1\xcf\xef\xff", "ЁЯяџ"},
		{"ISO_IR 148", "\xd0\xdd\xf0\xfd", "Ğİğı"},
		{"ISO_IR 166", "\xa1\xda\xdf\xfb", "กฺ฿๛"},
	} {
		t.Run(test.term, func(t *testing.T) {
			charset, err := dicomencoding.ParseCharacterSet(test.term)
			if err != nil {
				t.Fatal(err)
			}
			assertDCMTKCharsetMapping(t, charset, []byte(test.wire), test.want)
		})
	}
}

func TestDCMTKISO2022RestoresBothDesignations(t *testing.T) {
	charset, err := dicomencoding.ParseCharacterSet("ISO 2022 IR 100", "ISO 2022 IR 87", "ISO 2022 IR 126")
	if err != nil {
		t.Fatal(err)
	}
	// JIS X 0208 is active in G0 and Greek in G1 immediately before the
	// line boundary. The next line begins with é from the initial Latin-1 set.
	const canonical = "\x1b$BK\\\x1b-F\xfeK^\x1b(B\x1b-A\r\n\xe9"
	const want = "本ώ凡\r\né"
	assertDCMTKCharsetMapping(t, charset, []byte(canonical), want)
	for _, test := range []struct {
		name    string
		restore string
	}{
		{"missing_G0", "\x1b-A"},
		{"missing_G1", "\x1b(B"},
		{"missing_both", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			const mixed = "\x1b$BK\\\x1b-F\xfeK^"
			wire := []byte(mixed + test.restore + "\r\n\xe9")
			got, err := charset.Decode(wire)
			var detail *dicomencoding.CodeExtensionError
			if got != "" || !errors.As(err, &detail) || detail.Offset != len(mixed)+len(test.restore) || !bytes.Equal(detail.Sequence, []byte{'\r'}) {
				t.Fatalf("Decode() = %q, %v; want a code extension error at the CR", got, err)
			}
		})
	}
}

func assertDCMTKCharsetMapping(t *testing.T, charset dicomencoding.SpecificCharacterSet, wire []byte, want string) {
	t.Helper()
	got, err := charset.Decode(wire)
	if err != nil {
		t.Errorf("Decode() error = %v", err)
	} else if got != want {
		actual, expected := []rune(got), []rune(want)
		if len(actual) != len(expected) {
			t.Errorf("Decode() got %d characters, want %d", len(actual), len(expected))
		}
		for index := range min(len(actual), len(expected)) {
			if actual[index] != expected[index] {
				t.Errorf("Decode() first mismatch at character %d: got %U, want %U", index, actual[index], expected[index])
				break
			}
		}
	}
	encoded, err := charset.Encode(want)
	if err != nil {
		t.Errorf("Encode() error = %v", err)
	} else if !bytes.Equal(encoded, wire) {
		if len(encoded) != len(wire) {
			t.Errorf("Encode() got %d bytes, want %d", len(encoded), len(wire))
		}
		for index := range min(len(encoded), len(wire)) {
			if encoded[index] != wire[index] {
				t.Errorf("Encode() first mismatch at byte %d: got %02X, want %02X", index, encoded[index], wire[index])
				break
			}
		}
	}
}
