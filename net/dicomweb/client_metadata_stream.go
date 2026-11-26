package dicomweb

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/ThalesMMS/dicom-go/dicomjson"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
)

// SearchStudiesStream performs a JSON-only QIDO request and synchronously
// delivers each validated dataset. The response contains headers, not a Body.
// Earlier callbacks remain consumed after a later decode/limit/callback error.
// Streaming requests bypass ResponseCache and do not replay consumed callbacks.
func (c Client) SearchStudiesStream(ctx context.Context, params url.Values, handle func(Dataset) error) (Response, error) {
	u, err := c.Endpoint.StudySearchURL(params)
	if err != nil {
		return Response{}, err
	}
	return c.streamMetadataAt(ctx, u, handle)
}
func (c Client) SearchSeriesStream(ctx context.Context, studyUID string, params url.Values, handle func(Dataset) error) (Response, error) {
	u, err := c.Endpoint.SeriesSearchURL(studyUID, params)
	if err != nil {
		return Response{}, err
	}
	return c.streamMetadataAt(ctx, u, handle)
}
func (c Client) SearchInstancesStream(ctx context.Context, studyUID, seriesUID string, params url.Values, handle func(Dataset) error) (Response, error) {
	u, err := c.Endpoint.InstanceSearchURL(studyUID, seriesUID, params)
	if err != nil {
		return Response{}, err
	}
	return c.streamMetadataAt(ctx, u, handle)
}

// StudyMetadataStream streams JSON WADO-RS metadata. It shares the explicit
// partial-consumption, body ownership and JSON-only contract of SearchStudiesStream.
func (c Client) StudyMetadataStream(ctx context.Context, studyUID string, handle func(Dataset) error) (Response, error) {
	u, err := c.Endpoint.StudyMetadataURL(studyUID)
	if err != nil {
		return Response{}, err
	}
	return c.streamScopedMetadataAt(ctx, u, InstanceRef{StudyInstanceUID: studyUID}, handle)
}
func (c Client) SeriesMetadataStream(ctx context.Context, studyUID, seriesUID string, handle func(Dataset) error) (Response, error) {
	u, err := c.Endpoint.SeriesMetadataURL(studyUID, seriesUID)
	if err != nil {
		return Response{}, err
	}
	return c.streamScopedMetadataAt(ctx, u, InstanceRef{StudyInstanceUID: studyUID, SeriesInstanceUID: seriesUID}, handle)
}
func (c Client) InstanceMetadataStream(ctx context.Context, ref InstanceRef, handle func(Dataset) error) (Response, error) {
	u, err := c.Endpoint.InstanceMetadataURL(ref)
	if err != nil {
		return Response{}, err
	}
	return c.streamScopedMetadataAt(ctx, u, ref, handle)
}

// StudyMetadataRefsStream streams the instance references of JSON WADO-RS
// study metadata: each reference is delivered as soon as its dataset is
// decoded and bound to the study, with the same identity rules and in the
// same order as StudyMetadata returns them. A caller can observe a large
// listing arriving, for example to bound it by its progress rather than as a
// whole. It admits the JSON listings StudyMetadata admits: the response body
// is bounded as a whole by MaxBodyBytes, element and sequence-item counts are
// bounded per instance rather than per listing, and an empty body is an empty
// listing. Otherwise it shares the contract of StudyMetadataStream.
func (c Client) StudyMetadataRefsStream(ctx context.Context, studyUID string, handle func(InstanceRef) error) (Response, error) {
	u, err := c.Endpoint.StudyMetadataURL(studyUID)
	if err != nil {
		return Response{}, err
	}
	return c.streamScopedMetadataRefsAt(ctx, u, InstanceRef{StudyInstanceUID: studyUID}, metadataDecodeListing, refsHandler(handle))
}

// SeriesMetadataRefsStream is StudyMetadataRefsStream for series metadata.
func (c Client) SeriesMetadataRefsStream(ctx context.Context, studyUID, seriesUID string, handle func(InstanceRef) error) (Response, error) {
	u, err := c.Endpoint.SeriesMetadataURL(studyUID, seriesUID)
	if err != nil {
		return Response{}, err
	}
	return c.streamScopedMetadataRefsAt(ctx, u, InstanceRef{StudyInstanceUID: studyUID, SeriesInstanceUID: seriesUID}, metadataDecodeListing, refsHandler(handle))
}

// metadataDecodeMode selects how a streamed JSON array is admitted.
type metadataDecodeMode int

const (
	// metadataDecodeRepresented validates each dataset's represented shape
	// with element and item budgets shared by the whole response.
	metadataDecodeRepresented metadataDecodeMode = iota
	// metadataDecodeListing admits a WADO-RS instance listing as the
	// resident metadata methods do, bounding element and sequence-item
	// counts per instance.
	metadataDecodeListing
)

