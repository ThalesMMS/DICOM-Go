package qrmatch

import (
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/tags"
	"github.com/ThalesMMS/dicom-go/personname/fuzzy"
)

func TestMatchFuzzyPersonNames(t *testing.T) {
	tests := []struct {
		name      string
		encoder   fuzzy.FuzzyStr
		tag       core.Tag
		vr        core.VR
		query     string
		candidate string
		want      bool
	}{
		{"disabled is literal", nil, tags.PatientName, core.VRPN, "SMYTH^JON", "SMITH^JOHN", false},
		{"soundex patient name", fuzzy.Soundex{}, tags.PatientName, core.VRPN, "SMYTH^JON", "SMITH^JOHN", true},
		{"soundex referring physician", fuzzy.Soundex{}, tags.ReferringPhysicianName, core.VRPN, "JONSON", "JOHNSON^ANNA", true},
		{"literal still matches", fuzzy.Soundex{}, tags.PatientName, core.VRPN, "smith^john", "SMITH^JOHN", true},
		{"phonetic miss", fuzzy.Soundex{}, tags.PatientName, core.VRPN, "BROWN", "SMITH^JOHN", false},
		{"wild card stays literal", fuzzy.Soundex{}, tags.PatientName, core.VRPN, "SMYTH*", "SMITH^JOHN", false},
		{"wild card literal hit", fuzzy.Soundex{}, tags.PatientName, core.VRPN, "SMI*", "SMITH^JOHN", true},
		{"metaphone", fuzzy.Metaphone{}, tags.PatientName, core.VRPN, "NIGHT", "KNIGHT^ALAN", true},
		{"koelner", fuzzy.KoelnerPhonetik{}, tags.PatientName, core.VRPN, "MAYR", "MEIER^HANS", true},
		{"non-PN key stays literal", fuzzy.Soundex{}, tags.PatientID, core.VRLO, "SMYTH", "SMITH", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			query := dataset(str(tc.tag, tc.vr, tc.query))
			candidate := dataset(str(tc.tag, tc.vr, tc.candidate))
			result, err := Match(query, candidate, Options{FuzzyPersonNames: tc.encoder})
			if err != nil {
				t.Fatal(err)
			}
			if result.Match != tc.want {
				t.Fatalf("Match(%q, %q) = %v, want %v", tc.query, tc.candidate, result.Match, tc.want)
			}
		})
	}
}
