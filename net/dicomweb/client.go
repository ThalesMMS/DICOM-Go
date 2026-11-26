package dicomweb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"time"

	"github.com/ThalesMMS/dicom-go/transfer"
)

const (
	acceptDICOMJSON         = "application/dicom+json, application/json;q=0.9"
	acceptDICOM             = `application/dicom, multipart/related; type="application/dicom"`
	defaultMaxMetadataParts = 10_000
)

// Verify performs a light QIDO-RS study request to verify endpoint reachability.
func (c Client) Verify(ctx context.Context) (VerifyResult, error) {
	if err := c.validateMetadataResponseOptions(); err != nil {
		return VerifyResult{}, err
	}
	params := url.Values{}
	params.Set("limit", "1")
	u, err := c.Endpoint.StudySearchURL(params)
	if err != nil {
		return VerifyResult{}, err
	}
	started := time.Now()
	resp, err := c.doGETUncached(ctx, u, c.metadataAcceptHeader())
	result := VerifyResult{Response: resp, Duration: time.Since(started), StartedAt: started.UTC()}
	if err != nil {
		return result, err
	}
	if resp.StatusCode == http.StatusNoContent {
		return result, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return result, statusError(resp)
	}
	if len(strings.TrimSpace(string(resp.Body))) == 0 {
		return result, nil
	}
	if _, err := c.datasetsFromMetadataResponse(ctx, resp, false); err != nil {
		return result, newDICOMwebError(ErrorKindDecodeResponse, resp.URL, resp.StatusCode, err)
	}
	return result, nil
}

// SearchStudies performs a QIDO-RS study search and returns metadata in the
// package's Dataset shape, decoding either negotiated DICOM JSON or DICOM XML.
func (c Client) SearchStudies(ctx context.Context, params url.Values) ([]Dataset, error) {
	if err := c.validateMetadataResponseOptions(); err != nil {
		return nil, err
	}
	u, err := c.Endpoint.StudySearchURL(params)
	if err != nil {
		return nil, err
	}
	datasets, _, err := c.searchAt(ctx, u)
	return datasets, err
}

// searchAt performs one QIDO-RS request and also returns the response, whose
// Warning headers drive pagination.
func (c Client) searchAt(ctx context.Context, u *url.URL) ([]Dataset, Response, error) {
	result, err := c.doGET(ctx, u, c.metadataAcceptHeader())
	resp := result.Response
	if err != nil {
		return nil, resp, err
	}
	if resp.StatusCode == http.StatusNoContent {
		return nil, resp, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp, statusError(resp)
	}
	datasets, err := c.datasetsFromMetadataResponse(ctx, resp, false)
	if err != nil {
		return nil, resp, newDICOMwebError(ErrorKindDecodeResponse, resp.URL, resp.StatusCode, err)
	}
	return datasets, resp, nil
}

// SearchSeries performs a QIDO-RS series search for one study and returns
// metadata in the package's Dataset shape.
func (c Client) SearchSeries(ctx context.Context, studyInstanceUID string, params url.Values) ([]Dataset, error) {
	if err := c.validateMetadataResponseOptions(); err != nil {
		return nil, err
	}
	u, err := c.Endpoint.SeriesSearchURL(studyInstanceUID, params)
	if err != nil {
		return nil, err
	}
	datasets, _, err := c.searchAt(ctx, u)
	return datasets, err
}

// SearchInstances performs a QIDO-RS instance search for one series and
// returns metadata in the package's Dataset shape.
func (c Client) SearchInstances(ctx context.Context, studyInstanceUID, seriesInstanceUID string, params url.Values) ([]Dataset, error) {
	if err := c.validateMetadataResponseOptions(); err != nil {
		return nil, err
	}
	u, err := c.Endpoint.InstanceSearchURL(studyInstanceUID, seriesInstanceUID, params)
	if err != nil {
		return nil, err
	}
	datasets, _, err := c.searchAt(ctx, u)
	return datasets, err
}

// StudyMetadata retrieves WADO-RS study metadata and returns instance references.
func (c Client) StudyMetadata(ctx context.Context, studyInstanceUID string) ([]InstanceRef, error) {
	result, err := c.studyMetadataDatasets(ctx, studyInstanceUID)
	return result.refs, err
}

// StudyMetadataDatasets retrieves raw WADO-RS study metadata datasets.
func (c Client) StudyMetadataDatasets(ctx context.Context, studyInstanceUID string) ([]Dataset, error) {
	result, err := c.studyMetadataDatasets(ctx, studyInstanceUID)
	return result.datasets, err
}