// Per-instance bounds of a streamed instance listing: the budgets the
// represented streams apply to a whole response, applied to each instance.
const (
	metadataInstanceMaxElements      = 1_000_000
	metadataInstanceMaxSequenceItems = 1_000_000
)

// metadataInstanceBudget counts the elements and sequence items of one
// instance of a listing, nested sequence items included.
type metadataInstanceBudget struct{ elements, items int }

func (b *metadataInstanceBudget) dataset(dataset Dataset) error {
	for _, element := range dataset {
		if err := b.element(element.VR, element.Value); err != nil {
			return err
		}
	}
	return nil
}

func (b *metadataInstanceBudget) element(vr string, values []any) error {
	if b.elements++; b.elements > metadataInstanceMaxElements {
		return fmt.Errorf("%w: got more than %d in one instance", dicomjson.ErrMaxElementsExceeded, metadataInstanceMaxElements)
	}
	if !strings.EqualFold(strings.TrimSpace(vr), "SQ") {
		return nil
	}
	for _, value := range values {
		if b.items++; b.items > metadataInstanceMaxSequenceItems {
			return fmt.Errorf("%w: got more than %d in one instance", dicomjson.ErrMaxSequenceItemsExceeded, metadataInstanceMaxSequenceItems)
		}
		item, _ := value.(map[string]any)
		for _, attribute := range item {
			nested, _ := attribute.(map[string]any)
			nestedVR, _ := nested["vr"].(string)
			nestedValues, _ := nested["Value"].([]any)
			if err := b.element(nestedVR, nestedValues); err != nil {
				return err
			}
		}
	}
	return nil
}

func refsHandler(handle func(InstanceRef) error) func(InstanceRef, Dataset) error {
	if handle == nil {
		return nil
	}
	return func(ref InstanceRef, _ Dataset) error { return handle(ref) }
}

func (c Client) streamScopedMetadataAt(ctx context.Context, u *url.URL, scope InstanceRef, handle func(Dataset) error) (Response, error) {
	if handle == nil {
		return c.streamMetadataAt(ctx, u, nil)
	}
	return c.streamScopedMetadataRefsAt(ctx, u, scope, metadataDecodeRepresented, func(_ InstanceRef, dataset Dataset) error { return handle(dataset) })
}

func (c Client) streamScopedMetadataRefsAt(ctx context.Context, u *url.URL, scope InstanceRef, mode metadataDecodeMode, handle func(InstanceRef, Dataset) error) (Response, error) {
	if handle == nil {
		return c.streamMetadataAt(ctx, u, nil)
	}
	delivered := false
	var seen metadataIdentities
	if scope.SeriesInstanceUID == "" {
		seen = make(metadataIdentities)
	}
	return c.streamMetadataWith(ctx, u, mode, func(dataset Dataset) error {
		if scope.SOPInstanceUID != "" && delivered {
			return fmt.Errorf("%w: instance response contains more than one dataset", ErrInvalidMetadataIdentity)
		}
		ref, err := scopedMetadataRef(dataset, scope)
		if err != nil {
			return err
		}
		if err := seen.record(ref); err != nil {
			return err
		}
		delivered = true
		return handle(ref, dataset)
	})
}

func (c Client) streamMetadataAt(ctx context.Context, u *url.URL, handle func(Dataset) error) (Response, error) {
	return c.streamMetadataWith(ctx, u, metadataDecodeRepresented, handle)
}

// metadataBodyReader remembers the first error reading a response body other
// than its end: a connection cut or a deadline in the middle of the body is a
// failure of the request, not of the representation.
type metadataBodyReader struct {
	reader io.Reader
	err    error
}

func (r *metadataBodyReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && r.err == nil {
		r.err = err
	}
	return n, err
}

// metadataReadError reports a failed read of a metadata response body as the
// resident methods do: a request failure carrying the transport cause, with
// the response status. The body limit already comes as one.
func metadataReadError(response Response, err error) error {
	var webErr *Error
	if errors.As(err, &webErr) {
		return err
	}
	return newDICOMwebError(ErrorKindRequestFailure, response.URL, response.StatusCode, err)
}

// emptyJSONBody consumes the leading JSON whitespace of body and reports
// whether nothing else follows. The first other byte stays unread.
func emptyJSONBody(body *bufio.Reader) (bool, error) {
	for {
		b, err := body.ReadByte()
		if errors.Is(err, io.EOF) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		switch b {
		case ' ', '\t', '\r', '\n':
			continue
		}
		return false, body.UnreadByte()
	}
}

