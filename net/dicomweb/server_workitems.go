package dicomweb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dicomjson"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// workitemRouteOperation maps the PS3.18 section 11 resources:
//
//	POST   /workitems{?workitem}                          Create
//	GET    /workitems{?query}                             Search
//	GET    /workitems/{uid}                               Retrieve
//	POST   /workitems/{uid}{?transaction}                 Update
//	PUT    /workitems/{uid}/state                         Change State
//	POST   /workitems/{uid}/cancelrequest                 Request Cancellation
//	POST   /workitems/{uid}/subscribers/{ae}{?deletionlock,filter}  Subscribe
//	DELETE /workitems/{uid}/subscribers/{ae}              Unsubscribe
//	POST   /workitems/{global}/subscribers/{ae}/suspend   Suspend Global Subscription
//	GET    /subscribers/{ae} (WebSocket upgrade)          Open Event Channel
func workitemRouteOperation(method string, parts []string) Operation {
	if len(parts) == 2 && parts[0] == "subscribers" {
		if method == http.MethodGet {
			return OperationOpenEventChannel
		}
		return OperationUnknown
	}
	if len(parts) == 0 || parts[0] != "workitems" {
		return OperationUnknown
	}
	switch {
	case len(parts) == 1 && method == http.MethodGet:
		return OperationSearchWorkitems
	case len(parts) == 1 && method == http.MethodPost:
		return OperationCreateWorkitem
	case len(parts) == 2 && method == http.MethodGet:
		return OperationRetrieveWorkitem
	case len(parts) == 2 && method == http.MethodPost:
		return OperationUpdateWorkitem
	case len(parts) == 3 && parts[2] == "state" && method == http.MethodPut:
		return OperationChangeWorkitemState
	case len(parts) == 3 && parts[2] == "cancelrequest" && method == http.MethodPost:
		return OperationRequestWorkitemCancellation
	case len(parts) == 4 && parts[2] == "subscribers" && method == http.MethodPost:
		return OperationSubscribeWorkitem
	case len(parts) == 4 && parts[2] == "subscribers" && method == http.MethodDelete:
		return OperationUnsubscribeWorkitem
	case len(parts) == 5 && parts[2] == "subscribers" && parts[4] == "suspend" && method == http.MethodPost:
		return OperationSuspendWorkitemSubscription
	}
	return OperationUnknown
}

func workitemAllowedMethods(parts []string) []string {
	if len(parts) == 2 && parts[0] == "subscribers" {
		return []string{http.MethodGet}
	}
	if len(parts) == 0 || parts[0] != "workitems" {
		return nil
	}
	switch {
	case len(parts) == 1, len(parts) == 2:
		return []string{http.MethodGet, http.MethodPost}
	case len(parts) == 3 && parts[2] == "state":
		return []string{http.MethodPut}
	case len(parts) == 3 && parts[2] == "cancelrequest":
		return []string{http.MethodPost}
	case len(parts) == 4 && parts[2] == "subscribers":
		return []string{http.MethodPost, http.MethodDelete}
	case len(parts) == 5 && parts[2] == "subscribers" && parts[4] == "suspend":
		return []string{http.MethodPost}
	}
	return nil
}

func isWorkitemOperation(operation Operation) bool {
	switch operation {
	case OperationCreateWorkitem, OperationRetrieveWorkitem, OperationUpdateWorkitem,
		OperationChangeWorkitemState, OperationRequestWorkitemCancellation, OperationSearchWorkitems,
		OperationSubscribeWorkitem, OperationUnsubscribeWorkitem, OperationSuspendWorkitemSubscription,
		OperationOpenEventChannel:
		return true
	}
	return false
}

func (s *Server) workitemBackend() (WorkitemBackend, bool) {
	if s.options.Workitems != nil {
		return s.options.Workitems, true
	}
	backend, ok := s.options.Backend.(WorkitemBackend)
	return backend, ok
}