// StudyMetadataDatasetsWithURL retrieves validated metadata datasets together
// with the final request URL, including after redirects or a cache hit.
// Use the URL with Endpoint.BulkDataURLFromMetadata and keep it out of logs,
// since its path and query can contain sensitive values.
func (c Client) StudyMetadataDatasetsWithURL(ctx context.Context, studyInstanceUID string) ([]Dataset, string, error) {
	result, err := c.studyMetadataDatasets(ctx, studyInstanceUID)
	return result.datasets, result.requestURL, err
}

type metadataResult struct {
	datasets   []Dataset
	refs       []InstanceRef
	requestURL string
}

func (c Client) studyMetadataDatasets(ctx context.Context, studyInstanceUID string) (metadataResult, error) {
	if err := c.validateMetadataResponseOptions(); err != nil {
		return metadataResult{}, err
	}
	u, err := c.Endpoint.StudyMetadataURL(studyInstanceUID)
	if err != nil {
		return metadataResult{}, err
	}
	return c.retrieveMetadata(ctx, u, InstanceRef{StudyInstanceUID: studyInstanceUID})
}

// SeriesMetadata retrieves WADO-RS series metadata and returns instance references.
func (c Client) SeriesMetadata(ctx context.Context, studyInstanceUID, seriesInstanceUID string) ([]InstanceRef, error) {
	result, err := c.seriesMetadataDatasets(ctx, studyInstanceUID, seriesInstanceUID)
	return result.refs, err
}

// SeriesMetadataDatasets retrieves raw WADO-RS series metadata datasets.
func (c Client) SeriesMetadataDatasets(ctx context.Context, studyInstanceUID, seriesInstanceUID string) ([]Dataset, error) {
	result, err := c.seriesMetadataDatasets(ctx, studyInstanceUID, seriesInstanceUID)
	return result.datasets, err
}

func (c Client) seriesMetadataDatasets(ctx context.Context, studyInstanceUID, seriesInstanceUID string) (metadataResult, error) {
	if err := c.validateMetadataResponseOptions(); err != nil {
		return metadataResult{}, err
	}
	u, err := c.Endpoint.SeriesMetadataURL(studyInstanceUID, seriesInstanceUID)
	if err != nil {
		return metadataResult{}, err
	}
	return c.retrieveMetadata(ctx, u, InstanceRef{StudyInstanceUID: studyInstanceUID, SeriesInstanceUID: seriesInstanceUID})
}

// InstanceMetadata retrieves raw WADO-RS metadata for one instance.
func (c Client) InstanceMetadata(ctx context.Context, ref InstanceRef) ([]Dataset, error) {
	if err := c.validateMetadataResponseOptions(); err != nil {
		return nil, err
	}
	u, err := c.Endpoint.InstanceMetadataURL(ref)
	if err != nil {
		return nil, err
	}
	result, err := c.retrieveMetadata(ctx, u, ref)
	return result.datasets, err
}

func (c Client) retrieveMetadata(ctx context.Context, u *url.URL, scope InstanceRef) (metadataResult, error) {
	result, err := c.doGET(ctx, u, c.metadataAcceptHeader())
	if err != nil {
		return metadataResult{}, err
	}
	resp := result.Response
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return metadataResult{}, statusError(resp)
	}
	datasets, err := c.datasetsFromMetadataResponse(ctx, resp, true)
	if err != nil {
		return metadataResult{}, newDICOMwebError(ErrorKindDecodeResponse, resp.URL, resp.StatusCode, err)
	}
	refs, err := instanceRefsFromDatasets(datasets, scope)
	if err != nil {
		return metadataResult{}, newDICOMwebError(ErrorKindDecodeResponse, resp.URL, resp.StatusCode, err)
	}
	requestURL := result.redirectedURL
	if requestURL == "" {
		requestURL = u.String()
	}
	return metadataResult{datasets: datasets, refs: refs, requestURL: requestURL}, nil
}

// RetrieveInstance retrieves a WADO-RS DICOM object as one or more object parts.
func (c Client) RetrieveInstance(ctx context.Context, ref InstanceRef) ([]ObjectPart, error) {
	return c.RetrieveInstanceWithOptions(ctx, ref, RetrieveOptions{})
}

// RetrieveInstanceWithOptions retrieves a WADO-RS DICOM object with explicit
// media/transfer-syntax preferences.
func (c Client) RetrieveInstanceWithOptions(ctx context.Context, ref InstanceRef, opts RetrieveOptions) ([]ObjectPart, error) {
	if _, err := responseLimits(c.Options); err != nil {
		return nil, newDICOMwebError(ErrorKindRequestFailure, "", 0, err)
	}
	u, err := c.Endpoint.InstanceURL(ref)
	if err != nil {
		return nil, err
	}
	result, err := c.doGETAccepts(ctx, u, retrieveObjectAccepts(opts))
	if err != nil {
		return nil, err
	}
	resp := result.Response
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, statusError(resp)
	}
	parts, err := objectPartsFromResponseWithPolicy(resp, c.Options, dicomObjectResponsePolicy)
	if err != nil {
		return nil, newDICOMwebError(ErrorKindDecodeResponse, resp.URL, resp.StatusCode, err)
	}
	return parts, nil
}

