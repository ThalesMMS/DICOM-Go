package dicomweb

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"sync/atomic"
	"testing"
)

func TestSearchPagerDeduplicatesAtTheRequestedLevel(t *testing.T) {
	for _, test := range []struct {
		name  string
		tag   string
		pager func(Client, url.Values, SearchPagerOptions) *SearchPager
	}{
		{"study", "0020000D", Client.StudyPager},
		{"series", "0020000E", func(c Client, p url.Values, opts SearchPagerOptions) *SearchPager {
			return c.SeriesPager("2.25.800", p, opts)
		}},
		{"instance", "00080018", func(c Client, p url.Values, opts SearchPagerOptions) *SearchPager {
			return c.InstancePager("2.25.800", "2.25.801", p, opts)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			const a, b, c, d = "2.25.81", "2.25.82", "2.25.83", "2.25.84"
			pages := []struct {
				offset string
				limit  string
				rows   []string
				want   []string
			}{
				{"5", "3", []string{a, b, a}, []string{a, b}},
				{"8", "2", []string{b, c}, []string{c}},
				{"10", "1", []string{d}, []string{d}},
			}
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				index := int(calls.Add(1)) - 1
				if index >= len(pages) {
					t.Errorf("unexpected extra request: %s", r.URL.RawQuery)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				spec := pages[index]
				if params := r.URL.Query(); params.Get("offset") != spec.offset || params.Get("limit") != spec.limit {
					t.Errorf("request %d: got %v, want offset %s and limit %s", index, params, spec.offset, spec.limit)
				}
				datasets := make([]Dataset, 0, len(spec.rows))
				for _, uid := range spec.rows {
					dataset := Dataset{
						"0020000D": {VR: "UI", Value: []any{"2.25.800"}},
						"0020000E": {VR: "UI", Value: []any{"2.25.801"}},
						test.tag:   {VR: "UI", Value: []any{uid}},
					}
					datasets = append(datasets, dataset)
				}
				w.Header().Set("Content-Type", "application/dicom+json")
				if err := json.NewEncoder(w).Encode(datasets); err != nil {
					t.Errorf("encode fixture: %v", err)
				}
			}))
			t.Cleanup(server.Close)
			client := Client{Endpoint: Endpoint{BaseURL: server.URL}}
			pager := test.pager(client, url.Values{"offset": {"5"}}, SearchPagerOptions{PageSize: 3, MaxResults: 4})
			for _, spec := range pages {
				page, ok, err := pager.Next(context.Background())
				if err != nil || !ok || strconv.Itoa(page.Offset) != spec.offset {
					t.Fatalf("Next = %+v, %v, %v", page, ok, err)
				}
				var got []string
				for _, dataset := range page.Datasets {
					got = append(got, dataset[test.tag].Value[0].(string))
				}
				if !reflect.DeepEqual(got, spec.want) {
					t.Fatalf("UIDs = %v; want %v", got, spec.want)
				}
			}
			if _, ok, err := pager.Next(context.Background()); ok || err != nil || !pager.Done() || calls.Load() != 3 {
				t.Fatalf("after result cap: ok=%v done=%v requests=%d err=%v", ok, pager.Done(), calls.Load(), err)
			}
		})
	}
}

func TestSearchPagerStopsOnRepeatedIdentitiesIndependentOfWireBytes(t *testing.T) {
	const first = `[{"0020000D":{"vr":"UI","Value":["2.25.1"]}},{"0020000D":{"vr":"UI","Value":["2.25.2"]}}]`
	for _, test := range []struct {
		name  string
		pages []string
	}{
		{"same_identities_with_new_metadata_and_order", []string{first, `[ { "0020000D": {"vr":"UI","Value":["2.25.2"]}, "00080060": {"vr":"CS","Value":["CT"]} }, {"0020000D":{"vr":"UI","Value":["2.25.1"]}} ]`}},
		{"unidentified_row_with_different_formatting", []string{
			`[{"00080060":{"vr":"CS","Value":["CT"]}},{"00080050":{"vr":"SH","Value":["ANONYMOUS"]}}]`,
			`[ {"00080060": {"Value": ["CT"], "vr": "CS"}}, {"00080050":{"Value":["ANONYMOUS"],"vr":"SH"}} ]`,
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				index := int(calls.Add(1)) - 1
				if index >= len(test.pages) {
					t.Error("pager fetched beyond the repeated page")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/dicom+json")
				_, _ = io.WriteString(w, test.pages[index])
			}))
			t.Cleanup(server.Close)
			pager := (Client{Endpoint: Endpoint{BaseURL: server.URL}}).StudyPager(nil, SearchPagerOptions{PageSize: 2})
			results, err := pager.All(context.Background())
			if !errors.Is(err, ErrSearchPagination) || len(results) != 2 || calls.Load() != 2 || !pager.Done() {
				t.Fatalf("All = %d results, %v; requests=%d done=%v", len(results), err, calls.Load(), pager.Done())
			}
		})
	}
}

func TestSearchPagerRejectsSizeAndOffsetViolations(t *testing.T) {
	for _, test := range []struct {
		name   string
		offset string
		body   string
	}{
		{"oversized", "0", `[{},{}]`},
		{"offset_overflow", strconv.Itoa(math.MaxInt), `[{}]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/dicom+json")
				_, _ = io.WriteString(w, test.body)
			}))
			t.Cleanup(server.Close)
			client := Client{Endpoint: Endpoint{BaseURL: server.URL}}
			pager := client.StudyPager(url.Values{"offset": {test.offset}}, SearchPagerOptions{PageSize: 1})
			page, ok, err := pager.Next(context.Background())
			if !errors.Is(err, ErrSearchPagination) || ok || len(page.Datasets) != 0 || !pager.Done() {
				t.Fatalf("Next = %+v, %v, %v; done=%v", page, ok, err, pager.Done())
			}
		})
	}
}
