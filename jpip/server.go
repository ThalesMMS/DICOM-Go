package jpip

import (
	"bytes"
	"container/list"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ServerConfig bounds a Server. Zero fields use the defaults.
type ServerConfig struct {
	Provider TargetProvider
	// MaxSessions bounds open JPIP sessions (channels); a further cnew is
	// answered 503 (default 64).
	MaxSessions int
	// SessionIdleTimeout closes sessions without requests (default 5 min).
	SessionIdleTimeout time.Duration
	// MaxConcurrentRequests bounds requests in progress; others are answered
	// 503 with Retry-After (default 16).
	MaxConcurrentRequests int
	// MaxResponseBytes bounds one JPP/JPT response body; a larger window ends
	// with EOR "response limit reached" and continues in the next request of
	// a session (default 16 MiB).
	MaxResponseBytes int
	// MaxQueryBytes bounds the request query (default 4 KiB, else 414).
	MaxQueryBytes int
	// MaxCacheBytes bounds the codestream bytes of indexed targets kept
	// between requests (default 512 MiB).
	MaxCacheBytes int64
	// Index bounds the indexing of one codestream.
	Index IndexLimits
	// OnEvent, when set, receives one PHI-free event per request and session
	// change: kinds, statuses, sizes and durations, never target names,
	// query strings or channel identifiers.
	OnEvent func(ServerEvent)
	// Now replaces time.Now in tests.
	Now func() time.Time
}

const (
	defaultMaxSessions           = 64
	defaultSessionIdleTimeout    = 5 * time.Minute
	defaultMaxConcurrentRequests = 16
	defaultMaxServerResponse     = 16 << 20
	defaultMaxQueryBytes         = 4 << 10
	defaultMaxCacheBytes         = 512 << 20
)

// ServerEventKind classifies a ServerEvent.
type ServerEventKind string

const (
	EventRequest        ServerEventKind = "request"
	EventSessionOpened  ServerEventKind = "session-opened"
	EventSessionClosed  ServerEventKind = "session-closed"
	EventSessionExpired ServerEventKind = "session-expired"
)

// ServerEvent is a diagnostic record without PHI.
type ServerEvent struct {
	Kind        ServerEventKind
	Status      int
	ContentType string
	// Reason is a stable category for refusals ("busy", "not-found", ...).
	Reason   string
	Bytes    int
	Messages int
	EOR      EORReason
	Sessions int
	Duration time.Duration
}

// Server is a JPIP server (ISO/IEC 15444-9) over HTTP for JPEG 2000 targets.
// It answers stateless requests and requests on sessions opened with
// cnew=http, whose server-side cache model avoids resending data-bin bytes.
// Responses are JPP-streams (precinct data-bins) or JPT-streams (tile
// data-bins) selected with the type request field; a stateless request
// without type receives the complete codestream as image/jp2 (image/jphc for
// HTJ2K), which jpip.Client decodes directly.
//
// Supported request fields: target, tid, cid, cnew, cclose, stream (one-based
// codestream identifier), type, fsiz, roff, rsiz, comps, layers and len.
// Other fields are ignored, which ISO/IEC 15444-9 permits as the server may
// send more data than asked. Like the rest of this module's network code,
// the server has no authentication: deploy it behind a trusted transport.
type Server struct {
	config ServerConfig
	slots  chan struct{}

	mu       sync.Mutex
	closed   bool
	sessions map[string]*session
	cache    map[string]*list.Element
	lru      *list.List
	cached   int64
}

type session struct {
	mu       sync.Mutex
	id       string
	target   string
	tid      string
	model    map[dataBinKey]int64
	lastUsed time.Time
	closed   atomic.Bool
}

type cachedTarget struct {
	name    string
	tid     string
	target  Target
	bytes   int64
	mu      sync.Mutex
	indexes map[int]*CodestreamIndex
	errs    map[int]error
}

// NewServer validates config.
func NewServer(config ServerConfig) (*Server, error) {
	if config.Provider == nil {
		return nil, errors.New("jpip: server needs a target provider")
	}
	if config.MaxSessions <= 0 {
		config.MaxSessions = defaultMaxSessions
	}
	if config.SessionIdleTimeout <= 0 {
		config.SessionIdleTimeout = defaultSessionIdleTimeout
	}
	if config.MaxConcurrentRequests <= 0 {
		config.MaxConcurrentRequests = defaultMaxConcurrentRequests
	}
	if config.MaxResponseBytes <= 0 {
		config.MaxResponseBytes = defaultMaxServerResponse
	}
	if config.MaxResponseBytes < 64 {
		return nil, errors.New("jpip: server response limit is too small")
	}
	if config.MaxQueryBytes <= 0 {
		config.MaxQueryBytes = defaultMaxQueryBytes
	}
	if config.MaxCacheBytes <= 0 {
		config.MaxCacheBytes = defaultMaxCacheBytes
	}
	config.Index = config.Index.normalized()
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Server{
		config:   config,
		slots:    make(chan struct{}, config.MaxConcurrentRequests),
		sessions: map[string]*session{},
		cache:    map[string]*list.Element{},
		lru:      list.New(),
	}, nil
}

// Close ends every session and drops cached targets; later requests are
// answered 503.
func (s *Server) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for id, sess := range s.sessions {
		sess.closed.Store(true)
		delete(s.sessions, id)
	}
	s.cache = map[string]*list.Element{}
	s.lru.Init()
	s.cached = 0
}

