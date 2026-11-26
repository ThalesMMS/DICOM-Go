package dicomweb

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
)

func TestEndpointUnscopedSearchURLs(t *testing.T) {
	endpoint := Endpoint{BaseURL: "https://pacs.example.test/dicom-web/", QIDOPath: "/qido-rs/studies/"}
	params := url.Values{"Modality": {"CT"}}
	tests := []struct {
		name  string
		build func() (*url.URL, error)
		path  string
	}{
		{"all series", func() (*url.URL, error) { return endpoint.AllSeriesSearchURL(params) }, "/dicom-web/qido-rs/series"},
		{"all instances", func() (*url.URL, error) { return endpoint.AllInstanceSearchURL(params) }, "/dicom-web/qido-rs/instances"},
		{"study instances", func() (*url.URL, error) { return endpoint.StudyInstanceSearchURL(" 1.2.3/4 ", params) }, "/dicom-web/qido-rs/studies/1.2.3%2F4/instances"},
	}
	for _, test := range tests {
		u, err := test.build()
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		if u.Path != test.path || u.Query().Get("Modality") != "CT" {
			t.Fatalf("%s: URL = %s, want path %s with the Modality key", test.name, u, test.path)
		}
	}
	// The root above a base that ends in /studies, and a bare origin.
	for base, want := range map[string]string{
		"https://pacs.example.test/dicom-web/studies": "/dicom-web/instances",
		"https://pacs.example.test":                   "/instances",
		"https://pacs.example.test/studies/":          "/instances",
	} {
		u, err := Endpoint{BaseURL: base}.AllInstanceSearchURL(nil)
		if err != nil || u.Path != want {
			t.Fatalf("AllInstanceSearchURL with base %s = %v (%v), want path %s", base, u, err, want)
		}
	}
	_, err := endpoint.StudyInstanceSearchURL(" \t ", nil)
	assertErrorKind(t, err, ErrorKindInvalidEndpoint)
	_, err = Client{Endpoint: endpoint}.SearchStudyInstances(context.Background(), "", nil)
	assertErrorKind(t, err, ErrorKindInvalidEndpoint)
}

