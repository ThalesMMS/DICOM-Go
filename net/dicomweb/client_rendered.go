package dicomweb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// RenderTarget identifies the resource of a WADO-RS rendered or thumbnail
// request (PS3.18 10.4.1.1.3 and 10.4.1.1.4): a study, a series, an instance,
// or selected one-based frames of an instance. Empty trailing UIDs select the
// parent level; Frames requires all three UIDs.
type RenderTarget struct {
	StudyInstanceUID  string
	SeriesInstanceUID string
	SOPInstanceUID    string
	Frames            []int
}

// RenderedOptions selects rendered media types and the PS3.18 8.3.5.1
// rendering query parameters. Zero values omit a parameter.
type RenderedOptions struct {
	// MediaTypes lists acceptable rendered media types in preference order,
	// for example "image/png". Empty selects "image/jpeg".
	MediaTypes []string
	// Viewport scales, and optionally crops, the rendered image.
	Viewport *RenderViewport
	// Window applies a VOI window; an empty Function means "linear".
	Window *RenderWindow
	// Quality (1-100) applies to lossy media types.
	Quality int
	// Annotations requests burned-in annotations such as "patient".
	Annotations []string
	// ICCProfile is one of "no", "yes", "srgb", "adobergb", "rommrgb" or
	// "displayp3".
	ICCProfile string
}

// ThumbnailOptions selects thumbnail media types and size. Viewport source
// coordinates are not defined for thumbnails and are rejected.
type ThumbnailOptions struct {
	MediaTypes []string
	Viewport   *RenderViewport
}

// RenderedObject is one rendered or thumbnail representation.
type RenderedObject struct {
	ContentType     string
	ContentLocation string
	Data            []byte
}

// RenderedURL builds the WADO-RS rendered resource URL with its query
// parameters.
func (e Endpoint) RenderedURL(target RenderTarget, opts RenderedOptions) (*url.URL, error) {
	u, err := e.renderResourceURL(target, "rendered")
	if err != nil {
		return nil, err
	}
	query, err := renderedQuery(opts)
	if err != nil {
		return nil, err
	}
	u.RawQuery = query.Encode()
	return u, nil
}

// ThumbnailURL builds the WADO-RS thumbnail resource URL.
func (e Endpoint) ThumbnailURL(target RenderTarget, opts ThumbnailOptions) (*url.URL, error) {
	u, err := e.renderResourceURL(target, "thumbnail")
	if err != nil {
		return nil, err
	}
	query := url.Values{}
	if opts.Viewport != nil {
		if opts.Viewport.SourceX != nil || opts.Viewport.SourceY != nil || opts.Viewport.SourceWidth != nil || opts.Viewport.SourceHeight != nil {
			return nil, invalidRenderOption("thumbnail viewport accepts only width and height")
		}
		value, err := renderViewportValue(opts.Viewport)
		if err != nil {
			return nil, err
		}
		query.Set("viewport", value)
	}
	u.RawQuery = query.Encode()
	return u, nil
}

func (e Endpoint) renderResourceURL(target RenderTarget, resource string) (*url.URL, error) {
	study := strings.TrimSpace(target.StudyInstanceUID)
	series := strings.TrimSpace(target.SeriesInstanceUID)
	instance := strings.TrimSpace(target.SOPInstanceUID)
	var (
		u   *url.URL
		err error
	)
	switch {
	case len(target.Frames) > 0:
		u, err = e.FramesURL(InstanceRef{StudyInstanceUID: study, SeriesInstanceUID: series, SOPInstanceUID: instance}, target.Frames)
	case instance != "":
		u, err = e.InstanceURL(InstanceRef{StudyInstanceUID: study, SeriesInstanceUID: series, SOPInstanceUID: instance})
	case series != "":
		u, err = e.SeriesURL(study, series)
	default:
		u, err = e.StudyURL(study)
	}
	if err != nil {
		return nil, err
	}
	u.Path = joinURLPath(u.Path, "", resource)
	return u, nil
}