// Sessions returns the number of open sessions.
func (s *Server) Sessions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

func (s *Server) emit(event ServerEvent) {
	if s.config.OnEvent != nil {
		s.config.OnEvent(event)
	}
}

type requestError struct {
	status int
	reason string
}

func (e *requestError) Error() string { return e.reason }

func fail(status int, reason string) error { return &requestError{status: status, reason: reason} }

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := s.config.Now()
	event := ServerEvent{Kind: EventRequest}
	defer func() {
		event.Duration = s.config.Now().Sub(started)
		s.emit(event)
	}()
	w.Header().Set("Cache-Control", "no-store")
	refuse := func(err error) {
		var re *requestError
		if !errors.As(err, &re) {
			re = &requestError{status: http.StatusInternalServerError, reason: "internal"}
		}
		event.Status, event.Reason = re.status, re.reason
		if re.status == http.StatusServiceUnavailable {
			w.Header().Set("Retry-After", "1")
		}
		http.Error(w, http.StatusText(re.status), re.status)
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		refuse(fail(http.StatusMethodNotAllowed, "method"))
		return
	}
	if len(r.URL.RawQuery) > s.config.MaxQueryBytes {
		refuse(fail(http.StatusRequestURITooLong, "query-too-long"))
		return
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		refuse(fail(http.StatusServiceUnavailable, "busy"))
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		refuse(fail(http.StatusBadRequest, "query"))
		return
	}
	if err := s.serve(w, r, query, &event); err != nil {
		refuse(err)
	}
}

