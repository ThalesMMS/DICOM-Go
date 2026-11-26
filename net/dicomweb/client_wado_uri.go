package dicomweb

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"unicode"

	"github.com/ThalesMMS/dicom-go/core"
)

var wadoURIResponsePolicy = responseMediaPolicy{
	plainSingleMedia: true,
	single:           mediaTypeSet("application/dicom"),
}

// NormalizeWADOURIPath accepts an optional relative path below Endpoint.BaseURL.
// An empty value disables WADO-URI. A single leading or trailing slash is optional.
func NormalizeWADOURIPath(value string) (string, error) {
	if strings.ContainsAny(value, `\?#:%`) || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return "", errors.New("WADO-URI path must be a relative HTTP service path")
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	path := strings.TrimSuffix(strings.TrimPrefix(value, "/"), "/")
	for _, segment := range strings.Split(path, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", errors.New("WADO-URI path must contain only nonempty service segments")
		}
	}
	return path, nil
}

// WADOURIInstanceURL builds the URI Service request for one DICOM instance.
func (e Endpoint) WADOURIInstanceURL(ref InstanceRef) (*url.URL, error) {
	path, err := NormalizeWADOURIPath(e.WADOURIPath)
	if err != nil {
		return nil, &Error{Kind: ErrorKindInvalidEndpoint, Err: err}
	}
	if path == "" {
		return nil, &Error{Kind: ErrorKindInvalidEndpoint, Err: errors.New("WADO-URI path is required")}
	}
	if !core.IsValidUID(ref.StudyInstanceUID) || !core.IsValidUID(ref.SeriesInstanceUID) ||
		!core.IsValidUID(ref.SOPInstanceUID) {
		return nil, &Error{Kind: ErrorKindInvalidEndpoint, Err: errors.New("WADO-URI requires valid study, series and SOP instance UIDs")}
	}
	return e.serviceURL(path, nil, url.Values{
		"requestType": {"WADO"},
		"studyUID":    {ref.StudyInstanceUID},
		"seriesUID":   {ref.SeriesInstanceUID},
		"objectUID":   {ref.SOPInstanceUID},
		"contentType": {"application/dicom"},
	})
}

// RetrieveWADOURIInstanceStream streams one application/dicom Part 10 payload.
// The request uses the server's default transfer syntax. The response supplies
// the actual Transfer Syntax UID in File Meta Information.
//
// The handler is called synchronously with a bounded reader. Consume or stage
// the payload inside the handler, then validate the file and its identity before
// publishing it after this method succeeds.
func (c Client) RetrieveWADOURIInstanceStream(ctx context.Context, ref InstanceRef, handle func(ObjectPartStream) error) error {
	if handle == nil {
		return errors.New("WADO-URI stream handler is required")
	}
	u, err := c.Endpoint.WADOURIInstanceURL(ref)
	if err != nil {
		return err
	}
	if _, err := responseLimits(c.Options); err != nil {
		return newDICOMwebError(ErrorKindRequestFailure, "", 0, err)
	}
	httpResp, cancel, err := c.doHTTPWithOptions(ctx, http.MethodGet, u, nil, "", "application/dicom", requestOptions{sameOriginRedirect: true})
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
	if resp.StatusCode != http.StatusOK {
		return statusError(resp)
	}
	if len(resp.Header.Values("Content-Range")) != 0 {
		return newDICOMwebError(ErrorKindDecodeResponse, resp.URL, resp.StatusCode, errors.New("WADO-URI response must contain the complete instance"))
	}
	return c.streamObjectParts(resp, httpResp.Body, wadoURIResponsePolicy,
		func(part LocatedObjectPartStream) error { return handle(part.Part) })
}