func (t RenderTarget) multipleRepresentations() bool {
	switch {
	case len(t.Frames) > 0:
		return len(t.Frames) > 1
	case strings.TrimSpace(t.SOPInstanceUID) != "":
		return false
	default:
		return true
	}
}

func renderedQuery(opts RenderedOptions) (url.Values, error) {
	query := url.Values{}
	if opts.Viewport != nil {
		value, err := renderViewportValue(opts.Viewport)
		if err != nil {
			return nil, err
		}
		query.Set("viewport", value)
	}
	if window := opts.Window; window != nil {
		if math.IsNaN(window.Center) || math.IsInf(window.Center, 0) || math.IsNaN(window.Width) || math.IsInf(window.Width, 0) || window.Width <= 0 {
			return nil, invalidRenderOption("window center must be finite and width positive")
		}
		function := strings.TrimSpace(window.Function)
		if function == "" {
			function = "linear"
		}
		if function != "linear" && function != "linear-exact" && function != "sigmoid" {
			return nil, invalidRenderOption("window function must be linear, linear-exact or sigmoid")
		}
		query.Set("window", formatRenderFloat(window.Center)+","+formatRenderFloat(window.Width)+","+function)
	}
	if opts.Quality != 0 {
		if opts.Quality < 1 || opts.Quality > 100 {
			return nil, invalidRenderOption("quality must be between 1 and 100")
		}
		query.Set("quality", strconv.Itoa(opts.Quality))
	}
	if len(opts.Annotations) > 0 {
		values := make([]string, 0, len(opts.Annotations))
		for _, annotation := range opts.Annotations {
			annotation = strings.TrimSpace(annotation)
			if annotation == "" || strings.ContainsAny(annotation, ", ") {
				return nil, invalidRenderOption("annotation values must be single keywords")
			}
			values = append(values, annotation)
		}
		query.Set("annotation", strings.Join(values, ","))
	}
	if profile := strings.TrimSpace(opts.ICCProfile); profile != "" {
		switch profile {
		case "no", "yes", "srgb", "adobergb", "rommrgb", "displayp3":
			query.Set("iccprofile", profile)
		default:
			return nil, invalidRenderOption("unsupported iccprofile value")
		}
	}
	return query, nil
}

func renderViewportValue(viewport *RenderViewport) (string, error) {
	if viewport.Width <= 0 || viewport.Height <= 0 {
		return "", invalidRenderOption("viewport width and height must be positive")
	}
	values := []string{strconv.Itoa(viewport.Width), strconv.Itoa(viewport.Height)}
	sources := []*float64{viewport.SourceX, viewport.SourceY, viewport.SourceWidth, viewport.SourceHeight}
	last := -1
	for index, source := range sources {
		if source != nil {
			if math.IsNaN(*source) || math.IsInf(*source, 0) || (index >= 2 && *source == 0) {
				return "", invalidRenderOption("viewport source values must be finite and source sizes non-zero")
			}
			last = index
		}
	}
	for index := 0; index <= last; index++ {
		if sources[index] == nil {
			values = append(values, "")
			continue
		}
		values = append(values, formatRenderFloat(*sources[index]))
	}
	return strings.Join(values, ","), nil
}

