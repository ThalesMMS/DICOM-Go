package dictionary_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary"
)

// FuzzParseDIC checks that arbitrary .dic text never panics or exceeds its
// limits, that tolerant mode only fails on resource limits, and that every
// accepted definition is reachable through the lookup it was indexed for.
// Persistent seeds live in testdata/fuzz/FuzzParseDIC.
func FuzzParseDIC(f *testing.F) {
	for _, seed := range []string{
		"# comment\n\n(0014,FFF0)\tUS\tA\t1\tSITE\n",
		"(60xx,0F00)\tUS\tPattern\t1\tSITE\n(5000-50FF,0F10)\txs\tRange\t2-2n\tSITE\n",
		"(0015-o-0019,0F20)\tSH\tOdd\t1-n\n(0014,F100-u-F10F)\tIS\tElems\t1-3\n",
		"(0019,\"ACME\",10)\tDS\tRel\t1\tS\n(0019,\"ACME\",1020)\tLO\tFixed\t1\tS\n(0021-o-0023,\"ACME\",30-u-3F)\tUL\tR\tn\n",
		"(FFFE,E0F0)\tna\tItemLike\t1\n(0014,FFF1)\tox\tO\t1\r\n(0014,FFF2)\tlt\tL\t1\n(0014,FFF3)\tup\tU\t1\n",
		"(0014,FFF0)\tUS\tRETIRED_Old\t1\tDICOM/retired\n(0014,FFF0)\tSS\tNew\t1\n",
		"(0019,\"unterminated,10)\tUS\tX\t1\n(0003,\"A\",10)\tUS\tX\t1\n(0014,FFF0)\tpi\tX\t1\n",
		"(6001-6001,0010)\tUS\tX\t1\n(0014,FFF0)\tUS\tX\t0\n(xxxx,xxxx)\tUN\tAll\t1\n",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<16 {
			return
		}
		limits := dictionary.DICOptions{MaxFileSize: 1 << 15, MaxLineLength: 256, MaxEntries: 256, MaxRepeating: 32, MaxSkipped: 64}
		strict, strictReport, strictErr := dictionary.ParseDIC(bytes.NewReader(data), limits)
		tolerantOpts := limits
		tolerantOpts.Tolerant = true
		tolerant, report, err := dictionary.ParseDIC(bytes.NewReader(data), tolerantOpts)
		if err != nil {
			if !errors.Is(err, dictionary.ErrDICResourceLimit) {
				t.Fatalf("tolerant mode failed on a non-resource error: %v", err)
			}
			return
		}
		if len(report.Skipped) > limits.MaxSkipped || report.Entries > limits.MaxEntries || tolerant.Len() > report.Entries {
			t.Fatalf("limits exceeded: %+v len %d", report, tolerant.Len())
		}
		for _, skipped := range report.Skipped {
			if skipped.Line < 1 || skipped.Line > report.Lines || skipped.Error() == "" {
				t.Fatalf("bad skip report %+v", skipped)
			}
		}
		if strictErr == nil {
			if len(report.Skipped) != 0 || strict.Len() != tolerant.Len() || strictReport.Entries != report.Entries {
				t.Fatalf("strict and tolerant disagree: %+v vs %+v", strictReport, report)
			}
		} else if len(report.Skipped) == 0 {
			t.Fatalf("strict failed (%v) but tolerant skipped nothing", strictErr)
		}
		for _, def := range tolerant.Entries() {
			if def.Line < 1 || def.Line > report.Lines {
				t.Fatalf("entry line %d outside 1..%d", def.Line, report.Lines)
			}
			if def.Creator == "" {
				if !def.Matches(def.Entry.Tag) {
					t.Fatalf("entry does not match its first tag: %+v", def)
				}
				if _, ok := tolerant.ByTag(def.Entry.Tag); !ok {
					t.Fatalf("ByTag lost %v", def.Entry.Tag)
				}
				if _, ok := tolerant.ByKeyword(def.Entry.Keyword); !ok {
					t.Fatalf("ByKeyword lost %q", def.Entry.Keyword)
				}
				continue
			}
			if !dictionary.IsPrivateGroup(def.Entry.Tag.Group) {
				t.Fatalf("private definition in group %04X", def.Entry.Tag.Group)
			}
			tag := def.Entry.Tag
			if !def.Fixed {
				tag = core.NewTag(tag.Group, 0x1000|tag.Element)
			}
			if !def.Matches(tag) {
				t.Fatalf("private entry does not match %v: %+v", tag, def)
			}
			if _, ok := dictionary.LookupScopedEntry(tolerant, tag, def.Creator); !ok {
				t.Fatalf("scoped lookup lost %v", tag)
			}
		}
	})
}