// RetrieveInstanceStreamWithOptions retrieves a WADO-RS DICOM object and streams each object part to handle.
func (c Client) RetrieveInstanceStreamWithOptions(ctx context.Context, ref InstanceRef, opts RetrieveOptions, handle func(ObjectPartStream) error) error {
	if handle == nil {
		return fmt.Errorf("WADO-RS stream handler is required")
	}
	u, err := c.Endpoint.InstanceURL(ref)
	if err != nil {
		return err
	}
	return c.retrieveStreamAt(ctx, u, opts, false, func(part LocatedObjectPartStream) error { return handle(part.Part) })
}

// RetrieveStudyStreamWithOptions streams every Part 10 object in a WADO-RS study response.
func (c Client) RetrieveStudyStreamWithOptions(ctx context.Context, studyInstanceUID string, opts RetrieveOptions, handle func(LocatedObjectPartStream) error) error {
	u, err := c.Endpoint.StudyURL(studyInstanceUID)
	if err != nil {
		return err
	}
	return c.retrieveStreamAt(ctx, u, opts, true, handle)
}

// RetrieveSeriesStreamWithOptions streams every Part 10 object in a WADO-RS series response.
func (c Client) RetrieveSeriesStreamWithOptions(ctx context.Context, studyInstanceUID, seriesInstanceUID string, opts RetrieveOptions, handle func(LocatedObjectPartStream) error) error {
	u, err := c.Endpoint.SeriesURL(studyInstanceUID, seriesInstanceUID)
	if err != nil {
		return err
	}
	return c.retrieveStreamAt(ctx, u, opts, true, handle)
}

func (c Client) retrieveStreamAt(ctx context.Context, u *url.URL, opts RetrieveOptions, requireMultipart bool, handle func(LocatedObjectPartStream) error) error {
	if handle == nil {
		return fmt.Errorf("WADO-RS stream handler is required")
	}
	if _, err := responseLimits(c.Options); err != nil {
		return newDICOMwebError(ErrorKindRequestFailure, "", 0, err)
	}
	httpResp, cancel, err := c.doAcceptGET(ctx, u, retrieveObjectAccepts(opts))
	if err != nil {
		return err
	}
	defer func() {
		if cancel != nil {
			cancel()
		}
		_ = httpResp.Body.Close()
	}()

	resp := responseFromHTTP(u.String(), httpResp)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return statusError(resp)
	}
	policy := dicomObjectResponsePolicy
	policy.requireMultipart = requireMultipart
	return c.streamObjectParts(resp, httpResp.Body, policy, handle)
}