func formatRenderFloat(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

func invalidRenderOption(message string) error {
	return &Error{Kind: ErrorKindInvalidEndpoint, Err: fmt.Errorf("dicomweb: %s", message)}
}

// renderedAcceptHeader advertises the requested media types with descending
// quality values. Collections and multi-frame selections use
// multipart/related, as PS3.18 8.7.3.3 requires; a single rendered
// representation and every thumbnail use the bare media type.
func renderedAcceptHeader(mediaTypes []string, multipart, thumbnail bool) (string, error) {
	if len(mediaTypes) == 0 {
		mediaTypes = []string{"image/jpeg"}
	}
	values := make([]string, 0, len(mediaTypes))
	seen := map[string]bool{}
	for index, raw := range mediaTypes {
		mediaType, params, err := mime.ParseMediaType(strings.TrimSpace(raw))
		mediaType = strings.ToLower(mediaType)
		if err != nil || len(params) != 0 || !isRenderedMediaType(mediaType, thumbnail) {
			return "", invalidRenderOption("unsupported rendered media type")
		}
		if seen[mediaType] {
			continue
		}
		seen[mediaType] = true
		value := mediaType
		if multipart {
			value = `multipart/related; type="` + mediaType + `"`
		}
		if index > 0 {
			quality := 1 - 0.1*float64(index)
			if quality < 0.1 {
				quality = 0.1
			}
			value += "; q=" + strconv.FormatFloat(quality, 'f', 1, 64)
		}
		values = append(values, value)
	}
	return strings.Join(values, ", "), nil
}

func renderedResponsePolicy(thumbnail bool) responseMediaPolicy {
	types := []string{"image/jpeg", "image/png", "image/gif", "image/jp2", "image/jph", "image/jphc", "image/jxl"}
	if !thumbnail {
		types = append(types, "video/mpeg", "video/mp4", "video/h265", "text/html", "text/plain", "application/pdf")
	}
	set := mediaTypeSet(types...)
	return responseMediaPolicy{single: set, parts: set}
}

// RetrieveRendered retrieves rendered representations of target. A study,
// series or multi-frame selection returns one representation per instance or
// frame; an instance or single frame returns one.
func (c Client) RetrieveRendered(ctx context.Context, target RenderTarget, opts RenderedOptions) ([]RenderedObject, error) {
	var objects []RenderedObject
	err := c.RetrieveRenderedStream(ctx, target, opts, func(part LocatedObjectPartStream) error {
		data, err := io.ReadAll(part.Part.Reader)
		if err != nil {
			return err
		}
		objects = append(objects, RenderedObject{ContentType: part.Part.ContentType, ContentLocation: part.ContentLocation, Data: data})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return objects, nil
}

// RetrieveRenderedStream retrieves rendered representations and invokes
// handle synchronously for each one, under the client's response limits. The
// part Reader is valid only until handle returns.
func (c Client) RetrieveRenderedStream(ctx context.Context, target RenderTarget, opts RenderedOptions, handle func(LocatedObjectPartStream) error) error {
	if handle == nil {
		return fmt.Errorf("WADO-RS rendered stream handler is required")
	}
	u, err := c.Endpoint.RenderedURL(target, opts)
	if err != nil {
		return err
	}
	multipart := target.multipleRepresentations()
	policy := renderedResponsePolicy(false)
	policy.requireMultipart = multipart
	return c.streamRendered(ctx, u, opts.MediaTypes, multipart, false, policy, handle)
}

// streamRendered asks for the media types in one Accept field with quality
// values. A server that refuses that request without answering 406 may not
// read a list: Orthanc's DICOMweb plugin answers 400 to any rendered or
// thumbnail request whose Accept names more than one media type. Each media
// type is then asked for alone, in order of preference. A refused request
// delivered no representation, so handle has not been called for it.
func (c Client) streamRendered(ctx context.Context, u *url.URL, mediaTypes []string, multipart, thumbnail bool, policy responseMediaPolicy, handle func(LocatedObjectPartStream) error) error {
	accept, err := renderedAcceptHeader(mediaTypes, multipart, thumbnail)
	if err != nil {
		return err
	}
	err = c.streamResource(ctx, u, accept, policy, handle)
	if !strings.Contains(accept, ",") || !renderedListRefused(err) {
		return err
	}
	for _, mediaType := range mediaTypes {
		single, singleErr := renderedAcceptHeader([]string{mediaType}, multipart, thumbnail)
		if singleErr != nil {
			return singleErr
		}
		singleErr = c.streamResource(ctx, u, single, policy, handle)
		if singleErr == nil || !renderedListRefused(singleErr) {
			return singleErr
		}
		err = singleErr
	}
	return err
}

// renderedListRefused reports a status that can mean the server did not
// accept the Accept field as written, as opposed to the resource.
func renderedListRefused(err error) bool {
	var webErr *Error
	if !errors.As(err, &webErr) {
		return false
	}
	switch webErr.StatusCode {
	case http.StatusBadRequest, http.StatusUnsupportedMediaType:
		return true
	}
	return false
}

// RetrieveThumbnail retrieves the single thumbnail representation of target.
func (c Client) RetrieveThumbnail(ctx context.Context, target RenderTarget, opts ThumbnailOptions) (RenderedObject, error) {
	u, err := c.Endpoint.ThumbnailURL(target, opts)
	if err != nil {
		return RenderedObject{}, err
	}
	var thumbnail RenderedObject
	count := 0
	err = c.streamRendered(ctx, u, opts.MediaTypes, false, true, renderedResponsePolicy(true), func(part LocatedObjectPartStream) error {
		count++
		if count > 1 {
			return &ResponseDecodeError{Kind: ResponseDecodePartLimit, Limit: 1, Value: int64(count)}
		}
		data, err := io.ReadAll(part.Part.Reader)
		if err != nil {
			return err
		}
		thumbnail = RenderedObject{ContentType: part.Part.ContentType, ContentLocation: part.ContentLocation, Data: data}
		return nil
	})
	if err != nil {
		return RenderedObject{}, err
	}
	return thumbnail, nil
}

// streamResource performs a bounded GET and streams each representation of a
// single or multipart/related response through policy.
func (c Client) streamResource(ctx context.Context, u *url.URL, accept string, policy responseMediaPolicy, handle func(LocatedObjectPartStream) error) error {
	return c.streamResourceWithOptions(ctx, u, accept, policy, handle, streamResourceOptions{})
}

type streamResourceOptions struct {
	sameOriginRedirect bool
	// Staged bulk resolution requires terminal HTTP validation beyond MIME completion.
	requireEOF bool
}

func (c Client) streamResourceWithOptions(ctx context.Context, u *url.URL, accept string, policy responseMediaPolicy, handle func(LocatedObjectPartStream) error, options streamResourceOptions) error {
	if _, err := responseLimits(c.Options); err != nil {
		return newDICOMwebError(ErrorKindRequestFailure, "", 0, err)
	}
	httpResp, cancel, err := c.doHTTPWithOptions(ctx, http.MethodGet, u, nil, "", accept, requestOptions{sameOriginRedirect: options.sameOriginRedirect})
	if err != nil {
		return err
	}
	bodyClosed := false
	defer func() {
		if cancel != nil {
			cancel()
		}
		if !bodyClosed {
			_ = httpResp.Body.Close()
		}
	}()
	resp := responseFromHTTP(u.String(), httpResp)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return statusError(resp)
	}
	body := io.Reader(httpResp.Body)
	var observed *responseReadErrorObserver
	if options.requireEOF {
		maximum := c.Options.MaxBodyBytes
		if maximum <= 0 {
			maximum = DefaultMaxBodyBytes
		}
		body = &responseBodyLimitReader{reader: body, remaining: maximum, limit: maximum, resp: resp}
		observed = &responseReadErrorObserver{reader: body}
		body = observed
	}
	if err := c.streamObjectParts(resp, body, policy, handle); err != nil {
		if observed != nil {
			return errors.Join(err, observed.err)
		}
		return err
	}
	if !options.requireEOF {
		return nil
	}
	// MIME buffering can consume n>0 together with a terminal transport error
	// yet recognize the closing boundary without surfacing that error.
	if observed.err != nil {
		return observed.err
	}
	if _, err := io.Copy(io.Discard, body); err != nil {
		return errors.Join(err, observed.err)
	}
	if observed.err != nil {
		return observed.err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	bodyClosed = true
	return httpResp.Body.Close()
}

// responseReadErrorObserver is used only for atomic staged publication. It is
// below MIME buffering and its part/body readers, and retains the first non-EOF
// error even when a reader returns useful bytes alongside that error.
type responseReadErrorObserver struct {
	reader io.Reader
	err    error
}

func (r *responseReadErrorObserver) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if err != nil && err != io.EOF && r.err == nil {
		r.err = err
	}
	return n, err
}
