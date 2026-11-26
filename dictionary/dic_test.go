package dictionary_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func parseDIC(t *testing.T, src string, opts dictionary.DICOptions) (*dictionary.DICDictionary, dictionary.DICReport) {
	t.Helper()
	d, report, err := dictionary.ParseDIC(strings.NewReader(src), opts)
	if err != nil {
		t.Fatalf("ParseDIC: %v", err)
	}
	return d, report
}

func TestParseDICDocumentedFormat(t *testing.T) {
	src := strings.Join([]string{
		"# comment line",
		"   # indented comment",
		"",
		" \t ",
		"(0009,0000)\tUL\tAbsoluteGroupLength\t1\tSITE",
		"(0014,FFF0)\t\tUS\t\tThreeFieldsDefaultVM", // tab runs collapse; VM and version default
		"(0014,fff1)\tLO\tFourFields\t1-3\r",        // CRLF and lowercase hex
		"(0014,FFF2)\tFD\tRETIRED_OldThing\t3-3n\tSITE",
		"(0014,FFF3)\tCS\tRetiredByVersion\tn\tDICOM/retired",
		"(0014,FFF4)\tox\tContextOB\t1\tSITE",
		"(0014,FFF5)\tlt\tContextLUT\t1-n\tSITE",
		"(0014,FFF6)\tup\tContextOffset\t1\tSITE",
		"(0014,FFF7)\tpx\tContextPixels\t1\tSITE",
		"(FFFE,E0F0)\tna\tSyntheticDelimiter\t1\tSITE",
		"(60xx,0F00)\tUS\tPatternMarker\t1\tSITE",
		"(5000-50FF,0F10)\txs\tRangeMarker\t2\tSITE",
		"(0015-o-0019,0F20)\tSH\tOddRange\t1\tSITE",
		"(0014,F100-u-F10F)\tIS\tElementRange\t1\tSITE",
		"(0014,F200-F20F)\tIS\tEvenElementRange\t1\tSITE",
		"(0014,F3x0)\tDA\tNibblePattern\t1\tSITE",
		"(0019,\"ACME NDE 1.0\",10)\tDS\tProbeFrequency\t1\tSITE",
		"(0019, \" ACME NDE 1.0 \" ,1020)\tLO\tFixedCalibration\t1\tSITE",
	}, "\n")
	d, report := parseDIC(t, src, dictionary.DICOptions{})
	if report.Lines != 22 || report.Entries != 18 || report.Overridden != 0 || len(report.Skipped) != 0 || d.Len() != 18 {
		t.Fatalf("report %+v len %d", report, d.Len())
	}
	tests := []struct {
		tag     core.Tag
		keyword string
		vr      core.VR
		vm      string
		retired bool
	}{
		{core.NewTag(0x0009, 0x0000), "AbsoluteGroupLength", core.VRUL, "1", false},
		{core.NewTag(0x0014, 0xfff0), "ThreeFieldsDefaultVM", core.VRUS, "1", false},
		{core.NewTag(0x0014, 0xfff1), "FourFields", core.VRLO, "1-3", false},
		{core.NewTag(0x0014, 0xfff2), "OldThing", core.VRFD, "3-3n", true},
		{core.NewTag(0x0014, 0xfff3), "RetiredByVersion", core.VRCS, "n", true},
		{core.NewTag(0x0014, 0xfff4), "ContextOB", core.VROW, "1", false},
		{core.NewTag(0x0014, 0xfff5), "ContextLUT", core.VROW, "1-n", false},
		{core.NewTag(0x0014, 0xfff6), "ContextOffset", core.VRUL, "1", false},
		{core.NewTag(0x0014, 0xfff7), "ContextPixels", core.VROW, "1", false},
		{core.NewTag(0xfffe, 0xe0f0), "SyntheticDelimiter", "", "1", false},
		{core.NewTag(0x6000, 0x0f00), "PatternMarker", core.VRUS, "1", false},
		{core.NewTag(0x60fe, 0x0f00), "PatternMarker", core.VRUS, "1", false},
		{core.NewTag(0x5002, 0x0f10), "RangeMarker", core.VRUS, "2", false},
		{core.NewTag(0x0017, 0x0f20), "OddRange", core.VRSH, "1", false},
		{core.NewTag(0x0014, 0xf10f), "ElementRange", core.VRIS, "1", false},
		{core.NewTag(0x0014, 0xf20e), "EvenElementRange", core.VRIS, "1", false},
		{core.NewTag(0x0014, 0xf3a0), "NibblePattern", core.VRDA, "1", false},
	}
	for _, tc := range tests {
		e, ok := d.ByTag(tc.tag)
		if !ok || e.Tag != tc.tag || e.Keyword != tc.keyword || e.VR != tc.vr || e.VM != tc.vm || e.Retired != tc.retired {
			t.Errorf("ByTag(%v) = %+v, %v", tc.tag, e, ok)
		}
	}
	for _, tag := range []core.Tag{
		core.NewTag(0x6001, 0x0f00), // pattern groups are even
		core.NewTag(0x6100, 0x0f00),
		core.NewTag(0x5001, 0x0f10), // default range restriction is even
		core.NewTag(0x0016, 0x0f20), // -o- range
		core.NewTag(0x0014, 0xf20f), // default element range is even
		core.NewTag(0x0014, 0xf3a1), // nibble pattern keeps literal digits
		core.NewTag(0x0019, 0x1010), // private definitions never match by absolute tag
	} {
		if e, ok := d.ByTag(tag); ok {
			t.Errorf("ByTag(%v) matched %+v", tag, e)
		}
	}
	if e, ok := d.ByKeyword("patternmarker"); !ok || e.Tag != core.NewTag(0x6000, 0x0f00) || e.Name != "Pattern Marker" {
		t.Errorf("ByKeyword pattern: %+v %v", e, ok)
	}
	if e, ok := d.ByKeyword("OldThing"); !ok || !e.Retired || e.Name != "Old Thing" {
		t.Errorf("ByKeyword retired: %+v %v", e, ok)
	}
	if _, ok := d.ByKeyword("ProbeFrequency"); ok {
		t.Error("private keyword resolved without a scope")
	}
	specs := map[core.Tag]dictionary.ContextualVR{
		core.NewTag(0x0014, 0xfff4): dictionary.ContextualVROX,
		core.NewTag(0x0014, 0xfff5): dictionary.ContextualVRLT,
		core.NewTag(0x0014, 0xfff6): dictionary.ContextualVRUP,
		core.NewTag(0x0014, 0xfff7): dictionary.ContextualVRPX,
		core.NewTag(0x5010, 0x0f10): dictionary.ContextualVRXS,
	}
	for tag, want := range specs {
		spec, ok := d.VRSpecByTag(tag)
		if !ok || spec != dictionary.NewContextualVRSpec(want) {
			t.Errorf("VRSpecByTag(%v) = %v %v", tag, spec.Values(), ok)
		}
	}
	if spec, ok := d.VRSpecByTag(core.NewTag(0x0014, 0xfff1)); !ok || spec != dictionary.NewVRSpec(core.VRLO) {
		t.Errorf("exact spec %v", spec.Values())
	}
	if _, ok := d.VRSpecByTag(core.NewTag(0xfffe, 0xe0f0)); ok {
		t.Error("na entry reported a VR")
	}
	if got := dictionary.LookupVR(d, core.NewTag(0xfffe, 0xe0f0)); got != core.VRUN {
		t.Errorf("na LookupVR = %s", got)
	}

	entries := d.Entries()
	if len(entries) != 18 || entries[0].Line != 5 || entries[len(entries)-1].Creator != "ACME NDE 1.0" || !entries[len(entries)-1].Fixed {
		t.Fatalf("entries %+v", entries[len(entries)-1])
	}
	entries[0].Entry.VR = core.VRSQ
	if e, _ := d.ByTag(core.NewTag(0x0009, 0x0000)); e.VR != core.VRUL {
		t.Fatal("Entries exposed internal storage")
	}
	for _, e := range entries {
		if e.Line == 8 && (e.VMMin != 3 || e.VMMax != -1 || e.VMStep != 3) {
			t.Errorf("VM bounds %+v", e)
		}
		if e.Line == 7 && (e.VMMin != 1 || e.VMMax != 3 || e.VMStep != 1) {
			t.Errorf("VM range %+v", e)
		}
		if !e.Matches(e.Entry.Tag) && e.Creator == "" {
			t.Errorf("entry does not match its own tag: %+v", e)
		}
	}
}

