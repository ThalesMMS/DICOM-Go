package dicomweb

import (
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/transfer"
)

func (s *Server) serveWADOURI(w *serverResponseWriter, r *http.Request) (int, error) {
	if !s.options.EnableWADOURI {
		return 0, newServerStatusError(http.StatusNotImplemented, "not_implemented", ErrUnsupported)
	}
	if r.ContentLength > 0 || len(r.TransferEncoding) > 0 {
		return 0, ErrInvalidRequest
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return 0, ErrInvalidRequest
	}
	if len(query) > s.limits.MaxQueryFields {
		return 0, ErrResourceLimit
	}
	allowed := map[string]bool{"requestType": true, "studyUID": true, "seriesUID": true, "objectUID": true, "contentType": true, "transferSyntax": true, "frameNumber": true, "rows": true, "columns": true, "windowCenter": true, "windowWidth": true, "imageQuality": true, "annotation": true, "imageAnnotation": true, "anonymize": true, "charset": true, "region": true, "presentationSeriesUID": true, "presentationUID": true}
	for k, v := range query {
		if !allowed[k] || len(v) != 1 || v[0] == "" {
			return 0, ErrInvalidRequest
		}
		if len(v[0]) > s.limits.MaxQueryValueBytes {
			return 0, ErrResourceLimit
		}
	}
	if query.Get("requestType") != "WADO" {
		return 0, ErrInvalidRequest
	}
	ref := InstanceRef{StudyInstanceUID: query.Get("studyUID"), SeriesInstanceUID: query.Get("seriesUID"), SOPInstanceUID: query.Get("objectUID")}
	for _, uid := range []string{ref.StudyInstanceUID, ref.SeriesInstanceUID, ref.SOPInstanceUID} {
		if err := s.validateUID(uid); err != nil {
			return 0, err
		}
	}
	preferences, err := uriMediaPreferences(requestAcceptHeader(r), query.Get("contentType"))
	if err != nil {
		return 0, err
	}
	dicom := false
	for _, p := range preferences {
		if p.MediaType == "application/dicom" {
			dicom = true
		}
	}
	hasWindow := query.Has("windowCenter") || query.Has("windowWidth")
	hasPS := query.Has("presentationSeriesUID") || query.Has("presentationUID")
	if query.Has("rows") != query.Has("columns") || query.Has("windowCenter") != query.Has("windowWidth") || query.Has("presentationSeriesUID") != query.Has("presentationUID") || (hasWindow && hasPS) || (dicom && hasWindow) || (query.Has("annotation") && query.Has("imageAnnotation")) {
		return 0, ErrInvalidRequest
	}
	if hasPS {
		if s.validateUID(query.Get("presentationSeriesUID")) != nil || s.validateUID(query.Get("presentationUID")) != nil {
			return 0, ErrInvalidRequest
		}
	}
	if query.Has("anonymize") && query.Get("anonymize") != "yes" {
		return 0, ErrInvalidRequest
	}
	if query.Has("anonymize") || query.Has("charset") || query.Has("region") || hasPS || strings.TrimSpace(r.Header.Get("Accept-Charset")) != "" {
		return 0, newServerStatusError(http.StatusNotImplemented, "uri_option_not_implemented", ErrUnsupported)
	}
	if dicom {
		for _, k := range []string{"frameNumber", "rows", "columns", "imageQuality", "annotation", "imageAnnotation"} {
			if query.Has(k) {
				return 0, ErrInvalidRequest
			}
		}
		syntax := transfer.ExplicitVRLittleEndian.UID
		if query.Has("transferSyntax") {
			syntax = query.Get("transferSyntax")
			if s.validateUID(syntax) != nil {
				return 0, ErrInvalidRequest
			}
			if !webServiceTransferSyntaxAllowed(syntax) {
				return 0, ErrUnsupported
			}
		}
		for i := range preferences {
			preferences[i].TransferSyntaxUID = syntax
		}
		request := RetrieveRequest{Level: RetrieveLevelInstance, StudyInstanceUID: ref.StudyInstanceUID, SeriesInstanceUID: ref.SeriesInstanceUID, SOPInstanceUID: ref.SOPInstanceUID, Accept: preferences}
		return s.servePreparedRetrieve(w, r, request, true)
	}
	if query.Has("transferSyntax") {
		return 0, ErrInvalidRequest
	}
	mapped := url.Values{}
	if query.Has("rows") {
		for _, k := range []string{"rows", "columns"} {
			n, err := uriPositiveInteger(query.Get(k))
			if err != nil || n == 0 {
				return 0, ErrInvalidRequest
			}
		}
		mapped.Set("viewport", query.Get("columns")+","+query.Get("rows"))
	}
	if hasWindow {
		mapped.Set("window", query.Get("windowCenter")+","+query.Get("windowWidth")+",linear")
	}
	if query.Has("imageQuality") {
		n, err := uriPositiveInteger(query.Get("imageQuality"))
		if err != nil || n == 0 || n > 100 {
			return 0, ErrInvalidRequest
		}
		mapped.Set("quality", query.Get("imageQuality"))
	}
	for _, k := range []string{"annotation", "imageAnnotation"} {
		if query.Has(k) {
			mapped.Set("annotation", query.Get(k))
		}
	}
	path := "/studies/" + ref.StudyInstanceUID + "/series/" + ref.SeriesInstanceUID + "/instances/" + ref.SOPInstanceUID
	if query.Has("frameNumber") {
		n, err := uriPositiveInteger(query.Get("frameNumber"))
		if err != nil || n == 0 || n > uint64(s.limits.MaxFrames) {
			return 0, ErrInvalidRequest
		}
		path += "/frames/" + strconv.FormatUint(n, 10)
	}
	path += "/rendered"
	forwarded := r.Clone(r.Context())
	u := *r.URL
	u.Path, u.RawPath, u.RawQuery = path, "", mapped.Encode()
	forwarded.URL = &u
	var accept []string
	for _, p := range preferences {
		accept = append(accept, mime.FormatMediaType(p.MediaType, map[string]string{"q": strconv.FormatFloat(p.Quality, 'f', -1, 64)}))
	}
	forwarded.Header.Set("Accept", strings.Join(accept, ","))
	return s.serveRendered(w, forwarded, false)
}