func (s *Server) serveWorkitems(w *serverResponseWriter, r *http.Request, operation Operation) (int, error) {
	parts := requestResourceParts(r.URL)
	if operation == OperationOpenEventChannel {
		return s.serveEventChannel(w, r, parts[1])
	}
	backend, ok := s.workitemBackend()
	if !ok {
		return 0, newServerStatusError(http.StatusNotImplemented, "not_implemented", ErrUnsupported)
	}
	var uid string
	if len(parts) >= 2 {
		uid = parts[1]
		if err := s.validateUID(uid); err != nil {
			return 0, err
		}
	}
	ctx := r.Context()
	switch operation {
	case OperationSearchWorkitems:
		return s.serveWorkitemSearch(w, r, backend)
	case OperationRetrieveWorkitem:
		if err := requireDICOMJSONResponse(r); err != nil {
			return 0, err
		}
		dataset, err := safeWorkitemCall(func() (Dataset, error) { return backend.RetrieveWorkitem(ctx, uid) })
		if err != nil {
			return 0, classifyWorkitemError(err)
		}
		return 1, s.writeWorkitemDatasets(w, r, http.StatusOK, []Dataset{dataset})
	case OperationCreateWorkitem:
		workitem, err := s.decodeWorkitemBody(w, r, true)
		if err != nil {
			return 0, err
		}
		uid, err := s.createWorkitemUID(r, workitem)
		if err != nil {
			return 0, err
		}
		created, err := safeWorkitemCall(func() (WorkitemCreated, error) {
			return backend.CreateWorkitem(ctx, WorkitemCreateRequest{SOPInstanceUID: uid, Workitem: workitem})
		})
		if err != nil {
			return 0, classifyWorkitemError(err)
		}
		if err := s.validateUID(created.SOPInstanceUID); err != nil {
			return 0, ErrBackend
		}
		location := s.servicePath("/workitems/" + url.PathEscape(created.SOPInstanceUID))
		w.Header().Set("Location", location)
		w.Header().Set("Content-Location", location)
		s.writeWorkitemResult(w, r, http.StatusCreated, created.WorkitemResult)
		return 1, nil
	case OperationUpdateWorkitem:
		query, err := s.workitemQuery(r, "transaction")
		if err != nil {
			return 0, err
		}
		transaction := query.Get("transaction")
		if transaction != "" {
			if err := s.validateUID(transaction); err != nil {
				return 0, err
			}
		}
		modifications, err := s.decodeWorkitemBody(w, r, true)
		if err != nil {
			return 0, err
		}
		result, err := safeWorkitemCall(func() (WorkitemResult, error) {
			return backend.UpdateWorkitem(ctx, WorkitemUpdateRequest{SOPInstanceUID: uid, TransactionUID: transaction, Modifications: modifications})
		})
		return s.finishWorkitemTransaction(w, r, http.StatusOK, result, err)
	case OperationChangeWorkitemState:
		if err := s.workitemRequester(r); err != nil {
			return 0, err
		}
		body, err := s.decodeWorkitemBody(w, r, true)
		if err != nil {
			return 0, err
		}
		state, stateOK := singleStringValue(body, core.Tag{Group: 0x0074, Element: 0x1000})
		transaction, transactionOK := singleStringValue(body, core.Tag{Group: 0x0008, Element: 0x1195})
		if !stateOK || !transactionOK || body.Len() != 2 {
			return 0, ErrInvalidRequest
		}
		result, err := safeWorkitemCall(func() (WorkitemResult, error) {
			return backend.ChangeWorkitemState(ctx, WorkitemStateRequest{SOPInstanceUID: uid, State: state, TransactionUID: transaction})
		})
		return s.finishWorkitemTransaction(w, r, http.StatusOK, result, err)
	case OperationRequestWorkitemCancellation:
		if err := s.workitemRequester(r); err != nil {
			return 0, err
		}
		information, err := s.decodeWorkitemBody(w, r, false)
		if err != nil {
			return 0, err
		}
		result, err := safeWorkitemCall(func() (WorkitemResult, error) {
			return backend.RequestWorkitemCancellation(ctx, WorkitemCancellationRequest{SOPInstanceUID: uid, Information: information})
		})
		return s.finishWorkitemTransaction(w, r, http.StatusAccepted, result, err)
	}
	return s.serveWorkitemSubscription(w, r, operation, uid, parts)
}