// TestUnscopedSearchesReachTheServerResources runs each unscoped search
// against the package server and checks the resource, scope and keys it
// receives, the decoded results, paging and streaming.
func TestUnscopedSearchesReachTheServerResources(t *testing.T) {
	var (
		mu       sync.Mutex
		requests []SearchRequest
	)
	const total = 5
	backend := completeServerTestBackend()
	backend.search = func(_ context.Context, request SearchRequest, yield func(Dataset) error) (SearchResult, error) {
		mu.Lock()
		requests = append(requests, request)
		mu.Unlock()
		end := total
		if request.MaximumResults > 0 && request.Offset+request.MaximumResults < end {
			end = request.Offset + request.MaximumResults
		}
		for index := request.Offset; index < end; index++ {
			study := "1.2.3"
			if request.StudyInstanceUID != "" {
				study = request.StudyInstanceUID
			}
			if err := yield(Dataset{
				"0020000D": {VR: "UI", Value: []any{study}},
				"0020000E": {VR: "UI", Value: []any{fmt.Sprintf("1.2.3.%d", index+1)}},
				"00080018": {VR: "UI", Value: []any{fmt.Sprintf("1.2.3.%d.1", index+1)}},
				"00080060": {VR: "CS", Value: []any{"CT"}},
			}); err != nil {
				return SearchResult{}, err
			}
		}
		remaining := total - end
		return SearchResult{Remaining: &remaining}, nil
	}
	_, httpServer := newOpenTestServer(t, backend, nil)
	client := Client{Endpoint: Endpoint{BaseURL: httpServer.URL}}
	ctx := context.Background()
	last := func() SearchRequest {
		mu.Lock()
		defer mu.Unlock()
		return requests[len(requests)-1]
	}
	filter := func(request SearchRequest, key string) string {
		for _, f := range request.Filters {
			if f.Key == key {
				return strings.Join(f.Values, `\`)
			}
		}
		return ""
	}

	seriesParams, err := SeriesSearchParams(SeriesSearchCriteria{Modality: "CT", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	series, err := client.SearchAllSeries(ctx, seriesParams)
	if err != nil {
		t.Fatalf("SearchAllSeries: %v", err)
	}
	request := last()
	if request.Level != SearchLevelSeries || request.StudyInstanceUID != "" || filter(request, "Modality") != "CT" || request.Limit != 10 {
		t.Fatalf("SearchAllSeries reached the server as %+v", request)
	}
	matches := SeriesMatchesFromDatasets(series)
	if len(matches) != total || matches[0].StudyInstanceUID != "1.2.3" || matches[0].SeriesInstanceUID != "1.2.3.1" {
		t.Fatalf("SearchAllSeries matches = %+v", matches)
	}

	instanceParams, err := InstanceSearchParams(InstanceSearchCriteria{Modality: "CT"})
	if err != nil {
		t.Fatal(err)
	}
	instanceParams.Set("SeriesInstanceUID", "1.2.3.2")
	instances, err := client.SearchAllInstances(ctx, instanceParams)
	if err != nil {
		t.Fatalf("SearchAllInstances: %v", err)
	}
	request = last()
	if request.Level != SearchLevelInstance || request.StudyInstanceUID != "" || request.SeriesInstanceUID != "" || filter(request, "SeriesInstanceUID") != "1.2.3.2" {
		t.Fatalf("SearchAllInstances reached the server as %+v", request)
	}
	if got := InstanceMatchesFromDatasets(instances); len(got) != total || got[0].SOPInstanceUID != "1.2.3.1.1" {
		t.Fatalf("SearchAllInstances matches = %+v", got)
	}

	inStudy, err := client.SearchStudyInstances(ctx, "1.2.9", instanceParams)
	if err != nil {
		t.Fatalf("SearchStudyInstances: %v", err)
	}
	request = last()
	if request.Level != SearchLevelInstance || request.StudyInstanceUID != "1.2.9" || request.SeriesInstanceUID != "" {
		t.Fatalf("SearchStudyInstances reached the server as %+v", request)
	}
	if got := InstanceMatchesFromDatasets(inStudy); len(got) != total || got[0].StudyInstanceUID != "1.2.9" {
		t.Fatalf("SearchStudyInstances matches = %+v", got)
	}

	pagers := map[string]struct {
		pager *SearchPager
		level SearchLevel
		study string
		tag   string
	}{
		"all series":      {client.AllSeriesPager(url.Values{"Modality": {"CT"}}, SearchPagerOptions{PageSize: 2}), SearchLevelSeries, "", "0020000E"},
		"all instances":   {client.AllInstancePager(nil, SearchPagerOptions{PageSize: 2}), SearchLevelInstance, "", "00080018"},
		"study instances": {client.StudyInstancePager("1.2.9", nil, SearchPagerOptions{PageSize: 2}), SearchLevelInstance, "1.2.9", "00080018"},
	}
	for name, test := range pagers {
		mu.Lock()
		before := len(requests)
		mu.Unlock()
		all, err := test.pager.All(ctx)
		if err != nil {
			t.Fatalf("%s pager: %v", name, err)
		}
		seen := map[string]bool{}
		for _, dataset := range all {
			seen[dataset[test.tag].Value[0].(string)] = true
		}
		mu.Lock()
		walked := requests[before:]
		mu.Unlock()
		if len(all) != total || len(seen) != total || len(walked) != 3 {
			t.Fatalf("%s pager delivered %d results (%d distinct) in %d requests", name, len(all), len(seen), len(walked))
		}
		for index, request := range walked {
			if request.Level != test.level || request.StudyInstanceUID != test.study || request.Offset != 2*index || request.Limit != 2 {
				t.Fatalf("%s page %d reached the server as %+v", name, index, request)
			}
		}
	}

	streamed := 0
	for name, search := range map[string]func(func(Dataset) error) (Response, error){
		"all series":    func(h func(Dataset) error) (Response, error) { return client.SearchAllSeriesStream(ctx, nil, h) },
		"all instances": func(h func(Dataset) error) (Response, error) { return client.SearchAllInstancesStream(ctx, nil, h) },
		"study instances": func(h func(Dataset) error) (Response, error) {
			return client.SearchStudyInstancesStream(ctx, "1.2.9", nil, h)
		},
	} {
		count := 0
		if _, err := search(func(Dataset) error { count++; return nil }); err != nil || count != total {
			t.Fatalf("%s stream delivered %d datasets (%v)", name, count, err)
		}
		streamed += count
	}
	if streamed != 3*total {
		t.Fatalf("the streams delivered %d datasets", streamed)
	}
}