// RetrieveFrames fetches selected one-based frames with WADO-RS. The response
// remains frame-scoped; callers never need to retrieve or decode the complete
// multi-frame instance.
func (c Client) RetrieveFrames(ctx context.Context, ref InstanceRef, frames []int, opts RetrieveOptions) ([]FramePart, error) {
	parts := make([]FramePart, 0, len(frames))
	err := c.RetrieveFramesStreamWithOptions(ctx, ref, frames, opts, func(part FramePartStream) error {
		data, err := io.ReadAll(part.Reader)
		if err != nil {
			return err
		}
		parts = append(parts, FramePart{
			FrameNumber:       part.FrameNumber,
			ContentType:       part.ContentType,
			TransferSyntaxUID: part.TransferSyntaxUID,
			Data:              data,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return parts, nil
}

// RetrieveFramesStreamWithOptions retrieves selected one-based frames and
// invokes handle synchronously for each response part in request order. The
// part Reader is valid only until handle returns. Unread bytes are drained;
// returning an error stops retrieval and closes the HTTP response.
func (c Client) RetrieveFramesStreamWithOptions(ctx context.Context, ref InstanceRef, frames []int, opts RetrieveOptions, handle func(FramePartStream) error) error {
	if handle == nil {
		return fmt.Errorf("WADO-RS frame stream handler is required")
	}
	if _, err := responseLimits(c.Options); err != nil {
		return newDICOMwebError(ErrorKindRequestFailure, "", 0, err)
	}
	u, err := c.Endpoint.FramesURL(ref, frames)
	if err != nil {
		return err
	}
	httpResp, cancel, err := c.doAcceptGET(ctx, u, retrieveFramesAccepts(opts))
	if err != nil {
		return err
	}
	defer func() {
		if cancel != nil {
			cancel()
		}
		_ = httpResp.Body.Close()
	}()

	resp := responseFromHTTP(u.String(), httpResp)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return statusError(resp)
	}
	partCount := 0
	err = c.streamObjectParts(resp, httpResp.Body, frameResponsePolicy, func(part LocatedObjectPartStream) error {
		if partCount >= len(frames) {
			return &ResponseDecodeError{
				Kind: ResponseDecodePartLimit, Limit: int64(len(frames)), Value: int64(partCount + 1),
			}
		}
		frame := FramePartStream{
			FrameNumber:       frames[partCount],
			ContentType:       part.Part.ContentType,
			TransferSyntaxUID: part.Part.TransferSyntaxUID,
			Reader:            part.Part.Reader,
			Size:              -1,
		}
		partCount++
		return handle(frame)
	})
	if err != nil {
		return err
	}
	if partCount != len(frames) {
		return newDICOMwebError(ErrorKindDecodeResponse, resp.URL, resp.StatusCode,
			fmt.Errorf("RetrieveFrames returned %d parts for %d frames", partCount, len(frames)))
	}
	return nil
}

func (c Client) streamObjectParts(resp Response, body io.Reader, policy responseMediaPolicy, handle func(LocatedObjectPartStream) error) error {
	limits, err := responseLimits(c.Options)
	if err != nil {
		return newDICOMwebError(ErrorKindRequestFailure, "", 0, err)
	}
	contentType, err := responseContentType(resp.Header.Values("Content-Type"))
	if err != nil {
		return newDICOMwebError(ErrorKindDecodeResponse, resp.URL, resp.StatusCode, err)
	}
	mediaType, params, err := validateOuterResponseMediaType(contentType, policy, limits)
	if err != nil {
		return newDICOMwebError(ErrorKindDecodeResponse, resp.URL, resp.StatusCode, err)
	}
	maxBodyBytes := c.Options.MaxBodyBytes
	if maxBodyBytes <= 0 {
		maxBodyBytes = DefaultMaxBodyBytes
	}
	body = &responseBodyLimitReader{reader: truncatedBodyReader{body}, remaining: maxBodyBytes, limit: maxBodyBytes, resp: resp}
	if mediaType == "multipart/related" {
		reader := newMultipartResponseReader(body, params["boundary"], limits.maxHeaderBytes)
		partCount := 0
		for {
			part, err := reader.nextPart()
			if errors.Is(err, io.EOF) {
				if partCount == 0 {
					return newDICOMwebError(ErrorKindDecodeResponse, resp.URL, resp.StatusCode,
						&ResponseDecodeError{Kind: ResponseDecodeMalformedMultipart})
				}
				return nil
			}
			if err != nil {
				var clientErr *Error
				if errors.As(err, &clientErr) {
					return err
				}
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return err
				}
				if transportReadFailure(err) {
					return requestError(resp.URL, err)
				}
				return newDICOMwebError(ErrorKindDecodeResponse, resp.URL, resp.StatusCode,
					multipartDecodeError(err))
			}
			partCount++
			if partCount > limits.maxParts {
				return newDICOMwebError(ErrorKindDecodeResponse, resp.URL, resp.StatusCode,
					&ResponseDecodeError{Kind: ResponseDecodePartLimit, Limit: int64(limits.maxParts), Value: int64(partCount)})
			}
			if err := validatePartHeader(part.Header, limits); err != nil {
				return newDICOMwebError(ErrorKindDecodeResponse, resp.URL, resp.StatusCode, err)
			}
			partContentType := part.Header.Get("Content-Type")
			if _, _, err := validatePartMediaType(partContentType, policy, limits); err != nil {
				return newDICOMwebError(ErrorKindDecodeResponse, resp.URL, resp.StatusCode, err)
			}
			limited := newResponsePartLimitReader(part, limits.maxPartBytes)
			if err := handle(LocatedObjectPartStream{
				Part: ObjectPartStream{
					ContentType:       partContentType,
					TransferSyntaxUID: transferSyntaxFromContentType(partContentType),
					Reader:            limited,
				},
				ContentLocation: part.Header.Get("Content-Location"),
			}); err != nil {
				if responseDecodeFailure(err) {
					return newDICOMwebError(ErrorKindDecodeResponse, resp.URL, resp.StatusCode, err)
				}
				return err
			}
			if _, err := io.Copy(io.Discard, limited); err != nil {
				return objectPartDrainError(resp, err)
			}
		}
	}
	limited := newResponsePartLimitReader(body, limits.maxPartBytes)
	err = handle(LocatedObjectPartStream{
		Part: ObjectPartStream{
			ContentType:       contentType,
			TransferSyntaxUID: transferSyntaxFromContentType(contentType),
			Reader:            limited,
		},
		ContentLocation: resp.Header.Get("Content-Location"),
	})
	if err != nil {
		if responseDecodeFailure(err) {
			return newDICOMwebError(ErrorKindDecodeResponse, resp.URL, resp.StatusCode, err)
		}
		return err
	}
	if _, err := io.Copy(io.Discard, limited); err != nil {
		return objectPartDrainError(resp, err)
	}
	return nil
}

// truncatedBodyReader reports an HTTP body that ended before its declared
// Content-Length or final chunk, as net/http does when the connection closes
// early, as ResponseDecodeTruncatedBody. A multipart body that is complete
// but lacks its closing delimiter stays ResponseDecodeMalformedMultipart.
type truncatedBodyReader struct{ io.Reader }

func (body truncatedBodyReader) Read(buffer []byte) (int, error) {
	n, err := body.Reader.Read(buffer)
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return n, &ResponseDecodeError{Kind: ResponseDecodeTruncatedBody}
	}
	return n, err
}

// objectPartDrainError classifies a failure to read the rest of a part after
// its handler returned.
func objectPartDrainError(resp Response, err error) error {
	err = responsePartReadError(err)
	if responseDecodeFailure(err) {
		return newDICOMwebError(ErrorKindDecodeResponse, resp.URL, resp.StatusCode, err)
	}
	if transportReadFailure(err) {
		return requestError(resp.URL, err)
	}
	return err
}

// transportReadFailure reports a response body read that failed below HTTP,
// such as a connection reset by the peer, as opposed to a body that arrived
// but is malformed. The caller reports it as a request failure.
func transportReadFailure(err error) bool {
	var clientErr *Error
	if errors.As(err, &clientErr) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var netErr net.Error
	return errors.As(err, &netErr) || errors.Is(err, net.ErrClosed)
}

func retrieveObjectAccepts(opts RetrieveOptions) []string {
	uids := normalizedTransferSyntaxUIDs(opts.TransferSyntaxUIDs)
	if len(uids) == 0 {
		return []string{acceptDICOM}
	}
	groups := make([]string, 0, len(uids))
	for _, uid := range uids {
		groups = append(groups, fmt.Sprintf(
			`multipart/related; type="application/dicom"; transfer-syntax=%s, application/dicom; transfer-syntax=%s`,
			uid, uid))
	}
	return acceptSuffixes(groups)
}

func retrieveFramesAccepts(opts RetrieveOptions) []string {
	uids := normalizedTransferSyntaxUIDs(opts.TransferSyntaxUIDs)
	if len(uids) == 0 {
		return []string{`multipart/related; type="application/octet-stream"`}
	}
	groups := make([]string, 0, len(uids))
	for _, uid := range uids {
		mediaType := retrieveFrameMediaType(uid)
		groups = append(groups, fmt.Sprintf(
			`multipart/related; type="%s"; transfer-syntax=%s`,
			mediaType,
			uid,
		))
	}
	return acceptSuffixes(groups)
}

// acceptSuffixes keeps the multipart and single-part ranges for one syntax
// together. All suffixes share the allocation for the complete field value.
func acceptSuffixes(groups []string) []string {
	const separator = ", "
	accept := strings.Join(groups, separator)
	suffixes := make([]string, len(groups))
	for i, group := range groups {
		suffixes[i] = accept
		if i+1 < len(groups) {
			accept = accept[len(group)+len(separator):]
		}
	}
	return suffixes
}

func retrieveFrameMediaType(uid string) string {
	switch uid {
	case transfer.JPEGBaseline.UID,
		transfer.JPEGExtended.UID,
		transfer.JPEGLosslessNonHierarchical.UID,
		transfer.JPEGLosslessSV1.UID:
		return "image/jpeg"
	case transfer.JPEGLSLossless.UID, transfer.JPEGLSNearLossless.UID:
		return "image/jls"
	case transfer.JPEG2000LosslessOnly.UID, transfer.JPEG2000.UID:
		return "image/jp2"
	case transfer.JPEG2000Part2Lossless.UID, transfer.JPEG2000Part2.UID:
		return "image/jpx"
	case transfer.RLELossless.UID:
		return "image/dicom-rle"
	default:
		return "application/octet-stream"
	}
}

func normalizedTransferSyntaxUIDs(uids []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(uids))
	for _, uid := range uids {
		uid = strings.TrimSpace(uid)
		if uid == "" || seen[uid] {
			continue
		}
		seen[uid] = true
		out = append(out, uid)
	}
	return out
}

