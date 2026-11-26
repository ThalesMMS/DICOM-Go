package personname_test

import (
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/encoding"
	"github.com/ThalesMMS/dicom-go/personname"
)

// The DCMTK dcmdata/tests/tspchrs.cc PN test includes a JIS X 0208 character
// whose second octet looks like a separator. Exercise all three PN
// separators as trail bytes, then parse each decoded value as components.
func TestDCMTKPersonNameMultibyteDelimiters(t *testing.T) {
	tests := []struct {
		name        string
		declaration []string
		wire        string
		wantValues  []string
		family      string
		given       string
	}{
		{
			name:        "JIS_X_0208",
			declaration: []string{"", "ISO 2022 IR 87"},
			wire:        "Example^First=\x1b$BK\\K^K=\x1b(B^\x1b$BK!\x1b(B=\\Example^Next==",
			wantValues:  []string{"Example^First=本凡暴^法=", "Example^Next=="},
			family:      "本凡暴",
			given:       "法",
		},
		{
			name:        "GBK",
			declaration: []string{"GBK"},
			wire:        "Example^First=\x81\x5c\x81\x5e^\x82\x5c\x82\x5e=\\Example^Next==",
			wantValues:  []string{"Example^First=乗乛^俓俕=", "Example^Next=="},
			family:      "乗乛",
			given:       "俓俕",
		},
		{
			name:        "GB18030",
			declaration: []string{"GB18030"},
			wire:        "Example^First=\x81\x5c\x81\x5e^\x82\x5c\x82\x5e=\\Example^Next==",
			wantValues:  []string{"Example^First=乗乛^俓俕=", "Example^Next=="},
			family:      "乗乛",
			given:       "俓俕",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			charset, err := encoding.ParseCharacterSet(test.declaration...)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := charset.DecodePersonName([]byte(test.wire))
			if want := strings.Join(test.wantValues, "\\"); err != nil || decoded != want {
				t.Fatalf("DecodePersonName() = %q, %v; want %q", decoded, err, want)
			}
			values := strings.Split(decoded, "\\")
			first, err := personname.Parse(values[0])
			if err != nil {
				t.Fatal(err)
			}
			if got := first.Ideographic; got.FamilyName != test.family || got.GivenName != test.given || !first.Phonetic.IsEmpty() {
				t.Fatalf("first PN groups = ideographic %+v, phonetic %+v", got, first.Phonetic)
			}
			next, err := personname.Parse(values[1])
			if err != nil {
				t.Fatal(err)
			}
			if next.Alphabetic.FamilyName != "Example" || next.Alphabetic.GivenName != "Next" || !next.Ideographic.IsEmpty() || !next.Phonetic.IsEmpty() {
				t.Fatalf("second PN groups = %+v", next)
			}
			serialized := first.MustDCM() + "\\" + next.MustDCM()
			encoded, err := charset.EncodePersonName(serialized)
			if err != nil || string(encoded) != test.wire {
				t.Fatalf("EncodePersonName() = % X, %v; want % X", encoded, err, test.wire)
			}
		})
	}
}
