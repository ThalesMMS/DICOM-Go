package dicomweb

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
)

const (
	// DefaultSearchPageSize is the QIDO-RS limit of one page when
	// SearchPagerOptions.PageSize is zero.
	DefaultSearchPageSize = 100
	// DefaultMaxSearchPages bounds one pager when SearchPagerOptions.MaxPages
	// is zero.
	DefaultMaxSearchPages = 1000
)

var (
	// ErrSearchPageLimit reports that a search still had results after
	// SearchPagerOptions.MaxPages pages.
	ErrSearchPageLimit = errors.New("dicomweb: QIDO-RS search exceeded the page limit")
	// ErrSearchPagination reports a server whose pages do not advance: it
	// announced more results but returned none, returned a page without new
	// results, or sent a page whose size prevents a valid offset advance.
	ErrSearchPagination = errors.New("dicomweb: QIDO-RS pagination did not advance")
)

// SearchPagerOptions bounds a QIDO-RS pager.
type SearchPagerOptions struct {
	// PageSize is the limit requested for each page.
	PageSize int
	// MaxPages fails the pager with ErrSearchPageLimit when more pages remain.
	MaxPages int
	// MaxResults, when positive, stops after delivering this many results.
	MaxResults int
}

// SearchPage is one QIDO-RS response of a pager.
type SearchPage struct {
	// Datasets contains the results being delivered for the first time.
	Datasets []Dataset
	// Offset is the offset requested for this page.
	Offset int
	// Warnings holds the response's Warning header values.
	Warnings []string
	// MoreResults reports a PS3.18 Warning 299 that the server limited the
	// response and additional results can be requested.
	MoreResults bool
}

// SearchPager walks a QIDO-RS search with offset and limit (PS3.18
// 8.3.4.4). A page shorter than requested ends the search unless the server
// warned that more results remain, as servers do when they cap the limit; a
// full page is followed by another request. Each valid result UID at the
// searched level is delivered once, but the offset advances by all rows in
// the response. A SearchPager is not safe for concurrent use.
type SearchPager struct {
	client      Client
	build       func(url.Values) (*url.URL, error)
	params      url.Values
	opts        SearchPagerOptions
	identityTag string
	seen        map[string]struct{}
	previous    [sha256.Size]byte
	hasPrevious bool
	offset      int
	pages       int
	results     int
	done        bool
	err         error
}

// StudyPager returns a pager over a QIDO-RS study search. An offset in
// params selects the first page; limit and offset are then managed by the
// pager.
func (c Client) StudyPager(params url.Values, opts SearchPagerOptions) *SearchPager {
	return c.newSearchPager(c.Endpoint.StudySearchURL, "0020000D", params, opts)
}

// SeriesPager returns a pager over a QIDO-RS series search in one study.
func (c Client) SeriesPager(studyInstanceUID string, params url.Values, opts SearchPagerOptions) *SearchPager {
	return c.newSearchPager(func(values url.Values) (*url.URL, error) {
		return c.Endpoint.SeriesSearchURL(studyInstanceUID, values)
	}, "0020000E", params, opts)
}

// InstancePager returns a pager over a QIDO-RS instance search in one series.
func (c Client) InstancePager(studyInstanceUID, seriesInstanceUID string, params url.Values, opts SearchPagerOptions) *SearchPager {
	return c.newSearchPager(func(values url.Values) (*url.URL, error) {
		return c.Endpoint.InstanceSearchURL(studyInstanceUID, seriesInstanceUID, values)
	}, "00080018", params, opts)
}

func (c Client) newSearchPager(build func(url.Values) (*url.URL, error), identityTag string, params url.Values, opts SearchPagerOptions) *SearchPager {
	pager := &SearchPager{client: c, build: build, params: url.Values{}, opts: opts, identityTag: identityTag, seen: make(map[string]struct{})}
	for key, values := range params {
		pager.params[key] = append([]string(nil), values...)
	}
	if opts.PageSize < 0 || opts.MaxPages < 0 || opts.MaxResults < 0 {
		pager.err = errors.New("dicomweb: search pager options cannot be negative")
	}
	if pager.opts.PageSize == 0 {
		pager.opts.PageSize = DefaultSearchPageSize
	}
	if pager.opts.MaxPages == 0 {
		pager.opts.MaxPages = DefaultMaxSearchPages
	}
	if raw := strings.TrimSpace(pager.params.Get("offset")); raw != "" {
		offset, err := strconv.Atoi(raw)
		if err != nil || offset < 0 {
			pager.err = errors.New("dicomweb: search offset must be a non-negative integer")
		}
		pager.offset = offset
	}
	pager.params.Del("offset")
	pager.params.Del("limit")
	return pager
}

