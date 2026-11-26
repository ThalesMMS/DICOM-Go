package dicomweb

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/dictionary/tags"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/personname/fuzzy"
)

func searchCandidate(studyUID, patientName, studyDate string) *object.Object {
	return object.FromElements([]core.Element{
		{Header: core.ElementHeader{Tag: tags.StudyInstanceUID, VR: core.VRUI}, Value: core.StringValue{studyUID}},
		{Header: core.ElementHeader{Tag: tags.PatientName, VR: core.VRPN}, Value: core.StringValue{patientName}},
		{Header: core.ElementHeader{Tag: tags.StudyDate, VR: core.VRDA}, Value: core.StringValue{studyDate}},
	}, std.Dictionary)
}

// matcherSearchBackend is a QIDO-RS study search over in-memory candidates
// that applies SearchMatcher, as an archive backend would.
func matcherSearchBackend(encoder fuzzy.FuzzyStr, candidates []*object.Object) *serverTestBackend {
	backend := completeServerTestBackend()
	backend.search = func(ctx context.Context, req SearchRequest, yield func(Dataset) error) (SearchResult, error) {
		matcher, err := NewSearchMatcher(req, SearchMatcherOptions{Context: ctx, FuzzyPersonNames: encoder})
		if err != nil {
			return SearchResult{}, err
		}
		for _, candidate := range candidates {
			ok, err := matcher.Match(candidate)
			if err != nil {
				return SearchResult{}, err
			}
			if !ok {
				continue
			}
			uid, _ := candidate.GetString(tags.StudyInstanceUID)
			if err := yield(Dataset{"0020000D": {VR: "UI", Value: []any{uid}}}); err != nil {
				return SearchResult{}, err
			}
		}
		return SearchResult{FuzzyMatchingApplied: matcher.FuzzyMatchingApplied()}, nil
	}
	return backend
}

func TestServerQIDOFuzzyMatchingWithSearchMatcher(t *testing.T) {
	candidates := []*object.Object{
		searchCandidate("1.2.10", "SMITH^JOHN", "20240101"),
		searchCandidate("1.2.11", "BROWN^ANNA", "20240102"),
	}
	tests := []struct {
		name        string
		encoder     fuzzy.FuzzyStr
		query       string
		wantUIDs    []string
		wantWarning bool
	}{
		{"fuzzy applied", fuzzy.Soundex{}, "/studies?PatientName=SMYTH%5EJON&fuzzymatching=true", []string{"1.2.10"}, false},
		{"literal without fuzzymatching", fuzzy.Soundex{}, "/studies?PatientName=SMYTH%5EJON", nil, false},
		{"encoder absent keeps warning", nil, "/studies?PatientName=SMYTH%5EJON&fuzzymatching=true", nil, true},
		{"literal still matches", fuzzy.Soundex{}, "/studies?PatientName=smith%5Ejohn&fuzzymatching=true", []string{"1.2.10"}, false},
		{"uid list", nil, "/studies?StudyInstanceUID=1.2.10,1.2.11", []string{"1.2.10", "1.2.11"}, false},
		{"tag key and date range", nil, "/studies?00080020=20240102-", []string{"1.2.11"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server, err := NewServer(ServerOptions{Backend: matcherSearchBackend(tc.encoder, candidates), AllowUnauthenticated: true})
			if err != nil {
				t.Fatal(err)
			}
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, tc.query, nil).WithContext(context.Background())
			request.Header.Set("Accept", "application/dicom+json")
			server.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d body %s", recorder.Code, recorder.Body.String())
			}
			warning := strings.Contains(strings.Join(recorder.Header().Values("Warning"), "\n"), "fuzzymatching parameter is not supported")
			if warning != tc.wantWarning {
				t.Fatalf("fuzzymatching Warning present = %v, want %v (%q)", warning, tc.wantWarning, recorder.Header().Values("Warning"))
			}
			var datasets []Dataset
			if err := json.Unmarshal(recorder.Body.Bytes(), &datasets); err != nil {
				t.Fatal(err)
			}
			var uids []string
			for _, dataset := range datasets {
				uids = append(uids, dataset["0020000D"].Value[0].(string))
			}
			if strings.Join(uids, ",") != strings.Join(tc.wantUIDs, ",") {
				t.Fatalf("UIDs = %v, want %v", uids, tc.wantUIDs)
			}
		})
	}
}

func TestSearchMatcherRejectsInvalidQueries(t *testing.T) {
	matcher, err := NewSearchMatcher(SearchRequest{Filters: []SearchFilter{{Key: "StudyDate", Values: []string{"2024-01-01"}}}}, SearchMatcherOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := matcher.Match(searchCandidate("1.2.10", "SMITH", "20240101")); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("malformed date error = %v, want ErrInvalidRequest", err)
	}
	if _, err := NewSearchMatcher(SearchRequest{Filters: []SearchFilter{{Key: "NotAKeyword", Values: []string{"x"}}}}, SearchMatcherOptions{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("unknown keyword error = %v, want ErrInvalidRequest", err)
	}
	matcher, err = NewSearchMatcher(SearchRequest{StudyInstanceUID: "1.2.10", FuzzyMatching: true}, SearchMatcherOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if matcher.FuzzyMatchingApplied() {
		t.Fatal("fuzzy matching reported without an encoder")
	}
	if ok, err := matcher.Match(searchCandidate("1.2.11", "SMITH", "20240101")); err != nil || ok {
		t.Fatalf("path UID mismatch = %v, %v", ok, err)
	}
}