// StoreInstances stores DICOM objects with STOW-RS multipart upload.
func (c Client) StoreInstances(ctx context.Context, instances []StoreInstance) (StoreResult, error) {
	if len(instances) == 0 {
		return StoreResult{}, nil
	}
	u, err := c.Endpoint.StoreStudiesURL()
	if err != nil {
		return StoreResult{}, err
	}
	return c.storeInstancesAt(ctx, u, instances)
}

// StoreInstancesToStudy stores DICOM objects through the study-scoped STOW-RS route.
func (c Client) StoreInstancesToStudy(ctx context.Context, studyInstanceUID string, instances []StoreInstance) (StoreResult, error) {
	if len(instances) == 0 {
		return StoreResult{}, nil
	}
	u, err := c.Endpoint.StoreStudyURL(studyInstanceUID)
	if err != nil {
		return StoreResult{}, err
	}
	return c.storeInstancesAt(ctx, u, instances)
}

func (c Client) storeInstancesAt(ctx context.Context, u *url.URL, instances []StoreInstance) (StoreResult, error) {
	body, contentType, err := stowMultipartReader(instances)
	if err != nil {
		return StoreResult{Response: Response{URL: SafeURL(u.String())}}, err
	}
	defer body.Close()
	return c.storeMultipartAt(ctx, u, body, contentType)
}

