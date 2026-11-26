package privatecatalog

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
)

func complete(t testing.TB) *Catalog {
	t.Helper()
	c, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func synthetic(t testing.TB, id Source, text string, limits Limits) (*Catalog, error) {
	t.Helper()
	l, _, err := normalizeOptions(Options{Limits: limits})
	if err != nil {
		return nil, err
	}
	rows := 0
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "(") || strings.HasPrefix(strings.TrimSpace(line), "<entry") {
			rows++
		}
	}
	return build([]sourceInput{{SourceInfo{ID: id, SHA256: digest([]byte(text)), Rows: rows}, []byte(text)}}, l)
}

func TestCompleteSourcesAndEveryRowProvenance(t *testing.T) {
	c := complete(t)
	summary := c.Summary()
	if summary.Records != 13356 || summary.BySource[DCMTK] != 2837 || summary.BySource[GDCM] != 10519 {
		t.Fatalf("accounting: %+v", summary)
	}
	want := map[Source]string{DCMTK: "d3907ad1c14b2a9bfb434099e0d1775246be63b58f5d06b57c6ec5a53645fe12", GDCM: "d3744ac3678d4c089742d4cfc30616301f59da0ad528708f2a51e4332ef8fea4"}
	lines := map[Source][]string{}
	for _, source := range c.Sources() {
		data, err := files.ReadFile(source.DataFile)
		if err != nil || digest(data) != want[source.ID] || source.SHA256 != want[source.ID] || len(source.Commit) != 40 || len(source.NoticeFiles) == 0 {
			t.Fatalf("source: %+v %v", source, err)
		}
		lines[source.ID] = strings.Split(string(data), "\n")
	}
	seen := map[string]bool{}
	for _, r := range c.Records() {
		key := fmt.Sprintf("%s:%d", r.Source, r.Line)
		if seen[key] || r.Line < 1 || r.Row != lines[r.Source][r.Line-1] || digest([]byte(r.Row)) != r.RowSHA256 || r.Disposition == "" {
			t.Fatalf("origin lost: %s", key)
		}
		seen[key] = true
		if r.Disposition != Resolved && r.Reason == "" {
			t.Fatalf("unexplained quarantine: %s", key)
		}
	}
	for _, line := range []int{343, 379, 446, 609, 1174, 2035} {
		if !seen[fmt.Sprintf("DCMTK:%d", line)] {
			t.Fatal("spaced name/omitted version row dropped")
		}
	}
	if summary.ByDisposition[Contextual] != 4 || summary.ByDisposition[UnsupportedPattern] != 11 || summary.ByDisposition[InvalidDefinition] != 29 {
		t.Fatalf("unusable rows changed: %+v", summary)
	}
	for _, text := range []string{"OFFIS", "1994-2026", "Mathieu Malaterre", "CREATIS", "PixelMed Publishing", "Redistribution and use"} {
		if !strings.Contains(LicenseNotice(), text) {
			t.Fatalf("missing notice: %s", text)
		}
	}
	t.Logf("source dispositions: %v", summary.ByDisposition)
}

func TestSourceSelectionOptionsAndNil(t *testing.T) {
	for _, id := range []Source{DCMTK, GDCM} {
		c, err := New(Options{Sources: []Source{id}})
		if err != nil || len(c.Sources()) != 1 || c.Summary().BySource[id] != c.Sources()[0].Rows {
			t.Fatalf("selected source: %v", err)
		}
	}
	for _, opts := range []Options{{Sources: []Source{"unknown"}}, {Sources: []Source{DCMTK, DCMTK}}, {Sources: []Source{DCMTK, GDCM, DCMTK}}, {Limits: Limits{MaxRecords: -1}}, {Limits: Limits{MaxLineBytes: 4097}}} {
		if c, err := New(opts); c != nil || !errors.Is(err, ErrOptions) {
			t.Fatalf("invalid options accepted: %v", err)
		}
	}
	a := complete(t)
	b, err := New(Options{Sources: []Source{GDCM, DCMTK}})
	if err != nil || a.Lookup("GEMS_ACQU_01", core.NewTag(9, 0x8025)).Entry != b.Lookup("GEMS_ACQU_01", core.NewTag(9, 0x8025)).Entry {
		t.Fatal("source order changed resolution")
	}
	var nilCatalog *Catalog
	if nilCatalog.HasPrivateDefinitions() || nilCatalog.Records() != nil || nilCatalog.Sources() != nil || nilCatalog.Summary().Records != 0 {
		t.Fatal("nil catalog")
	}
	if r := nilCatalog.Lookup("ACME", core.NewTag(0x19, 0x1001)); r.Disposition != Missing || r.Entry.VR != core.VRUN {
		t.Fatal("nil lookup")
	}
}

