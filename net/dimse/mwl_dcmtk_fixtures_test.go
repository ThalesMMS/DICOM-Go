package dimse

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
)

// DCMTK's fictitious worklist database and example queries (dcmwlm/data,
// commit 8d68392d, imported under testdata/upstream/dcmtk with provenance).
const dcmtkWorklistRoot = "../../testdata/upstream/dcmtk"

// dcmtkWorklistGolden holds, per query, the response fingerprints DCMTK
// wlmscpfs returned (TestDCMTKWorklistOracle regenerates it).
const dcmtkWorklistGolden = "testdata/dcmtk_mwl_responses.json"

// dcmtkWorklistParseOptions accept the one non-Table K.6-1 return key the
// DCMTK queries request, Comments on the Scheduled Procedure Step.
var dcmtkWorklistParseOptions = ModalityWorklistParseOptions{AcceptUnknownReturnKeys: true}

func readDCMTKDumpFile(t *testing.T, path string) *object.Object {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	elements, err := dicomtest.ReadDCMTKDump(file)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return object.FromElements(elements, std.Dictionary)
}

func dcmtkWorklistFiles(t *testing.T, dir string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dcmtkWorklistRoot, dir, "*.dump"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no DCMTK %s dumps: %v", dir, err)
	}
	sort.Strings(paths)
	return paths
}

func dcmtkWorklistRecords(t *testing.T) []*object.Object {
	t.Helper()
	var records []*object.Object
	for _, path := range dcmtkWorklistFiles(t, "wlistdb") {
		records = append(records, readDCMTKDumpFile(t, path))
	}
	return records
}

// worklistQueryFiles lists DCMTK's example queries and the queries written
// for dicom-go under testdata/mwl_queries, all run against DCMTK's database.
func worklistQueryFiles(t *testing.T) []string {
	t.Helper()
	paths := dcmtkWorklistFiles(t, "wlistqry")
	extra, err := filepath.Glob(filepath.Join("testdata", "mwl_queries", "*.dump"))
	if err != nil || len(extra) == 0 {
		t.Fatalf("no additional worklist queries: %v", err)
	}
	sort.Strings(extra)
	return append(paths, extra...)
}

func queryName(path string) string {
	return strings.TrimSuffix(filepath.Base(path), ".dump")
}

// worklistFingerprint renders the non-empty attributes of a response, one
// "path=values" line each, sorted; the character set and timezone
// declarations are left out. Absent and empty attributes are equivalent,
// since wlmscpfs returns requested keys the record lacks as empty.
func worklistFingerprint(response *object.Object) string {
	var lines []string
	var walk func(prefix string, elements []core.Element)
	walk = func(prefix string, elements []core.Element) {
		for _, element := range elements {
			tag := element.Tag()
			if tag.Element == 0 || tag == tagMWLSpecificCharacterSet || tag == tagMWLTimezoneOffsetFromUTC {
				continue
			}
			path := prefix + fmt.Sprintf("%04X%04X", tag.Group, tag.Element)
			if sequence, ok := element.Value.(core.SequenceValue); ok {
				for i, item := range sequence.Items {
					walk(fmt.Sprintf("%s[%d].", path, i), item.Elements)
				}
				continue
			}
			var values []string
			for _, value := range element.StringValues() {
				values = append(values, strings.TrimSpace(value))
			}
			if joined := strings.Join(values, `\`); joined != "" {
				lines = append(lines, path+"="+joined)
			}
		}
	}
	walk("", response.Elements())
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// ourWorklistResponses answers a query with the in-memory handler and the
// SCP's projection, returning the sorted response fingerprints.
func ourWorklistResponses(t *testing.T, records []*object.Object, query *object.Object) []string {
	t.Helper()
	parsed, err := ParseModalityWorklistIdentifierWithOptions(query, dcmtkWorklistParseOptions)
	if err != nil {
		t.Fatalf("parse query: %v", err)
	}
	handler, err := NewInMemoryModalityWorklistHandlerWithOptions(records, InMemoryModalityWorklistOptions{UniversalQueryMatchesAll: true})
	if err != nil {
		t.Fatal(err)
	}
	var responses []string
	err = handler.Find(context.Background(), ModalityWorklistRequest{Identifier: parsed, RawIdentifier: query}, func(candidate *object.Object) error {
		projected, err := ProjectModalityWorklistResult(parsed, candidate)
		if err != nil {
			return err
		}
		responses = append(responses, worklistFingerprint(projected))
		return nil
	})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	sort.Strings(responses)
	return responses
}

// TestDCMTKWorklistFixturesMatchGolden: for DCMTK's example queries and the
// additional ones against DCMTK's example database, the in-memory handler and
// the SCP projection return the responses DCMTK wlmscpfs returned (recorded
// in the golden file).
func TestDCMTKWorklistFixturesMatchGolden(t *testing.T) {
	data, err := os.ReadFile(dcmtkWorklistGolden)
	if err != nil {
		t.Fatal(err)
	}
	var golden map[string][]string
	if err := json.Unmarshal(data, &golden); err != nil {
		t.Fatal(err)
	}
	records := dcmtkWorklistRecords(t)
	queries := worklistQueryFiles(t)
	if len(golden) != len(queries) {
		t.Fatalf("golden has %d queries, fixtures %d", len(golden), len(queries))
	}
	for _, path := range queries {
		name := queryName(path)
		t.Run(name, func(t *testing.T) {
			want, ok := golden[name]
			if !ok {
				t.Fatal("query missing from the golden file")
			}
			got := ourWorklistResponses(t, records, readDCMTKDumpFile(t, path))
			if strings.Join(got, "\n--\n") != strings.Join(want, "\n--\n") {
				t.Fatalf("responses differ from DCMTK wlmscpfs\ngot:\n%s\nwant:\n%s", strings.Join(got, "\n--\n"), strings.Join(want, "\n--\n"))
			}
		})
	}
}