// storeMultipartAt posts a STOW-RS multipart body and decodes the DICOM JSON
// store response.
func (c Client) storeMultipartAt(ctx context.Context, u *url.URL, body io.Reader, contentType string) (StoreResult, error) {
	resp, err := c.doPOST(ctx, u, body, contentType)
	result := StoreResult{Response: resp}
	if err != nil {
		return result, err
	}
	parsed, parseErr := StoreResultFromDICOMJSON(resp.Body)
	if parseErr != nil {
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return result, statusError(resp)
		}
		return result, newDICOMwebError(ErrorKindDecodeResponse, resp.URL, resp.StatusCode, parseErr)
	}
	parsed.Response = resp
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return parsed, statusError(resp)
	}
	return parsed, nil
}

// doGET performs a buffered GET, served from and stored in Options.Cache when
// one is configured.
func (c Client) doGET(ctx context.Context, u *url.URL, accept string) (bufferedGET, error) {
	return c.doGETAccepts(ctx, u, []string{accept})
}

func (c Client) doGETAccepts(ctx context.Context, u *url.URL, accepts []string) (bufferedGET, error) {
	if _, err := responseLimits(c.Options); err != nil {
		return bufferedGET{}, newDICOMwebError(ErrorKindRequestFailure, "", 0, err)
	}
	cache := c.Options.Cache
	var key responseCacheKey
	if cache != nil {
		originBound, err := c.redirectsRequireSameOrigin(c.usesBasicAuth())
		if err != nil {
			return bufferedGET{}, &Error{Kind: ErrorKindInvalidEndpoint, Err: err}
		}
		key = responseCacheKey{url: u.String(), accept: accepts[0], originBound: originBound}
		if cached, ok := cache.get(key); ok {
			return cached, nil
		}
	}
	raw, cancel, err := c.doAcceptGET(ctx, u, accepts)
	if err != nil {
		return bufferedGET{}, err
	}
	resp, err := c.bufferResponse(u, raw, cancel)
	result := bufferedGET{Response: resp}
	if err != nil {
		return result, err
	}
	if raw.Request != nil && raw.Request.URL != nil && *raw.Request.URL != *u {
		received := *raw.Request.URL
		received.Fragment, received.RawFragment = "", ""
		result.redirectedURL = received.String()
	}
	cache.put(key, result)
	return result, nil
}

// doGETUncached performs a buffered GET that always reaches the server.
func (c Client) doGETUncached(ctx context.Context, u *url.URL, accept string) (Response, error) {
	if _, err := responseLimits(c.Options); err != nil {
		return Response{}, newDICOMwebError(ErrorKindRequestFailure, "", 0, err)
	}
	return c.doRequest(ctx, http.MethodGet, u, nil, "", accept)
}

func (c Client) doPOST(ctx context.Context, u *url.URL, body io.Reader, contentType string) (Response, error) {
	return c.doRequest(ctx, http.MethodPost, u, body, contentType, acceptDICOMJSON)
}

func (c Client) doRequest(ctx context.Context, method string, u *url.URL, body io.Reader, contentType, accept string) (Response, error) {
	httpResp, cancel, err := c.doHTTP(ctx, method, u, body, contentType, accept)
	if err != nil {
		return Response{}, err
	}
	return c.bufferResponse(u, httpResp, cancel)
}

func (c Client) bufferResponse(u *url.URL, httpResp *http.Response, cancel context.CancelFunc) (Response, error) {
	defer func() {
		_ = httpResp.Body.Close()
		if cancel != nil {
			cancel()
		}
	}()

	maxBodyBytes := c.Options.MaxBodyBytes
	if maxBodyBytes <= 0 {
		maxBodyBytes = DefaultMaxBodyBytes
	}
	response := responseFromHTTP(u.String(), httpResp)
	data, err := io.ReadAll(io.LimitReader(httpResp.Body, maxBodyBytes+1))
	if err != nil {
		return response, newDICOMwebError(ErrorKindRequestFailure, response.URL, response.StatusCode, err)
	}
	if int64(len(data)) > maxBodyBytes {
		return response, newDICOMwebError(ErrorKindRequestFailure, response.URL, response.StatusCode,
			fmt.Errorf("%w of %d bytes", ErrResponseBodyLimit, maxBodyBytes))
	}
	response.Body = data
	return response, nil
}