func (s *Server) serveWorkitemSubscription(w *serverResponseWriter, r *http.Request, operation Operation, uid string, parts []string) (int, error) {
	backend, _ := s.workitemBackend()
	subscriptions, ok := backend.(WorkitemSubscriptionBackend)
	if !ok {
		return 0, newServerStatusError(http.StatusNotImplemented, "not_implemented", ErrUnsupported)
	}
	aeTitle := parts[3]
	if !ValidAETitle(aeTitle) {
		return 0, ErrInvalidRequest
	}
	request := WorkitemSubscriptionRequest{SOPInstanceUID: uid, AETitle: aeTitle}
	ctx := r.Context()
	var (
		result WorkitemResult
		err    error
		status = http.StatusOK
	)
	switch operation {
	case OperationSubscribeWorkitem:
		query, queryErr := s.workitemQuery(r, "deletionlock", "filter")
		if queryErr != nil {
			return 0, queryErr
		}
		if values, present := query["deletionlock"]; present {
			lock, boolErr := singleBoolean(values)
			if boolErr != nil {
				return 0, ErrInvalidRequest
			}
			request.DeletionLock = lock
		}
		filter, filterErr := workitemSubscriptionFilters(r.URL.RawQuery)
		if filterErr != nil {
			return 0, filterErr
		}
		request.Filter = filter
		if len(request.Filter) > 0 && uid != FilteredGlobalWorkitemSubscriptionUID {
			return 0, ErrInvalidRequest
		}
		result, err = safeWorkitemCall(func() (WorkitemResult, error) { return subscriptions.SubscribeWorkitem(ctx, request) })
		// PS3.18 defines no Location for Subscribe; the event channel is
		// always {service}/subscribers/{aetitle}, so none is sent.
		status = http.StatusCreated
	case OperationUnsubscribeWorkitem:
		if _, queryErr := s.workitemQuery(r); queryErr != nil {
			return 0, queryErr
		}
		result, err = safeWorkitemCall(func() (WorkitemResult, error) { return subscriptions.UnsubscribeWorkitem(ctx, request) })
	case OperationSuspendWorkitemSubscription:
		if _, queryErr := s.workitemQuery(r); queryErr != nil {
			return 0, queryErr
		}
		if uid != GlobalWorkitemSubscriptionUID && uid != FilteredGlobalWorkitemSubscriptionUID {
			return 0, ErrInvalidRequest
		}
		result, err = safeWorkitemCall(func() (WorkitemResult, error) {
			return subscriptions.SuspendWorkitemSubscription(ctx, request)
		})
	default:
		return 0, ErrNotFound
	}
	return s.finishWorkitemTransaction(w, r, status, result, err)
}

func (s *Server) serveEventChannel(w *serverResponseWriter, r *http.Request, aeTitle string) (int, error) {
	hub := s.options.WorkitemEvents
	if hub == nil {
		return 0, newServerStatusError(http.StatusNotImplemented, "not_implemented", ErrUnsupported)
	}
	if !ValidAETitle(aeTitle) {
		return 0, ErrInvalidRequest
	}
	if err := hub.accept(w, r, aeTitle); err != nil {
		if w.written {
			// The handshake failure response was already written.
			return 0, nil
		}
		return 0, err
	}
	w.status = http.StatusSwitchingProtocols
	w.written = true
	return 1, nil
}

func (s *Server) finishWorkitemTransaction(w *serverResponseWriter, r *http.Request, status int, result WorkitemResult, err error) (int, error) {
	if err != nil {
		return 0, classifyWorkitemError(err)
	}
	s.writeWorkitemResult(w, r, status, result)
	return 1, nil
}

func (s *Server) writeWorkitemResult(w *serverResponseWriter, r *http.Request, status int, result WorkitemResult) {
	for _, warning := range result.Warnings {
		if text := safeWarningText(warning); text != "" {
			w.Header().Add("Warning", fmt.Sprintf("299 %s: %s", s.serviceURI(r), text))
		}
	}
	w.WriteHeader(status)
}

// safeWarningText keeps a backend warning on one line of printable ASCII.
func safeWarningText(text string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(text) {
		if r >= 0x20 && r < 0x7F && r != '"' {
			b.WriteRune(r)
		}
		if b.Len() >= 200 {
			break
		}
	}
	return b.String()
}

// workitemQuery parses the query string and refuses parameters outside
// allowed.
func (s *Server) workitemQuery(r *http.Request, allowed ...string) (url.Values, error) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, ErrInvalidRequest
	}
	fields := 0
	for key, values := range query {
		fields += len(values)
		known := false
		for _, name := range allowed {
			known = known || key == name
		}
		if !known {
			return nil, ErrInvalidRequest
		}
		for _, value := range values {
			if len(value) > s.limits.MaxQueryValueBytes {
				return nil, ErrResourceLimit
			}
		}
	}
	if fields > s.limits.MaxQueryFields {
		return nil, ErrResourceLimit
	}
	return query, nil
}