func (c Client) streamMetadataWith(ctx context.Context, u *url.URL, mode metadataDecodeMode, handle func(Dataset) error) (Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if handle == nil {
		return Response{}, newDICOMwebError(ErrorKindRequestFailure, "", 0, fmt.Errorf("nil metadata handler"))
	}
	if err := c.validateMetadataResponseOptions(); err != nil {
		return Response{}, err
	}
	if !c.metadataMediaTypeConfigured(MetadataMediaTypeDICOMJSON) {
		return Response{}, newDICOMwebError(ErrorKindRequestFailure, "", 0, fmt.Errorf("JSON streaming requires the JSON metadata representation"))
	}
	raw, cancel, err := c.doHTTP(ctx, http.MethodGet, u, nil, "", acceptDICOMJSON)
	if err != nil {
		return Response{}, err
	}
	defer func() {
		_ = raw.Body.Close()
		if cancel != nil {
			cancel()
		}
	}()
	response := responseFromHTTP(u.String(), raw)
	if raw.StatusCode == http.StatusNoContent {
		return response, nil
	}
	if raw.StatusCode < 200 || raw.StatusCode >= 300 {
		return response, statusError(response)
	}
	limits, err := responseLimits(c.Options)
	if err != nil {
		return response, err
	}
	maximum := c.Options.MaxBodyBytes
	if maximum <= 0 {
		maximum = DefaultMaxBodyBytes
	}
	read := &metadataBodyReader{reader: &responseBodyLimitReader{reader: raw.Body, remaining: maximum, limit: maximum, resp: response}}
	body := bufio.NewReader(read)
	// An empty body is an empty array, as the resident metadata methods
	// read it, with or without a Content-Type.
	empty, err := emptyJSONBody(body)
	if err != nil {
		return response, metadataReadError(response, err)
	}
	contentTypes := raw.Header.Values("Content-Type")
	if empty && len(contentTypes) == 0 {
		return response, nil
	}
	contentType, err := responseContentType(contentTypes)
	if err != nil {
		return response, newDICOMwebError(ErrorKindDecodeResponse, response.URL, response.StatusCode, err)
	}
	media, params, err := parseResponseMediaType(contentType)
	if err != nil || !mediaAllowed(strings.ToLower(media), metadataJSONResponseMediaTypes, limits.allowedMedia) || !metadataCharsetSupported(params) || !metadataParametersSupported(params, "charset", "transfer-syntax") {
		return response, newDICOMwebError(ErrorKindDecodeResponse, response.URL, response.StatusCode, fmt.Errorf("unsupported JSON metadata Content-Type"))
	}
	syntax, err := metadataTransferSyntax(params)
	if err != nil {
		return response, newDICOMwebError(ErrorKindDecodeResponse, response.URL, response.StatusCode, err)
	}
	if empty {
		return response, nil
	}
	maximumDatasets := c.metadataDatasetLimit(limits)
	requestContext := ctx
	if raw.Request != nil {
		requestContext = raw.Request.Context()
	}
	count := 0
	next := func(dataset Dataset) error {
		if count >= maximumDatasets {
			return &ResponseDecodeError{Kind: ResponseDecodePartLimit, Limit: int64(maximumDatasets), Value: int64(count + 1)}
		}
		count++
		return handle(dataset)
	}
	if mode == metadataDecodeListing {
		// The resident methods' admission: the body bounds the listing as a
		// whole, and each instance's elements and items bound it alone.
		decoder := dicomjson.NewDecoder(body, std.Dictionary, dicomjson.UnmarshalOptions{Limits: dicomjson.Limits{MaxJSONBytes: maximum}})
		err = decoder.DecodeRawDatasetsContext(requestContext, func(dataset Dataset) error {
			var budget metadataInstanceBudget
			if err := budget.dataset(dataset); err != nil {
				return err
			}
			return next(dataset)
		})
	} else {
		decoder := dicomjson.NewDecoder(body, std.Dictionary, dicomjson.UnmarshalOptions{TransferSyntax: syntax, Limits: dicomjson.Limits{MaxJSONBytes: maximum, MaxInlineBinaryBytes: limits.maxPartBytes, MaxTotalBinaryBytes: maximum, MaxElements: 1_000_000, MaxSequenceItems: 1_000_000}})
		err = decoder.DecodeDatasetsContext(requestContext, next)
	}
	if err != nil && read.err != nil {
		// The body stopped arriving: the listing failed with the transport,
		// not with its representation.
		return response, metadataReadError(response, read.err)
	}
	if err != nil {
		return response, newDICOMwebError(ErrorKindDecodeResponse, response.URL, response.StatusCode, err)
	}
	return response, nil
}
