package dicomweb

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

type serverResponseWriter struct {
	http.ResponseWriter
	status    int
	written   bool
	remaining int64
	limit     int64
	readCtx   context.Context
}

func (w *serverResponseWriter) WriteHeader(status int) {
	if w.written {
		return
	}
	w.status = status
	w.written = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *serverResponseWriter) Write(data []byte) (int, error) {
	if w.readCtx != nil {
		if err := CheckReadLease(w.readCtx); err != nil {
			return 0, err
		}
	}
	if int64(len(data)) > w.remaining {
		return 0, ErrResourceLimit
	}
	if !w.written {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(data)
	w.remaining -= int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the connection, which the
// UPS-RS event channel hijacks for its WebSocket.
func (w *serverResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *serverResponseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *serverResponseWriter) clearProtectedReadHeaders() {
	if w.readCtx == nil {
		return
	}
	w.readCtx = nil
	for _, header := range []string{"Accept-Ranges", "Content-Length", "Content-Location", "Content-Range", "Content-Type", "Warning"} {
		w.Header().Del(header)
	}
}

func (s *Server) serveCore(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	writer := &serverResponseWriter{ResponseWriter: w, status: http.StatusOK, remaining: s.limits.MaxResponseBytes, limit: s.limits.MaxResponseBytes}
	requestID := ""
	if s.options.RequestID != nil {
		requestID = safeRequestID(s.options.RequestID, r)
		if requestID != "" {
			writer.Header().Set("X-Request-ID", requestID)
		}
	}
	operation := routeOperation(r)
	if operation == OperationRetrieveRendered || operation == OperationRetrieveThumbnail || operation == OperationRetrieveWADOURI || ((operation == OperationRetrieveStudy || operation == OperationRetrieveSeries || operation == OperationRetrieveInstance || operation == OperationRetrieveFrames) && s.options.RetrieveTranscode != nil) {
		writer.Header().Set("Cache-Control", "private, no-store")
		writer.Header().Set("Vary", "Accept")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
	}
	itemCount := 0
	errorCode := ""
	abortRead := false
	defer func() {
		if recovered := recover(); recovered != nil {
			errorCode = "panic"
			if !writer.written {
				writer.clearProtectedReadHeaders()
				writeServerError(writer, newServerStatusError(http.StatusInternalServerError, "backend_failure", ErrBackend))
			}
		}
		if s.options.Audit != nil {
			event := AuditEvent{Operation: operation, RequestID: requestID, StatusCode: writer.status, ItemCount: itemCount, Duration: time.Since(started), ErrorCode: errorCode}
			safeAudit(s.options.Audit, r.Context(), event)
		}
		if abortRead {
			panic(http.ErrAbortHandler)
		}
	}()

	if r == nil || r.URL == nil || len(r.RequestURI) > s.limits.MaxRequestURIBytes {
		errorCode = "invalid_request"
		writeServerError(writer, newServerStatusError(http.StatusRequestURITooLong, errorCode, ErrInvalidRequest))
		return
	}
	if requestHeaderBytes(r.Header) > s.limits.MaxHeaderBytes {
		errorCode = "request_headers_too_large"
		writeServerError(writer, newServerStatusError(http.StatusRequestHeaderFieldsTooLarge, errorCode, ErrResourceLimit))
		return
	}
	if err := s.acquireRequestSlot(r.Context(), w); err != nil {
		errorCode = errorCodeForServer(err)
		writeServerError(writer, err)
		return
	}
	defer func() { <-s.sem }()

	ctx := r.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	if s.limits.MaxDuration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.limits.MaxDuration)
		defer cancel()
	}
	if err := ctx.Err(); err != nil {
		errorCode = contextErrorCode(err)
		writeServerError(writer, err)
		return
	}
	r = r.WithContext(ctx)
	if operation == OperationUnknown {
		if allowed := allowedMethods(r.URL.Path); len(allowed) > 0 {
			writer.Header().Set("Allow", strings.Join(allowed, ", "))
			errorCode = "method_not_allowed"
			writeServerError(writer, newServerStatusError(http.StatusMethodNotAllowed, errorCode, ErrUnsupported))
			return
		}
		errorCode = "not_found"
		writeServerError(writer, newServerStatusError(http.StatusNotFound, errorCode, ErrNotFound))
		return
	}
	if s.options.Authorize == nil {
		if !s.options.AllowUnauthenticated {
			errorCode = "unauthorized"
			writer.Header().Set("WWW-Authenticate", `Bearer realm="dicomweb"`)
			writeServerError(writer, ErrUnauthorized)
			return
		}
	} else if err := safeAuthorize(s.options.Authorize, ctx, r, operation); err != nil {
		errorCode = authorizationErrorCode(err)
		writeServerError(writer, err)
		return
	}

	var lease *requestReadLease
	if s.options.AuthorizeReadLease != nil && isStudiesServiceRead(operation) {
		writer.Header().Set("Cache-Control", "private, no-store")
		grant, err := acquireReadLease(s.options.AuthorizeReadLease, r, operation)
		if err != nil {
			errorCode = "read_lease_denied"
			writeServerError(writer, err)
			return
		}
		deadline, stop := context.WithDeadlineCause(r.Context(), grant.ExpiresAt, ErrReadLeaseDenied)
		defer stop()
		ctx, cancel := context.WithCancelCause(deadline)
		defer cancel(nil)
		lease = &requestReadLease{ReadLease: grant, cancel: cancel}
		ctx = context.WithValue(ctx, readLeaseKey{}, lease)
		if err := CheckReadLease(ctx); err != nil {
			errorCode = errorCodeForServer(err)
			writeServerError(writer, err)
			return
		}
		writer.readCtx = ctx
		r = r.WithContext(ctx)
	}

	count, err := s.dispatch(writer, r, operation)
	itemCount = count
	if lease != nil && !writer.written {
		// Even an empty result or a status response can disclose the
		// existence of a resource, so check before sending it.
		if authorizationErr := lease.check(r.Context()); authorizationErr != nil {
			err = authorizationErr
		}
	}
	if lease != nil && (lease.denied.Load() ||
		(err != nil && errors.Is(lease.contextError(r.Context()), ErrReadLeaseDenied)) ||
		errors.Is(err, ErrReadLeaseDenied)) {
		err = ErrReadLeaseDenied
		abortRead = writer.written
		if !writer.written {
			itemCount = 0
		}
	}
	if err != nil {
		errorCode = errorCodeForServer(err)
		if !writer.written {
			// A failed read may have staged headers for data that is no
			// longer authorized, or for a body that will not be written.
			writer.clearProtectedReadHeaders()
			writeServerError(writer, err)
		}
	}
}