// doHTTPAttempt performs one HTTP attempt (plus one bearer refresh after a
// 401). The caller closes the response body and then calls the returned
// cancel function.
func (c Client) doHTTPAttempt(ctx context.Context, method string, u *url.URL, body io.Reader, contentType, accept string, options requestOptions) (*http.Response, context.CancelFunc, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	reqCtx := ctx
	var cancel context.CancelFunc
	if timeout := c.Options.Timeout; timeout > 0 {
		reqCtx, cancel = context.WithTimeout(ctx, timeout)
	} else if _, ok := ctx.Deadline(); !ok {
		reqCtx, cancel = context.WithTimeout(ctx, DefaultTimeout)
	}

	httpClient, err := c.httpClientForRequest(u, options.sameOriginRedirect)
	if err != nil {
		if cancel != nil {
			cancel()
		}
		return nil, nil, &Error{Kind: ErrorKindInvalidEndpoint, Err: err}
	}

	req, token, sourceAuth, err := c.newHTTPRequest(reqCtx, method, u, body, contentType, accept)
	if err != nil {
		if cancel != nil {
			cancel()
		}
		return nil, nil, err
	}

	maps.Copy(req.Header, options.headers)
	resp, err := httpClient.Do(req)
	if err != nil {
		if cancel != nil {
			cancel()
		}
		return nil, nil, requestError(u.String(), err)
	}
	if resp.StatusCode == http.StatusUnauthorized && sourceAuth {
		invalidator, ok := c.Options.BearerTokenSource.(bearerTokenInvalidator)
		if ok {
			invalidator.Invalidate(token)
		}
		// Retry only requests whose body can be reproduced. A streaming STOW-RS
		// body is intentionally not replayed after partial transmission.
		if ok && (body == nil || req.GetBody != nil) {
			discardAndCloseResponse(resp)
			var retryBody io.ReadCloser
			if req.GetBody != nil {
				var bodyErr error
				retryBody, bodyErr = req.GetBody()
				if bodyErr != nil {
					if cancel != nil {
						cancel()
					}
					return nil, nil, newDICOMwebError(ErrorKindRequestFailure, u.String(), 0, bodyErr)
				}
			}
			retry, _, _, requestErr := c.newHTTPRequest(reqCtx, method, u, retryBody, contentType, accept)
			if requestErr != nil {
				if retryBody != nil {
					_ = retryBody.Close()
				}
				if cancel != nil {
					cancel()
				}
				return nil, nil, requestErr
			}
			retry.ContentLength = req.ContentLength
			retry.GetBody = req.GetBody
			maps.Copy(retry.Header, options.headers)
			resp, err = httpClient.Do(retry)
			if err != nil {
				if cancel != nil {
					cancel()
				}
				return nil, nil, requestError(u.String(), err)
			}
		}
	}
	return resp, cancel, nil
}

func (c Client) newHTTPRequest(
	ctx context.Context,
	method string,
	u *url.URL,
	body io.Reader,
	contentType string,
	accept string,
) (*http.Request, AccessToken, bool, error) {
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, AccessToken{}, false,
			newDICOMwebError(ErrorKindRequestFailure, u.String(), 0, err)
	}
	req.Header.Set("Accept", accept)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if source := c.Options.BearerTokenSource; source != nil {
		token, err := accessTokenFromSource(ctx, u.String(), source)
		if err != nil {
			return nil, AccessToken{}, false, err
		}
		req.Header.Set("Authorization", "Bearer "+token.Value())
		return req, token, true, nil
	}
	if token := strings.TrimSpace(c.Options.BearerToken); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	} else if user := strings.TrimSpace(c.Options.BasicUsername); user != "" {
		req.SetBasicAuth(user, c.Options.BasicPassword)
	}
	return req, AccessToken{}, false, nil
}

func discardAndCloseResponse(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	_ = resp.Body.Close()
}

func responseFromHTTP(rawURL string, httpResp *http.Response) Response {
	return Response{
		URL:        SafeURL(rawURL),
		StatusCode: httpResp.StatusCode,
		Status:     safeHTTPStatus(httpResp.StatusCode),
		Header:     httpResp.Header.Clone(),
	}
}

// ErrResponseBodyLimit reports a response body longer than Options.MaxBodyBytes
// or than the limit of the request that read it.
var ErrResponseBodyLimit = errors.New("dicom dicomweb: response body exceeds the limit")