func TestConflictsAliasesVMAndDuplicateCandidates(t *testing.T) {
	text := "(0019,\"ACME\",01)\tUS\tFirst\t1\tPrivateTag\n(0019,\"ACME\",01)\tSL\tSecond\t1\tPrivateTag\n"
	c, err := synthetic(t, DCMTK, text, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	r := c.Lookup("ACME", core.NewTag(0x19, 0x8001))
	if r.Disposition != VRConflict || r.Entry.VR != core.VRUN || len(r.Candidates) != 2 || r.Candidates[0].Line == r.Candidates[1].Line {
		t.Fatalf("duplicate overwritten: %+v", r)
	}
	text = "(0019,\"ACME\",01)\tUS\tFirst\t1\n(0019,\"ACME\",01)\tUS\tSecond\t1-n\n(0019,\"ACME\",02)\tox\tOpaque\t1\n"
	c, err = synthetic(t, DCMTK, text, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	r = c.Lookup("ACME", core.NewTag(0x19, 0x8001))
	if r.Disposition != Resolved || r.Entry.VR != core.VRUS || !r.VMConflict || r.Entry.VM != "" || len(r.Candidates) != 2 {
		t.Fatalf("VM guessed: %+v", r)
	}
	r = c.Lookup("ACME", core.NewTag(0x19, 0x8002))
	if r.Disposition != Contextual || r.Entry.VR != core.VRUN || len(r.Candidates[0].AllowedVRs) != 2 {
		t.Fatalf("context guessed: %+v", r)
	}
	c = complete(t)
	for _, r := range c.Records() {
		if r.Disposition != Contextual {
			continue
		}
		var group, offset uint16
		fmt.Sscanf(r.Group, "%x", &group)
		if r.Source == GDCM {
			fmt.Sscanf(r.Element[2:], "%x", &offset)
		} else {
			fmt.Sscanf(r.Element, "%x", &offset)
		}
		element := offset
		if r.Source == GDCM || offset < 0x1000 {
			element |= 0x8000
		}
		if got := c.Lookup(r.Creator, core.NewTag(group, element)); got.Entry.VR != core.VRUN || got.Disposition != Contextual {
			t.Fatalf("alias guessed: %+v", got)
		}
	}
	// This full-source key is a real cross-source VR conflict, not just VM.
	r = c.Lookup("DIDI TO PCR 1.1", core.NewTag(0x19, 0x8022))
	if r.Disposition != VRConflict || r.Entry.VR != core.VRUN || len(r.Candidates) < 2 {
		t.Fatalf("source conflict lost: %+v", r)
	}
}

func TestMatchingPatternsFixedElementsAndInvalidDomain(t *testing.T) {
	text := "(0019-o-001D,\"ACME  TEAM\",10-u-12)\tUS\tRange\t1\n(0019,\"ACME  TEAM\",1013)\tLO\tFixed\t1\n(0019,\"ACME  TEAM\",11)\tSL\tOverlap\t1\n(0003,\"ACME\",01)\tUS\tForbidden\t1\n"
	c, err := synthetic(t, DCMTK, text, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if r := c.Lookup("ACME  TEAM ", core.NewTag(0x1d, 0xff12)); r.Entry.VR != core.VRUS {
		t.Fatalf("range missed: %+v", r)
	}
	if r := c.Lookup("ACME  TEAM", core.NewTag(0x19, 0x1013)); r.Entry.VR != core.VRLO {
		t.Fatal("fixed missed")
	}
	if _, ok := c.ByPrivate("ACME  TEAM", 0x19, 0x13); ok {
		t.Fatal("fixed generalized to offset")
	}
	if r := c.Lookup("ACME  TEAM", core.NewTag(0x19, 0x1113)); r.Disposition != Missing {
		t.Fatal("fixed moved to another block")
	}
	if r := c.Lookup("ACME  TEAM", core.NewTag(0x19, 0x8011)); r.Disposition != VRConflict {
		t.Fatal("exact/range conflict hidden")
	}
	for _, creator := range []string{"acme  team", "ACME TEAM", "UNKNOWN"} {
		if c.Lookup(creator, core.NewTag(0x19, 0xff12)).Disposition != Missing {
			t.Fatal("creator normalized too far")
		}
	}
	for _, tag := range []core.Tag{core.NewTag(3, 0x1001), core.NewTag(0x1a, 0x1010), core.NewTag(0x19, 0x0100), core.NewTag(0xffff, 0x1001)} {
		if r := c.Lookup("ACME", tag); r.Disposition != InvalidRequest || r.Entry.VR != core.VRUN {
			t.Fatal("invalid domain guessed")
		}
	}
	text = `<entry owner="ACME" group="60xx" element="xxaa" vr="US" vm="1" name="Range"/>` + "\n" +
		`<entry owner="ACME" group="0019" element="10xx" vr="LO" vm="1" name="Private Creator"/>` + "\n" +
		`<entry owner="ACME" group="0019" element="xx01" vr="SL" vm="1" name="Specific"/>` + "\n"
	c, err = synthetic(t, GDCM, text, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if c.Lookup("ACME", core.NewTag(0x6001, 0x80aa)).Entry.VR != core.VRUS || c.Lookup("ACME", core.NewTag(0x60ff, 0xffaa)).Entry.VR != core.VRUS {
		t.Fatal("odd wildcard missed")
	}
	if c.Lookup("ACME", core.NewTag(0x6000, 0x10aa)).Disposition != InvalidRequest || c.Lookup("ACME", core.NewTag(0x6101, 0x10aa)).Disposition != Missing {
		t.Fatal("wildcard expanded outside domain")
	}
	if c.Lookup("ACME", core.NewTag(0x19, 0x1001)).Entry.VR != core.VRSL || c.Lookup("ACME", core.NewTag(0x19, 0x1002)).Disposition != Missing || c.Records()[1].Disposition != UnsupportedPattern {
		t.Fatal("10xx poisoned relative data")
	}
}

func TestKnownCorpusContradictionsStayOpaque(t *testing.T) {
	c := complete(t)
	cases := []struct {
		creator string
		group   uint16
		offsets []uint16
	}{
		{"SIEMENS MED", 0x21, []uint16{0x11}}, {"SIEMENS CT VA0  COAD", 0x19, []uint16{0x90, 0x92, 0x93, 0x96}},
		{"PHILIPS MR/PART", 0x19, []uint16{0}}, {"PHILIPS MR/PART", 0x21, []uint16{0, 0x10}},
	}
	for _, tc := range cases {
		for _, offset := range tc.offsets {
			r := c.Lookup(tc.creator, core.NewTag(tc.group, 0x8000|offset))
			if r.Disposition != KnownCorpusConflict || r.Entry.VR != core.VRUN || len(r.Candidates) == 0 {
				t.Fatalf("known contradiction reactivated: %+v", r)
			}
		}
	}
}

func TestKnownPhilipsFixedCorpusKeysWithSourceSelection(t *testing.T) {
	onlyDCMTK, err := New(Options{Sources: []Source{DCMTK}})
	if err != nil {
		t.Fatal(err)
	}
	both := complete(t)
	for _, tag := range []core.Tag{core.NewTag(0x19, 0x1000), core.NewTag(0x21, 0x1000), core.NewTag(0x21, 0x1010)} {
		for _, c := range []*Catalog{onlyDCMTK, both} {
			r := c.Lookup("PHILIPS MR/PART", tag)
			if r.Entry.VR != core.VRUN || r.Disposition != KnownCorpusConflict || len(r.Candidates) == 0 {
				t.Fatalf("fixed corpus key reactivated: %+v", r)
			}
			for _, row := range r.Candidates {
				if row.Disposition != KnownCorpusConflict {
					t.Fatalf("candidate not quarantined: %+v", row)
				}
			}
		}
		for _, block := range []uint16{0x80, 0xff} {
			moved := core.NewTag(tag.Group, block<<8|uint16(uint8(tag.Element)))
			if r := onlyDCMTK.Lookup("PHILIPS MR/PART", moved); r.Disposition != Missing || len(r.Candidates) != 0 {
				t.Fatalf("fixed quarantine generalized: %+v", r)
			}
			if r := both.Lookup("PHILIPS MR/PART", moved); r.Disposition != KnownCorpusConflict || r.Entry.VR != core.VRUN || len(r.Candidates) != 1 || r.Candidates[0].Source != GDCM {
				t.Fatalf("relative source lost: %+v", r)
			}
		}
	}
	if onlyDCMTK.Summary().ByDisposition[KnownCorpusConflict] != 8 || both.Summary().ByDisposition[KnownCorpusConflict] != 16 {
		t.Fatal("fixed corpus rows not counted")
	}
}

func TestFiniteLimitsAndAtomicFailure(t *testing.T) {
	for _, l := range []Limits{{MaxSourceBytes: 1}, {MaxRecords: 1}, {MaxLineBytes: 20}, {MaxNormalizedBytes: 20}, {MaxPatterns: 1}, {MaxPartitions: 1}} {
		c, err := New(Options{Limits: l})
		if c != nil || !errors.Is(err, ErrLimitExceeded) {
			t.Fatalf("limit not atomic: %+v %v", l, err)
		}
	}
	row := "(0019,\"ACME\",01)\tUS\tValue\t1\n"
	r := Record{Source: DCMTK, Row: strings.TrimSuffix(row, "\n")}
	parseRow(&r)
	line, _ := normalizeRow(&r)
	maxLine := max(len(row)-1, len(line))
	c, err := synthetic(t, DCMTK, row, Limits{MaxSourceBytes: len(row), MaxLineBytes: maxLine, MaxNormalizedBytes: len(line) + 1, MaxRecords: 1})
	if err != nil || len(c.Records()) != 1 {
		t.Fatalf("exact boundary: %v", err)
	}
	for _, l := range []Limits{{MaxSourceBytes: len(row) - 1}, {MaxLineBytes: maxLine - 1}, {MaxNormalizedBytes: len(line)}} {
		if c, err := synthetic(t, DCMTK, row, l); c != nil || !errors.Is(err, ErrLimitExceeded) {
			t.Fatalf("boundary accepted: %+v %v", l, err)
		}
	}
	if c, err := synthetic(t, DCMTK, row+row, Limits{MaxPartitions: 1}); c != nil || !errors.Is(err, ErrLimitExceeded) {
		t.Fatal("duplicate visits not bounded")
	}
	if c, err := synthetic(t, DCMTK, row+row, Limits{MaxRecords: 1}); c != nil || !errors.Is(err, ErrLimitExceeded) {
		t.Fatal("repeated rows not counted")
	}
	input := sourceInput{SourceInfo{ID: DCMTK, SHA256: strings.Repeat("0", 64), Rows: 1}, []byte(row)}
	if c, err := build([]sourceInput{input}, DefaultLimits()); c != nil || !errors.Is(err, ErrSource) {
		t.Fatal("corrupt pin accepted")
	}
	text := `<entry owner="ACME" group="0019" element="xx01" vr="US" vm="1" name="A"/><entry owner="ACME" group="0019" element="xx02" vr="US" vm="1" name="B"/>` + "\n"
	c, err = synthetic(t, GDCM, text, Limits{})
	if err != nil || c.Records()[0].Disposition != InvalidDefinition {
		t.Fatal("multiple XML entries silently accepted")
	}
}

func TestDetachedOwnershipAndConcurrentLookups(t *testing.T) {
	c := complete(t)
	records := c.Records()
	originalVR := records[0].AllowedVRs[0]
	records[0].Row = "changed"
	records[0].AllowedVRs[0] = core.VRSQ
	sources := c.Sources()
	sources[0].NoticeFiles[0] = "changed"
	sources[0].Licenses[0] = "changed"
	sources[0].LicenseURLs[0] = "changed"
	summary := c.Summary()
	summary.BySource[DCMTK] = 0
	r := c.Lookup("GEMS_ACQU_01", core.NewTag(9, 0x8025))
	r.Candidates[0].AllowedVRs[0] = core.VRSQ
	if c.Records()[0].Row == "changed" || c.Records()[0].AllowedVRs[0] != originalVR ||
		c.Sources()[0].NoticeFiles[0] == "changed" || c.Sources()[0].Licenses[0] == "changed" || c.Sources()[0].LicenseURLs[0] == "changed" ||
		c.Summary().BySource[DCMTK] != 2837 || c.Lookup("GEMS_ACQU_01", core.NewTag(9, 0x8025)).Candidates[0].AllowedVRs[0] != core.VRUS {
		t.Fatal("mutable alias escaped")
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if c.Lookup("GEMS_ACQU_01", core.NewTag(9, 0xff25)).Entry.VR != core.VRUS || c.Summary().Records != 13356 {
					t.Error("concurrent resolution changed")
					return
				}
			}
		}()
	}
	wg.Wait()
}