func uriPositiveInteger(raw string) (uint64, error) {
	for i := range raw {
		if raw[i] < '0' || raw[i] > '9' {
			return 0, ErrInvalidRequest
		}
	}
	return strconv.ParseUint(raw, 10, 31)
}

// URI media parameters differ from RS: transfer-syntax and charset are query
// parameters only. q=0 is retained until specificity resolution, so contentType
// cannot override a header exclusion. The profile does not mix DICOM/rendered.
func uriMediaPreferences(header, contentType string) ([]MediaPreference, error) {
	parse := func(raw string, query bool) ([]MediaPreference, error) {
		result := parseAcceptHeader(raw)
		if len(result) == 0 || len(result) != len(splitAcceptValues(raw)) {
			if query {
				return nil, ErrInvalidRequest
			}
			return nil, ErrUnsupported
		}
		dicom, rendered := false, false
		for _, part := range splitAcceptValues(raw) {
			media, params, err := mime.ParseMediaType(strings.TrimSpace(part))
			if err != nil {
				return nil, ErrInvalidRequest
			}
			for k := range params {
				if k != "q" {
					return nil, ErrInvalidRequest
				}
			}
			if query && strings.Contains(media, "*") {
				return nil, ErrInvalidRequest
			}
		}
		for _, p := range result {
			if p.Multipart {
				return nil, ErrInvalidRequest
			}
			if p.Quality > 0 {
				if p.MediaType == "application/dicom" {
					dicom = true
				} else if !strings.Contains(p.MediaType, "*") {
					rendered = true
				}
			}
		}
		if dicom && rendered {
			return nil, ErrInvalidRequest
		}
		return result, nil
	}
	headers, err := parse(header, false)
	if err != nil {
		return nil, err
	}
	candidates := []string{"application/dicom", "image/jpeg", "image/png", "image/gif", "image/jp2", "image/jph", "image/jphc", "image/jxl", "video/mpeg", "video/mp4", "video/h265", "text/html", "text/plain", "application/pdf"}
	queryPreferences := headers
	if contentType != "" {
		queryPreferences, err = parse(contentType, true)
		if err != nil {
			return nil, err
		}
		candidates = nil
		for _, p := range queryPreferences {
			candidates = append(candidates, p.MediaType)
		}
	}
	result := make([]MediaPreference, 0, len(candidates))
	dicom, rendered := false, false
	for _, media := range candidates {
		if media != "application/dicom" && !isRenderedMediaType(media, false) {
			continue
		}
		hp, ok := bestMatchingMediaPreference(headers, media, "", false)
		if !ok || hp.Quality <= 0 {
			if contentType != "" {
				return nil, ErrUnsupported
			}
			continue
		}
		qp, ok := bestMatchingMediaPreference(queryPreferences, media, "", false)
		if !ok || qp.Quality <= 0 {
			continue
		}
		// A wildcard alone selects rendered output. DICOM via wildcard needs an
		// explicit contentType, so the two representation classes do not mix.
		if contentType == "" && hp.MediaType != "application/dicom" && media == "application/dicom" {
			continue
		}
		if media == "application/dicom" {
			dicom = true
		} else {
			rendered = true
		}
		result = append(result, MediaPreference{MediaType: media, Quality: qp.Quality})
	}
	if dicom && rendered {
		return nil, ErrInvalidRequest
	}
	if len(result) == 0 {
		return nil, ErrUnsupported
	}
	return result, nil
}
