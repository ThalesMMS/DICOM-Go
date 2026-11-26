package dicomweb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
)

const bulkDataRangeMediaType = "application/octet-stream"

// ErrBulkDataRangeResponse identifies a successful HTTP response with
// incompatible byte range status or framing. It is wrapped by an Error with Kind
// ErrorKindDecodeResponse.
var ErrBulkDataRangeResponse = errors.New("DICOMweb BulkData response does not match the requested byte range")

// RetrieveBulkDataRange reads exactly length bytes beginning at offset, where
// offset is zero-based within a single BulkData representation. The request
// follows the same URI resolution as RetrieveBulkData. Only a direct,
// identity-encoded application/octet-stream 206 response with the exact
// requested Content-Range and body length succeeds. A server that ignores
// Range and sends 200 may be called again through RetrieveBulkData if the
// caller explicitly wants the entire representation.
func (c Client) RetrieveBulkDataRange(ctx context.Context, uri string, offset, length int64) (ObjectPart, error) {
	limits, err := responseLimits(c.Options)
	if err != nil {
		return ObjectPart{}, newDICOMwebError(ErrorKindRequestFailure, "", 0, err)
	}
	if offset < 0 || length <= 0 || offset > math.MaxInt64-(length-1) {
		return ObjectPart{}, newDICOMwebError(ErrorKindRequestFailure, "", 0, errors.New("invalid BulkData byte range"))
	}
	maximum := min(limits.maxPartBytes, int64(math.MaxInt))
	if length > maximum {
		return ObjectPart{}, newDICOMwebError(ErrorKindRequestFailure, "", 0, &ResponseDecodeError{Kind: ResponseDecodePartBytes, Limit: maximum, Value: length})
	}
	u, err := c.Endpoint.BulkDataURL(uri)
	if err != nil {
		return ObjectPart{}, err
	}
	end := offset + (length - 1)
	httpResp, cancel, err := c.doHTTPWithOptions(ctx, http.MethodGet, u, nil, "", bulkDataRangeMediaType, requestOptions{
		headers: http.Header{
			"Range":           {fmt.Sprintf("bytes=%d-%d", offset, end)},
			"Accept-Encoding": {"identity"},
		},
		sameOriginRedirect: true,
	})
	if err != nil {
		return ObjectPart{}, err
	}
	defer func() {
		_ = httpResp.Body.Close()
		if cancel != nil {
			cancel()
		}
	}()

	resp := responseFromHTTP(u.String(), httpResp)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ObjectPart{}, statusError(resp)
	}
	if resp.StatusCode != http.StatusPartialContent ||
		!matchesBulkDataContentRange(resp.Header.Values("Content-Range"), offset, end) ||
		(httpResp.ContentLength >= 0 && httpResp.ContentLength != length) ||
		httpResp.Uncompressed || !identityContentEncoding(resp.Header) {
		return ObjectPart{}, newDICOMwebError(ErrorKindDecodeResponse, resp.URL, resp.StatusCode, ErrBulkDataRangeResponse)
	}
	contentType, err := responseContentType(resp.Header.Values("Content-Type"))
	if err != nil {
		return ObjectPart{}, newDICOMwebError(ErrorKindDecodeResponse, resp.URL, resp.StatusCode, err)
	}
	mediaType, params, err := parseResponseMediaType(contentType)
	if err != nil {
		return ObjectPart{}, newDICOMwebError(ErrorKindDecodeResponse, resp.URL, resp.StatusCode, err)
	}
	if mediaType != bulkDataRangeMediaType || !mediaAllowed(mediaType, bulkDataResponsePolicy.single, limits.allowedMedia) {
		return ObjectPart{}, newDICOMwebError(ErrorKindDecodeResponse, resp.URL, resp.StatusCode, &ResponseDecodeError{Kind: ResponseDecodeUnexpectedMedia})
	}
	reader := &responseBodyLimitReader{reader: httpResp.Body, remaining: length, limit: length, resp: resp}
	data, err := io.ReadAll(reader)
	if err != nil || int64(len(data)) != length {
		return ObjectPart{}, newDICOMwebError(ErrorKindDecodeResponse, resp.URL, resp.StatusCode, errors.Join(ErrBulkDataRangeResponse, err))
	}
	return ObjectPart{ContentType: contentType, TransferSyntaxUID: strings.TrimSpace(params["transfer-syntax"]), Data: data}, nil
}

func matchesBulkDataContentRange(values []string, offset, end int64) bool {
	if len(values) != 1 {
		return false
	}
	unit, remainder, found := strings.Cut(strings.TrimSpace(values[0]), " ")
	if !found || !strings.EqualFold(unit, "bytes") {
		return false
	}
	interval, totalLength, found := strings.Cut(remainder, "/")
	if !found {
		return false
	}
	first, last, found := strings.Cut(interval, "-")
	if !found {
		return false
	}
	gotOffset, offsetErr := strconv.ParseUint(first, 10, 63)
	gotEnd, endErr := strconv.ParseUint(last, 10, 63)
	if offsetErr != nil || endErr != nil || int64(gotOffset) != offset || int64(gotEnd) != end {
		return false
	}
	if totalLength == "*" {
		return true
	}
	total, err := strconv.ParseUint(totalLength, 10, 63)
	return err == nil && total > gotEnd
}

func identityContentEncoding(header http.Header) bool {
	values := header.Values("Content-Encoding")
	return len(values) == 0 || len(values) == 1 && strings.EqualFold(strings.TrimSpace(values[0]), "identity")
}
