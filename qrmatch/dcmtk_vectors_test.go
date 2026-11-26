package qrmatch

// Cross-check against DCMTK's DcmAttributeMatching vectors (tmatch.cc,
// testdata/upstream/dcmtk/match_vectors.json, #1053). Divergences name their
// normative reason; docs/CONFORMANCE.md lists the same cases.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/tags"
)

// dcmtkMatchDivergences lists the tmatch.cc rows dicom-go answers
// differently, by line.
var dcmtkMatchDivergences = map[int]string{
	95: "ACR-NEMA date YYYY.MM.DD as a query: PS3.5 6.2 DA notes the format is not DICOM " +
		"conformant (DCMTK's own isDateQuery rejects it, line 93); accepted only with LenientDateQueries",
	115: "ACR-NEMA time HH:MM:SS.frac as a query: PS3.5 6.2 TM notes the format is not DICOM " +
		"conformant (DCMTK's own isTimeQuery rejects it, line 108)",
	118: impreciseRangeBound,
	139: impreciseRangeBound,
}

// impreciseRangeBound is the reading of a range bound with trailing null
// components: DCMTK takes the earliest instant ("12" is 12:00:00), dicom-go
// the whole period it names.
const impreciseRangeBound = "an upper bound with trailing null components (\"12\", \"2000\") covers " +
	"the whole hour or year: PS3.5 6.2 (TM, DT) says such a value is not precise to the omitted " +
	"components and PS3.4 C.2.2.2.5 matches times \"prior to and including\" the bound; DCMTK " +
	"takes the first instant of the period"

func TestMatchAgreesWithDCMTKVectors(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "testdata", "upstream", "dcmtk", "match_vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Entries []struct {
			Line       int     `json:"line"`
			Kind       string  `json:"kind"`
			VR         string  `json:"vr"`
			Query      string  `json:"query"`
			Candidate  *string `json:"candidate"`
			Match      *bool   `json:"match"`
			ValidQuery *bool   `json:"valid_query"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Entries) < 75 {
		t.Fatalf("%d entries", len(fixture.Entries))
	}
	// Each DCMTK matching primitive is exercised through a key of a VR that
	// selects it: wild card matching (LO), range matching (DA, TM, DT), UID
	// list matching (UI) and single value matching without wild cards (AS,
	// which PS3.4 C.2.2.2.4 excludes from wild card matching).
	keys := map[string]struct {
		tag core.Tag
		vr  core.VR
		// valid is a candidate the query validity rows are matched against.
		valid string
	}{
		"wildcard":     {tags.PatientID, core.VRLO, ""},
		"date":         {tags.StudyDate, core.VRDA, "20000101"},
		"time":         {tags.StudyTime, core.VRTM, "120000"},
		"datetime":     {tags.AcquisitionDateTime, core.VRDT, "20000101"},
		"uid_list":     {tags.StudyInstanceUID, core.VRUI, ""},
		"single_value": {tags.PatientAge, core.VRAS, ""},
		"DA":           {tags.StudyDate, core.VRDA, ""},
		"TM":           {tags.StudyTime, core.VRTM, ""},
		"PN":           {tags.PatientName, core.VRPN, ""},
	}
	seen := map[int]bool{}
	for _, e := range fixture.Entries {
		kind := e.Kind
		if kind == "vr" {
			kind = e.VR
		}
		key, ok := keys[kind]
		if !ok {
			t.Fatalf("tmatch.cc:%d: no key for %s", e.Line, kind)
		}
		candidate := key.valid
		if e.Candidate != nil {
			candidate = *e.Candidate
		}
		result, err := Match(dataset(str(key.tag, key.vr, e.Query)), dataset(str(key.tag, key.vr, candidate)), Options{})
		var got, want bool
		switch {
		case e.ValidQuery != nil:
			got, want = !errors.Is(err, ErrInvalidQuery), *e.ValidQuery
		case err != nil && !errors.Is(err, ErrInvalidQuery):
			t.Fatalf("tmatch.cc:%d: %v", e.Line, err)
		default:
			got, want = err == nil && result.Match, *e.Match
		}
		reason, diverges := dcmtkMatchDivergences[e.Line]
		seen[e.Line] = true
		switch {
		case got != want && !diverges:
			t.Errorf("tmatch.cc:%d: %s query %q candidate %q: dicom-go %v (err %v), DCMTK %v", e.Line, kind, e.Query, candidate, got, err, want)
		case got == want && diverges:
			t.Errorf("tmatch.cc:%d: now agrees with DCMTK; remove the divergence %q", e.Line, reason)
		}
	}
	for line := range dcmtkMatchDivergences {
		if !seen[line] {
			t.Errorf("divergence %d names no vector", line)
		}
	}
}
