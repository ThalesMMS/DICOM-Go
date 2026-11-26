package dicomweb

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// PS3.18 10.6.1 defines series and instance searches that are not limited to
// one study ("/series", "/instances") and an instance search limited to one
// study but not to one series ("/studies/{study}/instances"). They take the
// same match, includefield, fuzzymatching, limit and offset parameters as
// the scoped searches; a Study or Series Instance UID becomes an ordinary
// matching key in the query.

// AllSeriesSearchURL builds the QIDO-RS URL that searches series in every
// study.
// A base or QIDO path that ends in /studies names the root above it.
func (e Endpoint) AllSeriesSearchURL(params url.Values) (*url.URL, error) {
	return e.serviceRootURL(e.QIDOPath, []string{"series"}, params)
}

// AllInstanceSearchURL builds the QIDO-RS URL that searches instances in
// every study and series. A base or QIDO path that ends in /studies names
// the root above it.
func (e Endpoint) AllInstanceSearchURL(params url.Values) (*url.URL, error) {
	return e.serviceRootURL(e.QIDOPath, []string{"instances"}, params)
}

// StudyInstanceSearchURL builds the QIDO-RS URL that searches instances in
// every series of one study.
func (e Endpoint) StudyInstanceSearchURL(studyInstanceUID string, params url.Values) (*url.URL, error) {
	studyInstanceUID = strings.TrimSpace(studyInstanceUID)
	if studyInstanceUID == "" {
		return nil, &Error{Kind: ErrorKindInvalidEndpoint, Err: fmt.Errorf("study instance UID is required")}
	}
	return e.serviceURL(e.QIDOPath, []string{"studies", studyInstanceUID, "instances"}, params)
}

// SearchAllSeries performs a QIDO-RS series search across every study and
// returns metadata in the package's Dataset shape.
func (c Client) SearchAllSeries(ctx context.Context, params url.Values) ([]Dataset, error) {
	return c.searchBuilt(ctx, func() (*url.URL, error) { return c.Endpoint.AllSeriesSearchURL(params) })
}

// SearchAllInstances performs a QIDO-RS instance search across every study
// and series and returns metadata in the package's Dataset shape.
func (c Client) SearchAllInstances(ctx context.Context, params url.Values) ([]Dataset, error) {
	return c.searchBuilt(ctx, func() (*url.URL, error) { return c.Endpoint.AllInstanceSearchURL(params) })
}

// SearchStudyInstances performs a QIDO-RS instance search across every
// series of one study and returns metadata in the package's Dataset shape.
func (c Client) SearchStudyInstances(ctx context.Context, studyInstanceUID string, params url.Values) ([]Dataset, error) {
	return c.searchBuilt(ctx, func() (*url.URL, error) { return c.Endpoint.StudyInstanceSearchURL(studyInstanceUID, params) })
}

func (c Client) searchBuilt(ctx context.Context, build func() (*url.URL, error)) ([]Dataset, error) {
	if err := c.validateMetadataResponseOptions(); err != nil {
		return nil, err
	}
	u, err := build()
	if err != nil {
		return nil, err
	}
	datasets, _, err := c.searchAt(ctx, u)
	return datasets, err
}

// AllSeriesPager returns a pager over a QIDO-RS series search across every
// study.
func (c Client) AllSeriesPager(params url.Values, opts SearchPagerOptions) *SearchPager {
	return c.newSearchPager(c.Endpoint.AllSeriesSearchURL, "0020000E", params, opts)
}

// AllInstancePager returns a pager over a QIDO-RS instance search across
// every study and series.
func (c Client) AllInstancePager(params url.Values, opts SearchPagerOptions) *SearchPager {
	return c.newSearchPager(c.Endpoint.AllInstanceSearchURL, "00080018", params, opts)
}

// StudyInstancePager returns a pager over a QIDO-RS instance search across
// every series of one study.
func (c Client) StudyInstancePager(studyInstanceUID string, params url.Values, opts SearchPagerOptions) *SearchPager {
	return c.newSearchPager(func(values url.Values) (*url.URL, error) {
		return c.Endpoint.StudyInstanceSearchURL(studyInstanceUID, values)
	}, "00080018", params, opts)
}

// SearchAllSeriesStream is the streaming form of SearchAllSeries, with the
// contract of SearchStudiesStream.
func (c Client) SearchAllSeriesStream(ctx context.Context, params url.Values, handle func(Dataset) error) (Response, error) {
	u, err := c.Endpoint.AllSeriesSearchURL(params)
	if err != nil {
		return Response{}, err
	}
	return c.streamMetadataAt(ctx, u, handle)
}

// SearchAllInstancesStream is the streaming form of SearchAllInstances, with
// the contract of SearchStudiesStream.
func (c Client) SearchAllInstancesStream(ctx context.Context, params url.Values, handle func(Dataset) error) (Response, error) {
	u, err := c.Endpoint.AllInstanceSearchURL(params)
	if err != nil {
		return Response{}, err
	}
	return c.streamMetadataAt(ctx, u, handle)
}

// SearchStudyInstancesStream is the streaming form of SearchStudyInstances,
// with the contract of SearchStudiesStream.
func (c Client) SearchStudyInstancesStream(ctx context.Context, studyInstanceUID string, params url.Values, handle func(Dataset) error) (Response, error) {
	u, err := c.Endpoint.StudyInstanceSearchURL(studyInstanceUID, params)
	if err != nil {
		return Response{}, err
	}
	return c.streamMetadataAt(ctx, u, handle)
}