type viewWindow struct {
	explicitSize   bool
	fx, fy         int64
	round          string
	ox, oy, sx, sy int64
	hasOffset      bool
	hasRegion      bool
	comps          []bool
	layers         int // -1: all
	length         int // -1: no limit
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request, query url.Values, event *ServerEvent) error {
	s.expireSessions()
	var sess *session
	if cid := query.Get("cid"); cid != "" {
		s.mu.Lock()
		sess = s.sessions[cid]
		s.mu.Unlock()
		if sess == nil {
			return fail(http.StatusNotFound, "unknown-channel")
		}
	}
	targetName := query.Get("target")
	if sess != nil {
		if targetName != "" && targetName != sess.target {
			return fail(http.StatusBadRequest, "channel-target-mismatch")
		}
		targetName = sess.target
	}
	if targetName == "" {
		return fail(http.StatusBadRequest, "missing-target")
	}
	newChannel := false
	if cnew := query.Get("cnew"); cnew != "" {
		if !listContains(cnew, "http") {
			return fail(http.StatusNotImplemented, "transport")
		}
		newChannel = true
	}
	stream := 1
	if raw := query.Get("stream"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return fail(http.StatusBadRequest, "stream")
		}
		stream = n
	}
	window, err := parseViewWindow(query)
	if err != nil {
		return err
	}
	entry, err := s.resolve(r.Context(), targetName)
	if err != nil {
		return err
	}
	if stream > len(entry.target.Codestreams) {
		return fail(http.StatusNotFound, "stream")
	}
	index, err := entry.index(stream-1, s.config.Index)
	if err != nil {
		if errors.Is(err, ErrIndexLimit) {
			return fail(http.StatusInsufficientStorage, "index-limit")
		}
		return fail(http.StatusUnprocessableEntity, "codestream")
	}
	s.touch(entry)

	types := query.Get("type")
	if types == "" && sess == nil && !newChannel {
		return s.serveRaw(w, r, entry, index, event)
	}
	kind, ok := chooseStreamType(types, index)
	if !ok {
		return fail(http.StatusUnsupportedMediaType, "type")
	}
	if newChannel {
		if sess, err = s.openSession(targetName, entry.tid); err != nil {
			return err
		}
		w.Header().Set("JPIP-cnew", "cid="+sess.id+",path="+r.URL.Path+",transport=http")
		event.Sessions = s.Sessions()
	}
	if closing := query.Get("cclose"); closing != "" {
		defer s.closeSessions(closing)
	}
	var model map[dataBinKey]int64
	if sess != nil {
		sess.mu.Lock()
		defer sess.mu.Unlock()
		if sess.closed.Load() {
			return fail(http.StatusNotFound, "unknown-channel")
		}
		sess.lastUsed = s.config.Now()
		if tid := query.Get("tid"); sess.tid != entry.tid || tid != "" && tid != "0" && tid != entry.tid {
			sess.model = map[dataBinKey]int64{}
			sess.tid = entry.tid
		}
		model = sess.model
	} else {
		model = map[dataBinKey]int64{}
	}
	plan := newPlan(index, uint64(stream), kind, window)
	body := plan.write(model, window.length, s.config.MaxResponseBytes)
	contentType := "image/" + kind
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("JPIP-tid", entry.tid)
	if window.explicitSize {
		w.Header().Set("JPIP-fsiz", strconv.FormatInt(plan.fsizX, 10)+","+strconv.FormatInt(plan.fsizY, 10))
	}
	if window.length < 0 || window.length > s.config.MaxResponseBytes {
		w.Header().Set("JPIP-len", strconv.Itoa(s.config.MaxResponseBytes))
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body.data)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body.data)
	}
	event.Status, event.ContentType = http.StatusOK, contentType
	event.Bytes, event.Messages, event.EOR = len(body.data), body.messages, body.eor
	return nil
}

func (s *Server) serveRaw(w http.ResponseWriter, r *http.Request, entry *cachedTarget, index *CodestreamIndex, event *ServerEvent) error {
	contentType := "image/jp2"
	if index.HTJ2K() {
		contentType = "image/jphc"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("JPIP-tid", entry.tid)
	rec := &statusRecorder{ResponseWriter: w}
	http.ServeContent(rec, r, "", time.Time{}, bytes.NewReader(index.data))
	event.Status, event.ContentType, event.Bytes = rec.status, contentType, rec.bytes
	return nil
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += n
	return n, err
}

func listContains(list, want string) bool {
	for _, item := range strings.Split(list, ",") {
		if strings.EqualFold(strings.TrimSpace(item), want) {
			return true
		}
	}
	return false
}

// chooseStreamType picks the first acceptable return type in the client's
// order; without a type, a session receives precincts when available.
func chooseStreamType(types string, index *CodestreamIndex) (string, bool) {
	if types == "" {
		if index.Precincts() {
			return "jpp-stream", true
		}
		return "jpt-stream", true
	}
	for _, item := range strings.Split(types, ",") {
		item = strings.ToLower(strings.TrimSpace(item))
		item = strings.TrimPrefix(item, "image/")
		if semi := strings.IndexByte(item, ';'); semi >= 0 {
			item = strings.TrimSpace(item[:semi])
		}
		switch item {
		case "jpp-stream":
			if index.Precincts() {
				return item, true
			}
		case "jpt-stream":
			return item, true
		}
	}
	return "", false
}

func parsePair(raw string) (int64, int64, string, bool) {
	parts := strings.Split(raw, ",")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, 0, "", false
	}
	a, errA := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
	b, errB := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	if errA != nil || errB != nil || a < 0 || b < 0 || a > 1<<32 || b > 1<<32 {
		return 0, 0, "", false
	}
	extra := ""
	if len(parts) == 3 {
		extra = strings.TrimSpace(parts[2])
	}
	return a, b, extra, true
}