func TestParseDICVMForms(t *testing.T) {
	for _, tc := range []struct {
		vm             string
		min, max, step int
	}{
		{"1", 1, 1, 1}, {"16", 16, 16, 1}, {"1-2", 1, 2, 1}, {"1-n", 1, -1, 1},
		{"2-2n", 2, -1, 2}, {"3-3n", 3, -1, 3}, {"n", 1, -1, 1}, {"2n", 2, -1, 1}, {"1-N", 1, -1, 1},
	} {
		d, _ := parseDIC(t, "(0014,FFF0)\tUS\tX\t"+tc.vm+"\tSITE\n", dictionary.DICOptions{})
		e := d.Entries()[0]
		if e.VMMin != tc.min || e.VMMax != tc.max || e.VMStep != tc.step || e.Entry.VM != tc.vm {
			t.Errorf("VM %q = %d %d %d", tc.vm, e.VMMin, e.VMMax, e.VMStep)
		}
	}
}

func TestParseDICLineErrors(t *testing.T) {
	cases := []struct {
		name, line, field string
		err               error
	}{
		{"too few fields", "(0014,FFF0)\tUS", "line", dictionary.ErrDICSyntax},
		{"too many fields", "(0014,FFF0)\tUS\tX\t1\tSITE\textra", "line", dictionary.ErrDICSyntax},
		{"missing parentheses", "0014,FFF0\tUS\tX\t1\tSITE", "tag", dictionary.ErrDICSyntax},
		{"missing element", "(0014)\tUS\tX\t1\tSITE", "tag", dictionary.ErrDICSyntax},
		{"three digit group", "(014,FFF0)\tUS\tX\t1\tSITE", "tag", dictionary.ErrDICSyntax},
		{"non hex", "(0014,FFG0)\tUS\tX\t1\tSITE", "tag", dictionary.ErrDICSyntax},
		{"two digit public element", "(0014,F0)\tUS\tX\t1\tSITE", "tag", dictionary.ErrDICSyntax},
		{"descending range", "(6010-6000,0010)\tUS\tX\t1\tSITE", "tag", dictionary.ErrDICSyntax},
		{"unknown restrictor", "(6000-q-6010,0010)\tUS\tX\t1\tSITE", "tag", dictionary.ErrDICSyntax},
		{"mixed widths", "(0019,\"A\",10-1010)\tUS\tX\t1\tSITE", "tag", dictionary.ErrDICSyntax},
		{"empty range", "(6001-6001,0010)\tUS\tX\t1\tSITE", "tag", dictionary.ErrDICSyntax},
		{"pattern with range", "(60xx-61xx,0010)\tUS\tX\t1\tSITE", "tag", dictionary.ErrDICSyntax},
		{"short pattern", "(6xx,0010)\tUS\tX\t1\tSITE", "tag", dictionary.ErrDICSyntax},
		{"trailing garbage", "(0014,FFF0)x\tUS\tX\t1\tSITE", "tag", dictionary.ErrDICSyntax},
		{"unterminated creator", "(0019,\"ACME,10)\tUS\tX\t1\tSITE", "tag", dictionary.ErrDICSyntax},
		{"creator without element", "(0019,\"ACME\")\tUS\tX\t1\tSITE", "tag", dictionary.ErrDICSyntax},
		{"empty creator", "(0019,\"  \",10)\tUS\tX\t1\tSITE", "tag", dictionary.ErrDICSyntax},
		{"backslash creator", "(0019,\"A\\B\",10)\tUS\tX\t1\tSITE", "tag", dictionary.ErrDICSyntax},
		{"even private group", "(0018,\"ACME\",10)\tUS\tX\t1\tSITE", "tag", dictionary.ErrDICSyntax},
		{"forbidden private group", "(0003,\"ACME\",10)\tUS\tX\t1\tSITE", "tag", dictionary.ErrDICSyntax},
		{"unrestricted private range", "(0011-u-0015,\"ACME\",10)\tUS\tX\t1\tSITE", "tag", dictionary.ErrDICSyntax},
		{"private pattern group", "(00x9,\"ACME\",10)\tUS\tX\t1\tSITE", "tag", dictionary.ErrDICSyntax},
		{"private creator element", "(0019,\"ACME\",0100)\tUS\tX\t1\tSITE", "tag", dictionary.ErrDICSyntax},
		{"private range crossing", "(0019,\"ACME\",00F0-1010)\tUS\tX\t1\tSITE", "tag", dictionary.ErrDICSyntax},
		{"lowercase VR", "(0014,FFF0)\tus\tX\t1\tSITE", "vr", dictionary.ErrDICVR},
		{"internal VR", "(0014,FFF0)\tpi\tX\t1\tSITE", "vr", dictionary.ErrDICVR},
		{"unknown VR", "(0014,FFF0)\t??\tX\t1\tSITE", "vr", dictionary.ErrDICVR},
		{"na outside items", "(0014,FFF0)\tna\tX\t1\tSITE", "vr", dictionary.ErrDICVR},
		{"zero VM", "(0014,FFF0)\tUS\tX\t0\tSITE", "vm", dictionary.ErrDICSyntax},
		{"descending VM", "(0014,FFF0)\tUS\tX\t3-2\tSITE", "vm", dictionary.ErrDICSyntax},
		{"bad VM", "(0014,FFF0)\tUS\tX\t1-m\tSITE", "vm", dictionary.ErrDICSyntax},
		{"huge VM", "(0014,FFF0)\tUS\tX\t99999999999\tSITE", "vm", dictionary.ErrDICSyntax},
		{"control byte", "(0014,FFF0)\tUS\tX\x01\t1\tSITE", "line", dictionary.ErrDICSyntax},
		{"non ASCII", "(0014,FFF0)\tUS\tXé\t1\tSITE", "line", dictionary.ErrDICSyntax},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := "# header\n\n(0014,FFF1)\tUS\tGood\t1\tSITE\n" + tc.line + "\n"
			_, report, err := dictionary.ParseDIC(strings.NewReader(src), dictionary.DICOptions{})
			var lineErr *dictionary.DICLineError
			if !errors.As(err, &lineErr) || !errors.Is(err, tc.err) || lineErr.Line != 4 || lineErr.Field != tc.field {
				t.Fatalf("err = %v (%+v)", err, lineErr)
			}
			if !strings.Contains(err.Error(), "line 4") || report.Lines != 4 {
				t.Fatalf("message %q report %+v", err, report)
			}
			d, report, err := dictionary.ParseDIC(strings.NewReader(src), dictionary.DICOptions{Tolerant: true})
			if err != nil || d.Len() != 1 || len(report.Skipped) != 1 || report.Skipped[0].Line != 4 || !errors.Is(&report.Skipped[0], tc.err) {
				t.Fatalf("tolerant: %v %+v", err, report)
			}
		})
	}
}