type responseBodyLimitReader struct {
	reader    io.Reader
	remaining int64
	limit     int64
	resp      Response
}

func (r *responseBodyLimitReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.remaining == 0 {
		var extra [1]byte
		n, err := r.reader.Read(extra[:])
		if n > 0 {
			return 0, newDICOMwebError(ErrorKindRequestFailure, r.resp.URL, r.resp.StatusCode,
				fmt.Errorf("%w of %d bytes", ErrResponseBodyLimit, r.limit))
		}
		return 0, err
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.reader.Read(p)
	r.remaining -= int64(n)
	return n, err
}

func requestError(rawURL string, err error) error {
	if isTimeout(err) {
		return newDICOMwebError(ErrorKindTimeout, rawURL, 0, err)
	}
	return newDICOMwebError(ErrorKindRequestFailure, rawURL, 0, err)
}

func statusError(resp Response) error {
	kind := ErrorKindHTTPStatus
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		kind = ErrorKindAuthStatus
	}
	return newDICOMwebError(kind, resp.URL, resp.StatusCode, nil)
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func stowMultipartReader(instances []StoreInstance) (io.ReadCloser, string, error) {
	for i, instance := range instances {
		if !storeInstanceHasPayload(instance) {
			return nil, "", newStorePayloadError(i, instance, errStorePayloadEmpty)
		}
	}

	reader, pipeWriter := io.Pipe()
	writer := multipart.NewWriter(pipeWriter)
	contentType := `multipart/related; type="application/dicom"; boundary=` + writer.Boundary()
	go func() {
		err := writeSTOWMultipart(writer, instances)
		if err == nil {
			// The closing boundary completes the request. A failed part
			// leaves the body without it, so a server never takes a cut
			// part for a complete one.
			err = writer.Close()
		}
		if err != nil {
			_ = pipeWriter.CloseWithError(err)
			return
		}
		_ = pipeWriter.Close()
	}()
	return reader, contentType, nil
}

func writeSTOWMultipart(writer *multipart.Writer, instances []StoreInstance) error {
	for i, instance := range instances {
		header := textproto.MIMEHeader{}
		header.Set("Content-Type", "application/dicom")
		part, err := writer.CreatePart(header)
		if err != nil {
			return err
		}
		payload, closePayload, err := storeInstancePayload(instance)
		if err != nil {
			return newStorePayloadError(i, instance, err)
		}
		// A read error is the payload's; a write error is the request's.
		source := &storePayloadReader{reader: payload}
		written, copyErr := io.Copy(part, source)
		var closeErr error
		if closePayload != nil {
			closeErr = closePayload.Close()
		}
		switch {
		case source.err != nil:
			return newStorePayloadError(i, instance, source.err)
		case copyErr != nil:
			return copyErr
		case closeErr != nil:
			return newStorePayloadError(i, instance, closeErr)
		case written == 0:
			return newStorePayloadError(i, instance, errStorePayloadEmpty)
		}
	}
	return nil
}

var errStorePayloadEmpty = errors.New("empty payload")

// StorePayloadError reports a STOW-RS instance whose Part 10 payload could
// not be produced: StoreInstance.Open failed, its reader failed or closed
// with an error, or it was empty. The request carrying it was cut before it
// was complete. Index is the instance's position in the request.
type StorePayloadError struct {
	Index          int
	Path           string
	SOPInstanceUID string
	Err            error
}

func newStorePayloadError(index int, instance StoreInstance, err error) *StorePayloadError {
	return &StorePayloadError{Index: index, Path: instance.Path, SOPInstanceUID: instance.SOPInstanceUID, Err: err}
}

func (e *StorePayloadError) Error() string {
	return fmt.Sprintf("STOW-RS object %s: %v", firstNonEmpty(e.Path, e.SOPInstanceUID), e.Err)
}

func (e *StorePayloadError) Unwrap() error { return e.Err }

// storePayloadReader keeps the read error of a payload apart from the write
// errors of the request it is copied into.
type storePayloadReader struct {
	reader io.Reader
	err    error
}

func (r *storePayloadReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if err != nil && err != io.EOF {
		r.err = err
	}
	return n, err
}

func storeInstanceHasPayload(instance StoreInstance) bool {
	return instance.Open != nil || instance.Reader != nil || len(instance.Data) > 0
}

func storeInstancePayload(instance StoreInstance) (io.Reader, io.Closer, error) {
	if instance.Open != nil {
		reader, err := instance.Open()
		if err != nil {
			return nil, nil, fmt.Errorf("open: %w", err)
		}
		if reader == nil {
			return nil, nil, errors.New("open returned a nil reader")
		}
		return reader, reader, nil
	}
	if instance.Reader != nil {
		return instance.Reader, nil, nil
	}
	return bytes.NewReader(instance.Data), nil, nil
}