func parseViewWindow(query url.Values) (viewWindow, error) {
	w := viewWindow{layers: -1, length: -1, round: "round-down"}
	if raw := query.Get("fsiz"); raw != "" {
		fx, fy, round, ok := parsePair(raw)
		if !ok || fx == 0 || fy == 0 {
			return viewWindow{}, fail(http.StatusBadRequest, "fsiz")
		}
		switch round {
		case "":
		case "round-down", "round-up", "closest":
			w.round = round
		default:
			return viewWindow{}, fail(http.StatusBadRequest, "fsiz")
		}
		w.explicitSize, w.fx, w.fy = true, fx, fy
	}
	if raw := query.Get("roff"); raw != "" {
		x, y, extra, ok := parsePair(raw)
		if !ok || extra != "" {
			return viewWindow{}, fail(http.StatusBadRequest, "roff")
		}
		w.ox, w.oy, w.hasOffset = x, y, true
	}
	if raw := query.Get("rsiz"); raw != "" {
		x, y, extra, ok := parsePair(raw)
		if !ok || extra != "" || x == 0 || y == 0 {
			return viewWindow{}, fail(http.StatusBadRequest, "rsiz")
		}
		w.sx, w.sy, w.hasRegion = x, y, true
	}
	if raw := query.Get("comps"); raw != "" {
		comps, ok := parseComponentRanges(raw)
		if !ok {
			return viewWindow{}, fail(http.StatusBadRequest, "comps")
		}
		w.comps = comps
	}
	if raw := query.Get("layers"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return viewWindow{}, fail(http.StatusBadRequest, "layers")
		}
		w.layers = n
	}
	if raw := query.Get("len"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return viewWindow{}, fail(http.StatusBadRequest, "len")
		}
		w.length = n
	}
	return w, nil
}

// parseComponentRanges reads "0,2-4,6-" into a component mask.
func parseComponentRanges(raw string) ([]bool, bool) {
	mask := make([]bool, 0)
	set := func(from, to int) bool {
		if from < 0 || to < from || to >= maxComponents {
			return false
		}
		for len(mask) <= to {
			mask = append(mask, false)
		}
		for c := from; c <= to; c++ {
			mask[c] = true
		}
		return true
	}
	for _, item := range strings.Split(raw, ",") {
		from, to, ranged := strings.Cut(strings.TrimSpace(item), "-")
		a, err := strconv.Atoi(from)
		if err != nil {
			return nil, false
		}
		b := a
		if ranged {
			if to == "" {
				b = maxComponents - 1
			} else if b, err = strconv.Atoi(to); err != nil {
				return nil, false
			}
		}
		if !set(a, b) {
			return nil, false
		}
	}
	return mask, true
}

func (s *Server) resolve(ctx context.Context, name string) (*cachedTarget, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, fail(http.StatusServiceUnavailable, "closed")
	}
	if elem := s.cache[name]; elem != nil {
		s.lru.MoveToFront(elem)
		entry := elem.Value.(*cachedTarget)
		s.mu.Unlock()
		return entry, nil
	}
	s.mu.Unlock()
	target, err := s.config.Provider.Target(ctx, name)
	if err != nil {
		if errors.Is(err, ErrTargetNotFound) {
			return nil, fail(http.StatusNotFound, "not-found")
		}
		if ctx.Err() != nil {
			return nil, fail(http.StatusServiceUnavailable, "canceled")
		}
		return nil, fail(http.StatusInternalServerError, "provider")
	}
	if len(target.Codestreams) == 0 {
		return nil, fail(http.StatusNotFound, "empty-target")
	}
	entry := &cachedTarget{name: name, tid: targetID(target), target: target, indexes: map[int]*CodestreamIndex{}, errs: map[int]error{}}
	for _, codestream := range target.Codestreams {
		if len(codestream) > s.config.Index.MaxCodestreamBytes {
			return nil, fail(http.StatusInsufficientStorage, "index-limit")
		}
		entry.bytes += int64(len(codestream))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if elem := s.cache[name]; elem != nil {
		s.lru.MoveToFront(elem)
		return elem.Value.(*cachedTarget), nil
	}
	s.cache[name] = s.lru.PushFront(entry)
	s.cached += entry.bytes
	s.evictLocked()
	return entry, nil
}