// createWorkitemUID resolves the Create target from "workitem",
// "AffectedSOPInstanceUID" or a bare UID query, or from the dataset's SOP
// Instance UID, which is then removed from the dataset (PS3.4 carries it as
// the Affected SOP Instance UID). Two different UIDs are refused.
func (s *Server) createWorkitemUID(r *http.Request, workitem *object.Object) (string, error) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return "", ErrInvalidRequest
	}
	uid := ""
	for key, values := range query {
		switch {
		case key == "workitem" || key == "AffectedSOPInstanceUID":
			if len(values) != 1 {
				return "", ErrInvalidRequest
			}
			if uid != "" && uid != values[0] {
				return "", ErrInvalidRequest
			}
			uid = values[0]
		case len(values) == 1 && values[0] == "" && s.validateUID(key) == nil:
			if uid != "" && uid != key {
				return "", ErrInvalidRequest
			}
			uid = key
		default:
			return "", ErrInvalidRequest
		}
	}
	sopInstanceUID := core.Tag{Group: 0x0008, Element: 0x0018}
	if element, ok := workitem.Get(sopInstanceUID); ok {
		values := element.StringValues()
		if len(values) != 1 {
			return "", ErrInvalidRequest
		}
		datasetUID := strings.TrimRight(values[0], "\x00 ")
		if uid != "" && datasetUID != uid {
			return "", ErrConflict
		}
		uid = datasetUID
		workitem.Remove(sopInstanceUID)
	}
	if uid != "" {
		if err := s.validateUID(uid); err != nil {
			return "", err
		}
	}
	return uid, nil
}

func singleStringValue(dataset *object.Object, tag core.Tag) (string, bool) {
	element, ok := dataset.Get(tag)
	if !ok {
		return "", false
	}
	values := element.StringValues()
	if len(values) != 1 {
		return "", false
	}
	value := strings.TrimRight(values[0], "\x00 ")
	return value, strings.TrimSpace(value) != ""
}

// decodeWorkitemBody reads one bounded DICOM JSON dataset (a JSON array with
// exactly one object, or a bare object). An absent body is allowed only when
// required is false. Bulk data references and group 0002 are refused.
func (s *Server) decodeWorkitemBody(w http.ResponseWriter, r *http.Request, required bool) (*object.Object, error) {
	if hasContentCoding(r.Header) {
		return nil, newServerStatusError(http.StatusUnsupportedMediaType, "unsupported_media_type", ErrUnsupported)
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.limits.MaxMetadataBytes)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, classifyMultipartError(err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		if required {
			return nil, ErrInvalidRequest
		}
		return nil, nil
	}
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, string(MetadataMediaTypeDICOMJSON)) {
		return nil, newServerStatusError(http.StatusUnsupportedMediaType, "unsupported_media_type", ErrUnsupported)
	}
	for key, value := range params {
		if key != "charset" || !strings.EqualFold(value, "utf-8") {
			return nil, newServerStatusError(http.StatusUnsupportedMediaType, "unsupported_media_type", ErrUnsupported)
		}
	}
	raw := bytes.TrimSpace(data)
	if raw[0] == '[' {
		var datasets []json.RawMessage
		if err := decodeJSON(raw, &datasets); err != nil || len(datasets) != 1 {
			return nil, ErrInvalidRequest
		}
		raw = datasets[0]
	}
	workitem, err := dicomjson.UnmarshalContext(r.Context(), raw, std.Dictionary, dicomjson.UnmarshalOptions{
		TransferSyntax: transfer.ExplicitVRLittleEndian,
		Limits: dicomjson.Limits{
			MaxJSONBytes: int64(len(raw)), MaxInlineBinaryBytes: s.limits.MaxMetadataBytes,
			MaxTotalBinaryBytes: s.limits.MaxMetadataBytes, MaxSequenceDepth: s.limits.MaxJSONDepth,
			MaxElements: s.limits.MaxJSONValues, MaxSequenceItems: s.limits.MaxJSONValues,
		},
	})
	if err != nil {
		return nil, classifyStoreMetadataDecodeError(err)
	}
	err = workitem.WalkContext(r.Context(), object.WalkOptions{MaxDepth: s.limits.MaxJSONDepth, MaxElements: s.limits.MaxJSONValues}, func(_ []core.Tag, element core.Element) error {
		if element.Tag().Group == 0x0002 {
			return ErrInvalidRequest
		}
		if _, bulk := element.Value.(core.BulkDataValue); bulk {
			return ErrInvalidRequest
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		if errors.Is(err, object.ErrWalkResourceLimit) {
			return nil, ErrResourceLimit
		}
		return nil, ErrInvalidRequest
	}
	return workitem, nil
}

// requireDICOMJSONResponse accepts an absent Accept header or one that
// admits application/dicom+json; workitems have no XML representation here.
func requireDICOMJSONResponse(r *http.Request) error {
	accept := strings.TrimSpace(requestAcceptHeader(r))
	if accept == "" {
		return nil
	}
	if representation, ok := selectMetadataRepresentation(accept); ok && representation == metadataRepresentationJSON {
		return nil
	}
	preferences := parseMetadataAcceptHeader(accept)
	if preference, ok := bestMatchingMediaPreference(preferences, string(MetadataMediaTypeDICOMJSON), transfer.ExplicitVRLittleEndian.UID, false); ok && preference.Quality > 0 {
		return nil
	}
	return newServerStatusError(http.StatusNotAcceptable, "not_acceptable", ErrUnsupported)
}

func (s *Server) writeWorkitemDatasets(w *serverResponseWriter, r *http.Request, status int, datasets []Dataset) error {
	var body bytes.Buffer
	body.WriteByte('[')
	for index, dataset := range datasets {
		data, err := s.encodeResponseDataset(r.Context(), dataset)
		if err != nil {
			return err
		}
		if index > 0 {
			body.WriteByte(',')
		}
		body.Write(data)
	}
	body.WriteByte(']')
	if int64(body.Len()) > s.limits.MaxResponseBytes {
		return ErrResourceLimit
	}
	w.Header().Set("Content-Type", string(MetadataMediaTypeDICOMJSON))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, err := w.Write(body.Bytes())
	return err
}

type workitemSearchBackend struct{ backend WorkitemBackend }

func (b workitemSearchBackend) Search(ctx context.Context, request SearchRequest, yield func(Dataset) error) (SearchResult, error) {
	return b.backend.SearchWorkitems(ctx, request, yield)
}

func (s *Server) serveWorkitemSearch(w *serverResponseWriter, r *http.Request, backend WorkitemBackend) (int, error) {
	if err := requireDICOMJSONResponse(r); err != nil {
		return 0, err
	}
	request, err := s.parseSearchRequest(r, OperationSearchWorkitems)
	if err != nil {
		return 0, err
	}
	request.Level = SearchLevelWorkitem
	return s.runSearch(w, r, workitemSearchBackend{backend: backend}, request, metadataRepresentationJSON)
}

func safeWorkitemCall[T any](call func() (T, error)) (result T, err error) {
	defer func() {
		if recover() != nil {
			var zero T
			result, err = zero, ErrBackend
		}
	}()
	return call()
}

// classifyWorkitemError keeps the sentinels a workitem backend may return
// and redacts everything else.
func classifyWorkitemError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrForbidden):
		return newServerStatusError(http.StatusForbidden, "forbidden", ErrForbidden)
	case errors.Is(err, ErrResourceLimit):
		var statusErr *serverStatusError
		if errors.As(err, &statusErr) {
			return err
		}
		return newServerStatusError(http.StatusServiceUnavailable, "resource_limit", ErrResourceLimit)
	}
	return classifyBackendError(err)
}

