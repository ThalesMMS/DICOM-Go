package ups

import (
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/net/dicomweb"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
)

// TestAETitleRulesFollowUL keeps the AE title checks of the packages that
// cannot import net/ul in step with ul.ValidateAETitle: a value as written in
// a data set or an HTTP parameter is valid when it is at most 16 bytes, has
// no NUL padding and ul.ValidateAETitle accepts it (PS3.5 Table 6.2-1).
func TestAETitleRulesFollowUL(t *testing.T) {
	for _, tc := range []struct {
		title string
		valid bool
	}{
		{"CT_SCP", true},
		{"PACS-01", true},
		{"ORTHANC.1", true},
		{"ct_scp", true},
		{"MY AE", true},
		{" LEADING", true},
		{"TRAILING ", true},
		{"0123456789ABCDEF", true},
		{"../PACS/X", true},
		{"A+B=C;D:E@F(G)!", true},
		{"", false},
		{"                ", false},
		{"0123456789ABCDEFG", false},
		{" 0123456789ABCDEF", false},
		{`CT\SCP`, false},
		{"CT\x00SCP", false},
		{"CTSCP\x00", false},
		{"CT\tSCP", false},
		{"CT\nSCP", false},
		{"CT\x1bSCP", false},
		{"CT\x7fSCP", false},
		{"CT\xc3\x89SCP", false},
	} {
		if got := len(tc.title) <= ul.MaxAETitleLength && !strings.ContainsRune(tc.title, 0) && ul.ValidateAETitle(tc.title) == nil; got != tc.valid {
			t.Fatalf("ul rule for %q = %t, want %t", tc.title, got, tc.valid)
		}
		if got := validAETitle(tc.title); got != tc.valid {
			t.Errorf("ups validAETitle(%q) = %t, want %t", tc.title, got, tc.valid)
		}
		if got := dicomweb.ValidAETitle(tc.title); got != tc.valid {
			t.Errorf("dicomweb.ValidAETitle(%q) = %t, want %t", tc.title, got, tc.valid)
		}
		if tc.title == "" {
			continue // an empty File Meta title means "not set"
		}
		if got := (object.FileMetaAETitles{Source: tc.title}).Validate() == nil; got != tc.valid {
			t.Errorf("object.FileMetaAETitles{Source: %q}.Validate() valid = %t, want %t", tc.title, got, tc.valid)
		}
	}
}
