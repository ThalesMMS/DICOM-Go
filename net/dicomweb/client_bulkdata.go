package dicomweb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/ThalesMMS/dicom-go/net/dicomweb/internal/originpolicy"
	"github.com/ThalesMMS/dicom-go/object"
)

const acceptBulkData = `multipart/related; type="application/octet-stream"`

// bulkDataResponsePolicy accepts uncompressed bulk data and the compressed
// pixel media types a server may use for Pixel Data bulk data (PS3.18
// 8.7.3.3.2), as one representation or in multipart/related.
var bulkDataResponsePolicy = func() responseMediaPolicy {
	set := mediaTypeSet(
		"application/octet-stream", "image/jpeg", "image/jls", "image/jp2", "image/jpx", "image/jphc",
		"image/jxl", "image/dicom-rle", "video/mpeg", "video/mp4", "video/h265",
	)
	return responseMediaPolicy{single: set, parts: set}
}()

// BulkDataURL resolves a BulkDataURI from QIDO-RS or WADO-RS metadata to an
// absolute URL. Relative references resolve against the endpoint base URL,
// treated as a directory. The result must share the base URL's scheme, host
// and port so that the client never sends its credentials to another origin;
// user information is rejected and a fragment is dropped.
func (e Endpoint) BulkDataURL(uri string) (*url.URL, error) {
	base, err := e.serviceURL("", nil, nil)
	if err != nil {
		return nil, err
	}
	if !strings.HasSuffix(base.Path, "/") {
		base.Path += "/"
	}
	return resolveBulkDataReference(base, uri)
}

// BulkDataURLFromMetadata resolves a BulkDataURI against the absolute request
// URL that returned it. Both URLs must share the endpoint origin. Use the
// resolved absolute URL with the existing retrieval or resolver methods.
func (e Endpoint) BulkDataURLFromMetadata(metadataURL, uri string) (*url.URL, error) {
	origin, err := e.serviceURL("", nil, nil)
	if err != nil {
		return nil, err
	}
	base, err := url.Parse(strings.TrimSpace(metadataURL))
	if err != nil || !sameOrigin(origin, base) || base.Opaque != "" || base.Fragment != "" || base.RawFragment != "" {
		return nil, &Error{Kind: ErrorKindInvalidEndpoint, Err: fmt.Errorf("BulkData metadata request URL must be absolute and use the endpoint origin")}
	}
	return resolveBulkDataReference(base, uri)
}

func resolveBulkDataReference(base *url.URL, uri string) (*url.URL, error) {
	reference, err := url.Parse(strings.TrimSpace(uri))
	if err != nil || strings.TrimSpace(uri) == "" {
		return nil, &Error{Kind: ErrorKindInvalidEndpoint, Err: fmt.Errorf("BulkDataURI is malformed")}
	}
	if reference.User != nil {
		return nil, &Error{Kind: ErrorKindInvalidEndpoint, Err: fmt.Errorf("BulkDataURI must not include user information")}
	}
	resolved := base.ResolveReference(reference)
	resolved.Fragment, resolved.RawFragment = "", ""
	if !sameOrigin(base, resolved) {
		return nil, &Error{Kind: ErrorKindInvalidEndpoint, Err: fmt.Errorf("BulkDataURI must use the endpoint origin")}
	}
	return resolved, nil
}

func sameOrigin(left, right *url.URL) bool {
	return right != nil && right.User == nil && originpolicy.SameOrigin(left, right)
}

// RetrieveBulkData retrieves a same-origin BulkDataURI and returns its
// representations. A reference to one attribute value yields one part.
func (c Client) RetrieveBulkData(ctx context.Context, uri string) ([]ObjectPart, error) {
	var parts []ObjectPart
	err := c.RetrieveBulkDataStream(ctx, uri, func(part LocatedObjectPartStream) error {
		data, err := io.ReadAll(part.Part.Reader)
		if err != nil {
			return err
		}
		parts = append(parts, ObjectPart{ContentType: part.Part.ContentType, TransferSyntaxUID: part.Part.TransferSyntaxUID, Data: data})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return parts, nil
}

// RetrieveBulkDataStream retrieves a same-origin BulkDataURI and invokes
// handle synchronously for each representation under the client's response
// limits. The part Reader is valid only until handle returns.
func (c Client) RetrieveBulkDataStream(ctx context.Context, uri string, handle func(LocatedObjectPartStream) error) error {
	if handle == nil {
		return fmt.Errorf("WADO-RS bulk data stream handler is required")
	}
	u, err := c.Endpoint.BulkDataURL(uri)
	if err != nil {
		return err
	}
	return c.streamResourceWithOptions(ctx, u, acceptBulkData, bulkDataResponsePolicy, handle, streamResourceOptions{sameOriginRedirect: true})
}

// errBulkDataTooLarge stops a bulk data read that crossed the caller's limit.
var errBulkDataTooLarge = errors.New("bulk data exceeds the caller limit")

// ResolveBulkDataURI implements object.BulkDataURIResolver for http, https and
// relative references: it retrieves the value with RetrieveBulkDataStream and
// buffers it, failing with object.ErrBulkDataTooLarge as soon as more than
// maxBytes (when positive) or the client's part limit arrive. The response
// must hold exactly one representation.
func (c Client) ResolveBulkDataURI(ctx context.Context, uri string, maxBytes int64) (object.BulkDataSource, error) {
	limits, err := responseLimits(c.Options)
	if err != nil {
		return object.BulkDataSource{}, newDICOMwebError(ErrorKindRequestFailure, "", 0, err)
	}
	limit := limits.maxPartBytes
	if maxBytes > 0 && maxBytes < limit {
		limit = maxBytes
	}
	var (
		data  []byte
		count int
	)
	err = c.RetrieveBulkDataStream(ctx, uri, func(part LocatedObjectPartStream) error {
		count++
		if count > 1 {
			return &ResponseDecodeError{Kind: ResponseDecodePartLimit, Limit: 1, Value: int64(count)}
		}
		var buffer bytes.Buffer
		read, err := buffer.ReadFrom(io.LimitReader(part.Part.Reader, limit+1))
		if err != nil {
			return err
		}
		if read > limit {
			return errBulkDataTooLarge
		}
		data = buffer.Bytes()
		return nil
	})
	var decodeErr *ResponseDecodeError
	if errors.Is(err, errBulkDataTooLarge) {
		return object.BulkDataSource{}, object.ErrBulkDataTooLarge
	}
	if errors.As(err, &decodeErr) && decodeErr.Kind == ResponseDecodePartBytes {
		return object.BulkDataSource{}, fmt.Errorf("%w: %w", object.ErrBulkDataTooLarge, err)
	}
	if err != nil {
		return object.BulkDataSource{}, err
	}
	return object.BulkDataSource{Reader: io.NopCloser(bytes.NewReader(data)), Size: int64(len(data))}, nil
}