// Done reports whether the pager has no further pages.
func (p *SearchPager) Done() bool {
	return p.done
}

// Next requests the next page. It returns ok=false once the search is
// complete; an empty response ends the search without a page. After an error from the request itself the pager stays at the
// same offset, so Next may be called again; pagination failures end it.
func (p *SearchPager) Next(ctx context.Context) (page SearchPage, ok bool, err error) {
	if p.err != nil {
		return SearchPage{}, false, p.err
	}
	if p.done {
		return SearchPage{}, false, nil
	}
	if p.pages >= p.opts.MaxPages {
		p.done = true
		return SearchPage{}, false, ErrSearchPageLimit
	}
	if err := p.client.validateMetadataResponseOptions(); err != nil {
		return SearchPage{}, false, err
	}
	limit := p.opts.PageSize
	if p.opts.MaxResults > 0 && p.opts.MaxResults-p.results < limit {
		limit = p.opts.MaxResults - p.results
	}
	params := url.Values{}
	for key, values := range p.params {
		params[key] = values
	}
	params.Set("limit", strconv.Itoa(limit))
	if p.offset > 0 {
		params.Set("offset", strconv.Itoa(p.offset))
	}
	u, err := p.build(params)
	if err != nil {
		return SearchPage{}, false, err
	}
	datasets, resp, err := p.client.searchAt(ctx, u)
	if err != nil {
		return SearchPage{}, false, err
	}
	p.pages++
	page = SearchPage{Offset: p.offset, Warnings: resp.Header.Values("Warning")}
	page.MoreResults = searchWarnsMoreResults(page.Warnings)
	count := len(datasets)
	if count == 0 {
		p.done = true
		if page.MoreResults {
			return SearchPage{}, false, ErrSearchPagination
		}
		return SearchPage{}, false, nil
	}
	if count > limit || p.offset > math.MaxInt-count {
		p.done = true
		return SearchPage{}, false, ErrSearchPagination
	}
	page.Datasets, err = p.newResults(datasets)
	if err != nil {
		p.done = true
		return SearchPage{}, false, err
	}
	p.offset += count
	p.results += len(page.Datasets)
	switch {
	case p.opts.MaxResults > 0 && p.results >= p.opts.MaxResults:
		p.done = true
	case count < limit && !page.MoreResults:
		p.done = true
	}
	return page, true, nil
}

func (p *SearchPager) resultUID(dataset Dataset) string {
	element := dataset[p.identityTag]
	if element.VR == "UI" && len(element.Value) == 1 {
		if uid, ok := element.Value[0].(string); ok && core.IsValidUID(uid) {
			return uid
		}
	}
	return ""
}

func (p *SearchPager) newResults(datasets []Dataset) ([]Dataset, error) {
	// Results without an identity remain visible. A fingerprint of their
	// decoded page detects repeated bodies independent of the wire formatting.
	hasUnidentified := slices.ContainsFunc(datasets, func(dataset Dataset) bool {
		return p.resultUID(dataset) == ""
	})
	if hasUnidentified {
		hash := sha256.New()
		encoder := json.NewEncoder(hash)
		for _, dataset := range datasets {
			if err := encoder.Encode(dataset); err != nil {
				return nil, ErrSearchPagination
			}
		}
		var fingerprint [sha256.Size]byte
		hash.Sum(fingerprint[:0])
		if p.hasPrevious && fingerprint == p.previous {
			return nil, ErrSearchPagination
		}
		p.previous = fingerprint
	}
	p.hasPrevious = hasUnidentified

	results := datasets[:0]
	for _, dataset := range datasets {
		if uid := p.resultUID(dataset); uid != "" {
			if _, duplicate := p.seen[uid]; duplicate {
				continue
			}
			p.seen[uid] = struct{}{}
		}
		results = append(results, dataset)
	}
	clear(datasets[len(results):])
	if len(results) == 0 {
		return nil, ErrSearchPagination
	}
	return results, nil
}

// All collects every remaining page.
func (p *SearchPager) All(ctx context.Context) ([]Dataset, error) {
	var all []Dataset
	for {
		page, ok, err := p.Next(ctx)
		if err != nil {
			return all, err
		}
		if !ok {
			return all, nil
		}
		all = append(all, page.Datasets...)
	}
}

// searchWarnsMoreResults recognizes the PS3.18 Warning 299 that a QIDO-RS
// response was limited and more results can be requested.
func searchWarnsMoreResults(warnings []string) bool {
	for _, warning := range warnings {
		warning = strings.TrimSpace(warning)
		if !strings.HasPrefix(warning, "299") {
			continue
		}
		text := strings.ToLower(warning)
		if strings.Contains(text, "additional result") || strings.Contains(text, "exceed") || strings.Contains(text, "truncat") {
			return true
		}
	}
	return false
}