func TestParseDICTolerantReportsAndBoundsSkips(t *testing.T) {
	src := "(0014,FFF0)\tUS\tA\t1\tSITE\nbad\n(0014,FFF1)\tXX\tB\t1\tSITE\n(0014,FFF2)\tUS\tC\t1\tSITE\n"
	d, report := parseDIC(t, src, dictionary.DICOptions{Tolerant: true})
	if d.Len() != 2 || report.Entries != 2 || len(report.Skipped) != 2 || report.Skipped[0].Line != 2 || report.Skipped[1].Line != 3 || report.Skipped[1].Field != "vr" {
		t.Fatalf("report %+v", report)
	}
	_, _, err := dictionary.ParseDIC(strings.NewReader(src), dictionary.DICOptions{Tolerant: true, MaxSkipped: 1})
	var lineErr *dictionary.DICLineError
	if !errors.Is(err, dictionary.ErrDICResourceLimit) || !errors.As(err, &lineErr) || lineErr.Line != 3 {
		t.Fatalf("skip budget: %v", err)
	}
}

func TestParseDICResourceLimits(t *testing.T) {
	line := "(0014,FFF0)\tUS\tA\t1\tSITE\n"
	check := func(name, src string, opts dictionary.DICOptions, wantLine int) {
		t.Helper()
		for _, tolerant := range []bool{false, true} {
			opts.Tolerant = tolerant
			_, _, err := dictionary.ParseDIC(strings.NewReader(src), opts)
			if !errors.Is(err, dictionary.ErrDICResourceLimit) {
				t.Fatalf("%s (tolerant=%v): %v", name, tolerant, err)
			}
			var lineErr *dictionary.DICLineError
			if wantLine > 0 && (!errors.As(err, &lineErr) || lineErr.Line != wantLine) {
				t.Fatalf("%s: line %v", name, err)
			}
		}
	}
	check("line length", "# ok\n"+strings.Repeat("#", 40)+"\n", dictionary.DICOptions{MaxLineLength: 32}, 2)
	check("line length at EOF", strings.Repeat("#", 33), dictionary.DICOptions{MaxLineLength: 32}, 1)
	check("line length huge", "# ok\n"+strings.Repeat("#", 1<<16)+"\n", dictionary.DICOptions{MaxLineLength: 64}, 2)
	check("entries", line+strings.Replace(line, "FFF0", "FFF1", 1)+strings.Replace(line, "FFF0", "FFF2", 1), dictionary.DICOptions{MaxEntries: 2}, 3)
	check("repeating", "(60xx,0F00)\tUS\tA\t1\tS\n(50xx,0F00)\tUS\tB\t1\tS\n", dictionary.DICOptions{MaxRepeating: 1}, 2)
	check("file size", strings.Repeat(line, 8), dictionary.DICOptions{MaxFileSize: int64(len(line) * 3)}, 0)
	if _, _, err := dictionary.ParseDIC(strings.NewReader(strings.Repeat(line, 3)), dictionary.DICOptions{MaxFileSize: int64(len(line) * 3)}); err != nil {
		t.Fatalf("exact size limit: %v", err)
	}
	for _, opts := range []dictionary.DICOptions{{MaxFileSize: -1}, {MaxLineLength: -1}, {MaxEntries: -1}, {MaxRepeating: -1}, {MaxSkipped: -1}} {
		if _, _, err := dictionary.ParseDIC(strings.NewReader(line), opts); !errors.Is(err, dictionary.ErrDICOptions) {
			t.Fatalf("options %+v: %v", opts, err)
		}
	}
}