// acquireRequestSlot takes one of the MaxConcurrentRequests slots. With the
// slots taken, a request waits in the bounded queue for at most MaxQueueWait;
// a full queue or an elapsed wait is a 503 resource-limit error, and the
// request's cancellation ends the wait with its context error. The caller
// releases the slot by receiving from s.sem.
//
// The connection's read and write deadlines started with the request, before
// its wait. A request that waited has them moved to MaxDuration from the end
// of the wait, so that it keeps the MaxDuration its handler context allows
// to read its body and write its answer, its 503 included, instead of
// losing the connection with no status once the wait plus its work exceed
// the server's ReadTimeout or WriteTimeout.
func (s *Server) acquireRequestSlot(ctx context.Context, w http.ResponseWriter) error {
	busy := newServerStatusError(http.StatusServiceUnavailable, "busy", ErrResourceLimit)
	select {
	case s.sem <- struct{}{}:
		return nil
	default:
	}
	select {
	case s.queue <- struct{}{}:
		defer func() { <-s.queue }()
	default:
		return busy
	}
	timer := time.NewTimer(s.limits.MaxQueueWait)
	defer timer.Stop()
	select {
	case s.sem <- struct{}{}:
		s.renewConnectionDeadlines(w)
		return nil
	case <-timer.C:
		s.renewConnectionDeadlines(w)
		return busy
	case <-ctx.Done():
		return ctx.Err()
	}
}

// renewConnectionDeadlines moves the read and write deadlines of the
// connection serving w to MaxDuration from now. A writer that does not reach
// the connection (a wrapper without Unwrap) keeps the server's deadlines.
func (s *Server) renewConnectionDeadlines(w http.ResponseWriter) {
	if w == nil || s.limits.MaxDuration <= 0 {
		return
	}
	controller := http.NewResponseController(w)
	deadline := time.Now().Add(s.limits.MaxDuration)
	_ = controller.SetReadDeadline(deadline)
	_ = controller.SetWriteDeadline(deadline)
}

func safeRequestID(callback func(*http.Request) string, r *http.Request) (result string) {
	defer func() {
		if recover() != nil {
			result = ""
		}
	}()
	value := strings.TrimSpace(callback(r))
	if len(value) > 64 {
		value = value[:64]
	}
	for index, char := range value {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '-' || char == '_' || char == '.') {
			return value[:index]
		}
	}
	return value
}

func safeAudit(callback func(context.Context, AuditEvent), ctx context.Context, event AuditEvent) {
	defer func() { _ = recover() }()
	callback(ctx, event)
}
func safeAuthorize(callback Authorizer, ctx context.Context, r *http.Request, operation Operation) (err error) {
	defer func() {
		if recover() != nil {
			err = ErrForbidden
		}
	}()
	err = callback(ctx, r, operation)
	if err != nil && !errors.Is(err, ErrUnauthorized) && !errors.Is(err, ErrForbidden) {
		err = ErrForbidden
	}
	return
}