// workitemRequester admits the optional "requester" query parameter of the
// Change Workitem State and Request Cancellation transactions (PS3.18 11.7
// and 11.8). The value must be an AE Title.
func (s *Server) workitemRequester(r *http.Request) error {
	query, err := s.workitemQuery(r, "requester")
	if err != nil {
		return err
	}
	if values, present := query["requester"]; present && (len(values) != 1 || !ValidAETitle(values[0])) {
		return ErrInvalidRequest
	}
	return nil
}

// workitemSubscriptionFilters parses the matching keys of a filtered global
// subscription from the raw query. PS3.18 11.10 sends one "filter" parameter
// holding a comma-separated list of attribute=value pairs, the URI template
// expansion of a list: the separating commas are literal and each pair is
// percent-encoded. Only literal commas separate pairs, so the earlier form of
// one percent-encoded "filter" parameter per pair, whose value may hold an
// encoded comma ("filter=00741202%3DCT%2C%20MR"), is still read as one pair.
// Both forms may be mixed.
func workitemSubscriptionFilters(rawQuery string) ([]SearchFilter, error) {
	var filters []SearchFilter
	for _, field := range strings.Split(rawQuery, "&") {
		rawKey, list, _ := strings.Cut(field, "=")
		if key, err := url.QueryUnescape(rawKey); err != nil {
			return nil, ErrInvalidRequest
		} else if key != "filter" {
			continue
		}
		for _, rawPair := range strings.Split(list, ",") {
			pair, err := url.QueryUnescape(rawPair)
			if err != nil {
				return nil, ErrInvalidRequest
			}
			key, value, found := strings.Cut(pair, "=")
			if !found || !validAttributeIdentifier(key) || value == "" {
				return nil, ErrInvalidRequest
			}
			filters = append(filters, SearchFilter{Key: key, Values: []string{value}})
		}
	}
	return filters, nil
}