func TestLoadDICFile(t *testing.T) {
	path := filepath.Join("testdata", "nde_site.dic")
	d, report, err := dictionary.LoadDICFile(path, dictionary.DICOptions{})
	if err != nil || d.Len() != 7 || report.Entries != 7 {
		t.Fatalf("LoadDICFile: %v %+v", err, report)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := dictionary.LoadDICFile(path, dictionary.DICOptions{MaxFileSize: info.Size() - 1}); !errors.Is(err, dictionary.ErrDICResourceLimit) {
		t.Fatalf("size precheck: %v", err)
	}
	if _, _, err := dictionary.LoadDICFile("testdata", dictionary.DICOptions{}); err == nil {
		t.Fatal("directory accepted")
	}
	if _, _, err := dictionary.LoadDICFile(filepath.Join(t.TempDir(), "missing.dic"), dictionary.DICOptions{}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file: %v", err)
	}
}

func TestDICOverridesAndPrecedence(t *testing.T) {
	src := strings.Join([]string{
		"(0014,FFF0)\tUS\tFirst\t1\tSITE",
		"(0014,FFF0)\tSS\tSecond\t1\tSITE", // same tag: later line wins
		"(6000-60FF,0F00)\tUS\tWide\t1\tSITE",
		"(6000-601E,0F00)\tSS\tNarrow\t1\tSITE", // narrower range wins inside its span
		"(6010,0F00)\tFL\tExact\t1\tSITE",       // exact wins over any range
		"(60xx,0F00)\tOW\tWideAgain\t1\tSITE",   // same span as Wide: the later line wins
		"(0019,\"ACME\",10)\tUS\tRelativeOld\t1\tSITE",
		"(0019,\"ACME\",10)\tUL\tRelative\t1\tSITE",
		"(0019,\"ACME\",1110)\tFD\tFixed\t1\tSITE",
		"(0019-o-001B,\"ACME\",10-u-1F)\tSL\tRelativeRange\t1\tSITE",
	}, "\n")
	d, report := parseDIC(t, src, dictionary.DICOptions{})
	if report.Entries != 10 || report.Overridden != 2 || d.Len() != 8 {
		t.Fatalf("report %+v len %d", report, d.Len())
	}
	for tag, want := range map[core.Tag]string{
		core.NewTag(0x0014, 0xfff0): "Second",
		core.NewTag(0x6002, 0x0f00): "Narrow",
		core.NewTag(0x6010, 0x0f00): "Exact",
		core.NewTag(0x6020, 0x0f00): "WideAgain",
	} {
		if e, ok := d.ByTag(tag); !ok || e.Keyword != want {
			t.Errorf("ByTag(%v) = %q, want %q", tag, e.Keyword, want)
		}
	}
	if _, ok := d.ByKeyword("First"); ok {
		t.Error("overridden keyword still resolves")
	}
	if e, ok := d.ByKeyword("Narrow"); !ok || e.Tag != core.NewTag(0x6000, 0x0f00) {
		t.Errorf("Narrow keyword %+v", e)
	}
	if e, ok := d.ByPrivate(" ACME ", 0x0019, 0x10); !ok || e.Keyword != "Relative" || e.Tag != core.NewTag(0x0019, 0x0010) {
		t.Errorf("ByPrivate %+v %v", e, ok)
	}
	if e, ok := d.ByPrivate("ACME", 0x001b, 0x15); !ok || e.Keyword != "RelativeRange" {
		t.Errorf("ByPrivate range %+v %v", e, ok)
	}
	for _, tc := range []struct {
		tag  core.Tag
		want string
	}{
		{core.NewTag(0x0019, 0x1110), "Fixed"},
		{core.NewTag(0x0019, 0x1010), "Relative"},
		{core.NewTag(0x0019, 0x2010), "Relative"},
		{core.NewTag(0x0019, 0x1210), "Relative"},
		{core.NewTag(0x0019, 0x1111), "RelativeRange"},
		{core.NewTag(0x001b, 0x1110), "RelativeRange"},
	} {
		if e, ok := d.ByPrivateTag("ACME", tc.tag); !ok || e.Keyword != tc.want || e.Tag != tc.tag {
			t.Errorf("ByPrivateTag(%v) = %+v, want %s", tc.tag, e, tc.want)
		}
	}
	for _, tc := range []struct {
		creator string
		tag     core.Tag
	}{
		{"acme", core.NewTag(0x0019, 0x1010)},  // creators are case sensitive
		{"ACME", core.NewTag(0x0019, 0x0010)},  // creator slot, not a data element
		{"ACME", core.NewTag(0x001d, 0x1010)},  // outside the group range
		{"ACME", core.NewTag(0x0019, 0x1120)},  // unknown offset
		{"A\\B", core.NewTag(0x0019, 0x1010)},  // invalid creator
		{"OTHER", core.NewTag(0x0019, 0x1110)}, // other creator
	} {
		if e, ok := d.ByPrivateTag(tc.creator, tc.tag); ok {
			t.Errorf("ByPrivateTag(%q, %v) matched %+v", tc.creator, tc.tag, e)
		}
	}
	if _, ok := d.ByPrivate("ACME", 0x0019, 0x20); ok {
		t.Error("fixed element resolved without its element")
	}

	var wg sync.WaitGroup
	for n := 0; n < 8; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				if e, ok := d.ByTag(core.NewTag(0x6002, 0x0f00)); !ok || e.Keyword != "Narrow" {
					t.Error("concurrent ByTag")
					return
				}
				if e, ok := d.ByPrivateTag("ACME", core.NewTag(0x0019, 0x1110)); !ok || e.Keyword != "Fixed" {
					t.Error("concurrent ByPrivateTag")
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestDICChainComposition(t *testing.T) {
	d, _, err := dictionary.LoadDICFile(filepath.Join("testdata", "nde_site.dic"), dictionary.DICOptions{})
	if err != nil {
		t.Fatal(err)
	}
	institution := core.NewTag(0x0008, 0x0080)
	marker := core.NewTag(0x6004, 0x0f00)
	curve := core.NewTag(0x5006, 0x0f10)
	if _, ok := std.Dictionary.ByTag(marker); ok {
		t.Fatal("fixture tag is standard; choose another")
	}
	override := dictionary.Chain{d, std.Dictionary}
	if e, ok := override.ByTag(institution); !ok || e.Keyword != "InspectionCompanyName" {
		t.Fatalf("override %+v", e)
	}
	if e, ok := override.ByKeyword("PatientName"); !ok || e.Tag != core.NewTag(0x0010, 0x0010) {
		t.Fatal("standard fallback lost")
	}
	extend := dictionary.Chain{std.Dictionary, d}
	if e, ok := extend.ByTag(institution); !ok || e.Keyword != "InstitutionName" {
		t.Fatalf("extension replaced standard entry: %+v", e)
	}
	for _, chain := range []dictionary.Chain{override, extend} {
		if e, ok := chain.ByTag(marker); !ok || e.Keyword != "TestOverlayMarker" || e.Tag != marker {
			t.Fatalf("repeating extension %+v", e)
		}
		if spec, ok := chain.VRSpecByTag(curve); !ok || spec.Context() != dictionary.ContextualVRXS {
			t.Fatalf("contextual VR through Chain: %v", spec.Values())
		}
		if got := dictionary.LookupVR(chain, marker); got != core.VRUS {
			t.Fatalf("LookupVR %s", got)
		}
	}

	if !dictionary.HasPrivateDictionary(override) {
		t.Fatal("private definitions not detected")
	}
	absoluteOnly, _ := parseDIC(t, "(0014,FFF0)\tUS\tA\t1\tSITE\n", dictionary.DICOptions{})
	if dictionary.HasPrivateDictionary(dictionary.Chain{absoluteOnly, std.Dictionary}) {
		t.Fatal("absolute-only dictionary activated private resolution")
	}
	for _, tc := range []struct {
		tag  core.Tag
		want string
		vr   core.VR
	}{
		{core.NewTag(0x0019, 0x1210), "ProbeFrequency", core.VRDS},
		{core.NewTag(0x0019, 0x1211), "WedgeAngles", core.VRFD},
		{core.NewTag(0x0019, 0x1020), "FixedCalibrationID", core.VRLO},
		{core.NewTag(0x0023, 0x4033), "ChannelGain", core.VRUL},
	} {
		e, ok := dictionary.LookupScopedEntry(override, tc.tag, "ACME NDE 1.0")
		if !ok || e.Keyword != tc.want || e.VR != tc.vr || e.Tag != tc.tag {
			t.Errorf("LookupScopedEntry(%v) = %+v %v", tc.tag, e, ok)
		}
	}
	if e, ok := dictionary.LookupScopedEntry(override, core.NewTag(0x0023, 0x4033), "ACME NDE 1.0"); !ok || !e.Retired {
		t.Error("retired version flag lost")
	}
	if _, ok := dictionary.LookupScopedEntry(override, core.NewTag(0x0019, 0x1220), "ACME NDE 1.0"); ok {
		t.Error("fixed element resolved in another block")
	}

	// A later catalog can still extend a loaded file; the first match wins.
	catalog, err := dictionary.NewPrivateCatalog([]dictionary.PrivateEntry{
		{Creator: "ACME NDE 1.0", Group: 0x0019, Offset: 0x10, VR: core.VRSH, Keyword: "CatalogFrequency"},
		{Creator: "ACME NDE 1.0", Group: 0x0019, Offset: 0x12, VR: core.VRSH, Keyword: "CatalogOnly"},
	})
	if err != nil {
		t.Fatal(err)
	}
	mixed := dictionary.Chain{d, catalog, std.Dictionary}
	if e, _ := dictionary.LookupScopedEntry(mixed, core.NewTag(0x0019, 0x1110), "ACME NDE 1.0"); e.Keyword != "ProbeFrequency" {
		t.Errorf("loaded file should win: %+v", e)
	}
	if e, _ := dictionary.LookupScopedEntry(mixed, core.NewTag(0x0019, 0x1112), "ACME NDE 1.0"); e.Keyword != "CatalogOnly" {
		t.Errorf("catalog fallback: %+v", e)
	}
}

// TestDICImplicitVRRead resolves private and repeating definitions from a
// loaded file while reading a synthetic Implicit VR Little Endian dataset.
func TestDICImplicitVRRead(t *testing.T) {
	d, _, err := dictionary.LoadDICFile(filepath.Join("testdata", "nde_site.dic"), dictionary.DICOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	put := func(group, element uint16, value []byte) {
		_ = binary.Write(&buf, binary.LittleEndian, [2]uint16{group, element})
		_ = binary.Write(&buf, binary.LittleEndian, uint32(len(value)))
		buf.Write(value)
	}
	put(0x0019, 0x0012, []byte("ACME NDE 1.0"))
	put(0x0019, 0x1210, []byte("5.0 "))
	put(0x0019, 0x1211, binary.LittleEndian.AppendUint64(binary.LittleEndian.AppendUint64(nil, 0x3ff0000000000000), 0x4000000000000000))
	put(0x6002, 0x0f00, []byte{7, 0})
	obj, err := object.ReadDataSetWithOptions(bytes.NewReader(buf.Bytes()), transfer.ImplicitVRLittleEndian, object.ReadFileOptions{Dictionary: dictionary.Chain{d, std.Dictionary}})
	if err != nil {
		t.Fatal(err)
	}
	for tag, want := range map[core.Tag]core.VR{
		core.NewTag(0x0019, 0x1210): core.VRDS,
		core.NewTag(0x0019, 0x1211): core.VRFD,
		core.NewTag(0x6002, 0x0f00): core.VRUS,
	} {
		e, ok := obj.Get(tag)
		if !ok || e.VR() != want {
			t.Errorf("%v: VR %v ok=%v", tag, e.VR(), ok)
		}
	}
	if raw, ok := obj.GetRaw(core.NewTag(0x6002, 0x0f00)); !ok || !bytes.Equal(raw, []byte{7, 0}) {
		t.Errorf("repeating value %v %v", raw, ok)
	}
}

// TestDICStandardSourceMatchesGeneratedDictionary loads the vendored standard
// source at run time and checks it against the generated std package.
func TestDICStandardSourceMatchesGeneratedDictionary(t *testing.T) {
	d, report, err := dictionary.LoadDICFile(filepath.Join("..", "internal", "standard", "dicom.dic"), dictionary.DICOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Skipped) != 0 || d.HasPrivateDefinitions() {
		t.Fatalf("report %+v", report)
	}
	checked := 0
	for _, def := range d.Entries() {
		switch def.Version {
		case "PRIVATE", "ILLEGAL", "GENERIC":
			continue // the generator does not expand private-group placeholders
		}
		if def.VRToken == "na" {
			continue
		}
		span := uint32(0)
		if def.Repeating {
			span = 0x1ff // every repeating range in the source is narrower
		}
		g0, e0 := uint32(def.Entry.Tag.Group), uint32(def.Entry.Tag.Element)
		for g := g0; g <= min(g0+span, 0xffff); g++ {
			for e := e0; e <= min(e0+span, 0xffff); e++ {
				tag := core.NewTag(uint16(g), uint16(e))
				if !def.Matches(tag) {
					continue
				}
				se, sok := std.Dictionary.ByTag(tag)
				de, dok := d.ByTag(tag)
				if !sok || !dok || se != de {
					t.Fatalf("%v: std %+v %v, dic %+v %v", tag, se, sok, de, dok)
				}
				ss, _ := dictionary.LookupVRSpec(std.Dictionary, tag)
				if ds, _ := d.VRSpecByTag(tag); ss != ds {
					t.Fatalf("%v: VR spec %v vs %v", tag, ss.Values(), ds.Values())
				}
				checked++
			}
		}
	}
	if checked < 5000 {
		t.Fatalf("only %d tags checked", checked)
	}
}