// touch re-accounts an entry after indexing grew it.
func (s *Server) touch(entry *cachedTarget) {
	entry.mu.Lock()
	size := int64(0)
	for _, codestream := range entry.target.Codestreams {
		size += int64(len(codestream))
	}
	for _, index := range entry.indexes {
		size += int64(index.packetCount) * 24
	}
	entry.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if elem := s.cache[entry.name]; elem != nil && elem.Value.(*cachedTarget) == entry {
		s.cached += size - entry.bytes
		entry.bytes = size
		s.evictLocked()
	}
}

func (s *Server) evictLocked() {
	for s.cached > s.config.MaxCacheBytes && s.lru.Len() > 1 {
		elem := s.lru.Back()
		entry := elem.Value.(*cachedTarget)
		s.lru.Remove(elem)
		delete(s.cache, entry.name)
		s.cached -= entry.bytes
	}
}

func (e *cachedTarget) index(i int, limits IndexLimits) (*CodestreamIndex, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if index := e.indexes[i]; index != nil {
		return index, nil
	}
	if err := e.errs[i]; err != nil {
		return nil, err
	}
	index, err := IndexCodestream(e.target.Codestreams[i], limits)
	if err != nil {
		e.errs[i] = err
		return nil, err
	}
	e.indexes[i] = index
	return index, nil
}

func (s *Server) openSession(target, tid string) (*session, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, fail(http.StatusInternalServerError, "entropy")
	}
	sess := &session{id: hex.EncodeToString(raw[:]), target: target, tid: tid, model: map[dataBinKey]int64{}, lastUsed: s.config.Now()}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, fail(http.StatusServiceUnavailable, "closed")
	}
	if len(s.sessions) >= s.config.MaxSessions {
		s.mu.Unlock()
		return nil, fail(http.StatusServiceUnavailable, "session-limit")
	}
	s.sessions[sess.id] = sess
	count := len(s.sessions)
	s.mu.Unlock()
	s.emit(ServerEvent{Kind: EventSessionOpened, Sessions: count})
	return sess, nil
}

func (s *Server) closeSessions(ids string) {
	s.mu.Lock()
	closed := 0
	for _, id := range strings.Split(ids, ",") {
		id = strings.TrimSpace(id)
		if id == "*" {
			for key, sess := range s.sessions {
				sess.closed.Store(true)
				delete(s.sessions, key)
				closed++
			}
			continue
		}
		if sess := s.sessions[id]; sess != nil {
			sess.closed.Store(true)
			delete(s.sessions, id)
			closed++
		}
	}
	count := len(s.sessions)
	s.mu.Unlock()
	for ; closed > 0; closed-- {
		s.emit(ServerEvent{Kind: EventSessionClosed, Sessions: count})
	}
}

func (s *Server) expireSessions() {
	now := s.config.Now()
	s.mu.Lock()
	expired := 0
	for id, sess := range s.sessions {
		if sess.mu.TryLock() {
			idle := now.Sub(sess.lastUsed) > s.config.SessionIdleTimeout
			if idle {
				sess.closed.Store(true)
				delete(s.sessions, id)
				expired++
			}
			sess.mu.Unlock()
		}
	}
	count := len(s.sessions)
	s.mu.Unlock()
	for ; expired > 0; expired-- {
		s.emit(ServerEvent{Kind: EventSessionExpired, Sessions: count})
	}
}
