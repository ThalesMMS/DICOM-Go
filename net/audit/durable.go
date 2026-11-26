package audit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"sync"
	"time"
)

var (
	ErrDurableSpool = errors.New("dicom: durable audit spool")
	ErrSpoolFull    = errors.New("dicom: durable audit spool full")
	ErrSpoolCorrupt = errors.New("dicom: durable audit spool corrupt")
	ErrSpoolClosed  = errors.New("dicom: durable audit spool closed")
)

// SpoolError is intentionally sanitized: Code is stable and contains no
// payload, patient identifier, path, or repository address.
type SpoolError struct {
	Code string
	kind error
}

func (e *SpoolError) Error() string            { return "dicom: durable audit " + e.Code }
func (e *SpoolError) Unwrap() error            { return e.kind }
func spoolError(code string, kind error) error { return &SpoolError{code, kind} }

// AuditDestination has a stable local ID. Reusing an ID with a changed
// transport/security fingerprint while it has pending records is refused.
type AuditDestination struct {
	ID, Network, Address string
	TLS                  *tls.Config
	// Fingerprint is a caller-supplied SHA256 policy digest. It is REQUIRED
	// for custom trust pools or TLS callbacks: CertPool cannot expose roots
	// completely, and callback identity/state cannot be fingerprinted. Change
	// this digest whenever such policy changes. The library cannot verify it.
	Fingerprint string
	Dial        func(context.Context, string, string) (net.Conn, error)
}

// DurableOptions is frozen by OpenDurableLogger. Zero limits select finite
// defaults. There are at most 16 active destinations and 64 suppression rules.
type DurableOptions struct {
	Directory                                    string
	Destinations                                 []AuditDestination
	AppName, Hostname                            string
	MaxMessages                                  int
	MaxBytes                                     int64
	MaxRecordBytes                               int
	MaxAge                                       time.Duration
	RetryInterval, MaxRetryInterval, SendTimeout time.Duration
	Suppression                                  []SuppressionRule
	// SourceIdentity binds a monotonic source cursor to its originating
	// application catalog. InitialSourceCursor applies only to a NEW spool.
	SourceIdentity      string
	InitialSourceCursor uint64
	// ResumeSource retires the disabled epoch at InitialSourceCursor before
	// workers start. Previously accepted records remain intact.
	ResumeSource bool
	// Now and Fault are deterministic test seams. Fault receives a stable
	// stage (record_sync/state_sync/state_rename/dir_sync/send/checkpoint/gc).
	Now   func() time.Time
	Fault func(stage string) error
}

type destinationState struct {
	Fingerprint string
	Cursor      uint64
}
type recordMeta struct {
	Sequence uint64
	Source   uint64
	Created  time.Time
	Bytes    int64
	Digest   string
	Targets  []string
}
type diskState struct {
	Version                                     int
	Next                                        uint64
	SourceIdentity                              string
	SourceCursor                                uint64
	SourcePaused                                bool
	Destinations                                map[string]destinationState
	Records                                     []recordMeta
	Expired, Suppressed, Sent, Failed, Rejected uint64
	SourceRetirements                           uint64
}

// DurableStats contains no payloads, identifiers or remote addresses. Pending
// per destination counts only records not yet checkpointed by that destination.
type DurableStats struct {
	Pending                                     int
	PendingBytes                                int64
	SourceCursor                                uint64
	SourcePaused                                bool
	Expired, Suppressed, Sent, Failed, Rejected uint64
	SourceRetirements                           uint64
	PersistenceFailures, Corruptions            uint64
	Destinations                                map[string]int
}

// DurableLogger owns its private disk spool and cloned TLS configurations.
// Log success means durable local acceptance, NOT remote persistence. Successful
// socket writes are checkpointed independently; a crash in that gap can repeat
// an attempt. Removed destinations pause pending records, never redirect them.
type DurableLogger struct {
	mu                               sync.Mutex
	options                          DurableOptions
	state                            diskState
	lock                             *os.File
	workers                          map[string]*durableWorker
	wake                             chan struct{}
	closed, poisoned                 bool
	persistenceFailures, corruptions uint64
	runMu                            sync.Mutex
	runCancel                        context.CancelFunc
	runDone                          chan struct{}
	lifecycleCtx                     context.Context
	lifecycleCancel                  context.CancelFunc
	active                           sync.WaitGroup
	closeOnce                        sync.Once
	closeErr                         error
	recoveryCtx                      context.Context
}

func normalizeDurable(o DurableOptions) (DurableOptions, error) {
	if o.Directory == "" || len(o.Directory) > 4096 || len(o.AppName) > 48 || len(o.Hostname) > 255 || len(o.Destinations) > 16 || len(o.SourceIdentity) > 256 {
		return o, spoolError("configuration", ErrSyslogConfig)
	}
	if o.MaxMessages == 0 {
		o.MaxMessages = 1000
	}
	if o.MaxBytes == 0 {
		o.MaxBytes = 16 << 20
	}
	if o.MaxRecordBytes == 0 {
		o.MaxRecordBytes = 4 << 20
	}
	if o.MaxAge == 0 {
		o.MaxAge = 30 * 24 * time.Hour
	}
	if o.RetryInterval == 0 {
		o.RetryInterval = 5 * time.Second
	}
	if o.MaxRetryInterval == 0 {
		o.MaxRetryInterval = time.Minute
	}
	if o.SendTimeout == 0 {
		o.SendTimeout = 10 * time.Second
	}
	if o.MaxMessages < 1 || o.MaxMessages > 100000 || o.MaxBytes < 1 || o.MaxBytes > 1<<40 || o.MaxRecordBytes < 1 || o.MaxRecordBytes > 64<<20 || int64(o.MaxRecordBytes) > o.MaxBytes || o.MaxAge < time.Second || o.MaxAge > 3650*24*time.Hour || o.RetryInterval < time.Millisecond || o.MaxRetryInterval < o.RetryInterval || o.MaxRetryInterval > time.Hour || o.SendTimeout < time.Millisecond || o.SendTimeout > time.Minute {
		return o, spoolError("limits", ErrSyslogConfig)
	}
	if err := ValidateSuppression(o.Suppression); err != nil {
		return o, err
	}
	o.Suppression = append([]SuppressionRule(nil), o.Suppression...)
	seen := map[string]bool{}
	ds := make([]AuditDestination, len(o.Destinations))
	for i, d := range o.Destinations {
		if !validDestinationID(d.ID) || seen[d.ID] || len(d.Address) > 1024 || len(d.Fingerprint) > 256 {
			return o, spoolError("destination_configuration", ErrSyslogConfig)
		}
		seen[d.ID] = true
		if _, _, err := net.SplitHostPort(d.Address); err != nil {
			return o, spoolError("destination_configuration", ErrSyslogConfig)
		}
		l := SyslogLogger{Network: d.Network, Address: d.Address, TLS: d.TLS}
		if err := l.check(); err != nil {
			return o, spoolError("destination_configuration", ErrSyslogConfig)
		}
		if d.TLS != nil {
			if customTLSPolicy(d.TLS) && (len(d.Fingerprint) != 64 || !validHexDigest(d.Fingerprint)) {
				return o, spoolError("tls_policy_fingerprint", ErrSyslogConfig)
			}
			var err error
			d.TLS, err = cloneTLSPolicy(d.TLS)
			if err != nil {
				return o, err
			}
		}
		ds[i] = d
	}
	o.Destinations = ds
	if o.Now == nil {
		o.Now = time.Now
	}
	return o, nil
}
func validDestinationID(id string) bool {
	if len(id) < 1 || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
func destinationFingerprint(d AuditDestination) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s", d.Network, d.Address, d.Fingerprint)
	if c := d.TLS; c != nil {
		fmt.Fprintf(h, "\x00%s/%d/%d/%t", c.ServerName, c.MinVersion, c.MaxVersion, c.InsecureSkipVerify)
		fmt.Fprintf(h, "/%v/%v/%q/%t/%d/%d/%t/%t/%t/%t/%t/%t/%t/%t", c.CipherSuites, c.CurvePreferences, c.NextProtos, c.SessionTicketsDisabled, c.Renegotiation, c.ClientAuth, c.VerifyPeerCertificate != nil, c.VerifyConnection != nil, c.GetCertificate != nil, c.GetClientCertificate != nil, c.GetConfigForClient != nil, c.WrapSession != nil, c.UnwrapSession != nil, c.Time != nil)
		for _, cert := range c.Certificates {
			for _, b := range cert.Certificate {
				h.Write(b)
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func validHexDigest(s string) bool { _, err := hex.DecodeString(s); return err == nil }
func customTLSPolicy(c *tls.Config) bool {
	return c.RootCAs != nil || c.ClientCAs != nil || c.VerifyPeerCertificate != nil || c.VerifyConnection != nil || c.GetCertificate != nil || c.GetClientCertificate != nil || c.GetConfigForClient != nil || c.WrapSession != nil || c.UnwrapSession != nil || c.Time != nil
}

// Private keys, randomness readers, caches, callback closures and their
// returned objects remain caller-owned and MUST stay immutable/thread-safe.
func cloneTLSPolicy(c *tls.Config) (*tls.Config, error) {
	n := c.Clone()
	if c.RootCAs != nil {
		n.RootCAs = c.RootCAs.Clone()
	}
	if c.ClientCAs != nil {
		n.ClientCAs = c.ClientCAs.Clone()
	}
	n.CipherSuites = append([]uint16(nil), c.CipherSuites...)
	n.CurvePreferences = append([]tls.CurveID(nil), c.CurvePreferences...)
	n.NextProtos = append([]string(nil), c.NextProtos...)
	cloneCertificate := func(c tls.Certificate) (tls.Certificate, error) {
		n := c
		n.Certificate = make([][]byte, len(c.Certificate))
		for i, b := range c.Certificate {
			n.Certificate[i] = bytes.Clone(b)
		}
		n.OCSPStaple = bytes.Clone(c.OCSPStaple)
		n.SignedCertificateTimestamps = make([][]byte, len(c.SignedCertificateTimestamps))
		for i, b := range c.SignedCertificateTimestamps {
			n.SignedCertificateTimestamps[i] = bytes.Clone(b)
		}
		n.SupportedSignatureAlgorithms = append([]tls.SignatureScheme(nil), c.SupportedSignatureAlgorithms...)
		if c.Leaf != nil {
			var err error
			n.Leaf, err = x509.ParseCertificate(bytes.Clone(c.Leaf.Raw))
			if err != nil {
				return n, spoolError("tls_leaf", ErrSyslogConfig)
			}
		}
		return n, nil
	}
	n.Certificates = make([]tls.Certificate, len(c.Certificates))
	for i, certificate := range c.Certificates {
		var err error
		n.Certificates[i], err = cloneCertificate(certificate)
		if err != nil {
			return nil, err
		}
	}
	if c.NameToCertificate != nil {
		n.NameToCertificate = make(map[string]*tls.Certificate, len(c.NameToCertificate))
		for name, certificate := range c.NameToCertificate {
			if certificate != nil {
				copy, err := cloneCertificate(*certificate)
				if err != nil {
					return nil, err
				}
				n.NameToCertificate[name] = &copy
			}
		}
	}
	return n, nil
}

// OpenDurableLogger recovers bounded records/checkpoints and takes a process
// lock. Missing/corrupt committed data fails closed. It never logs underlying
// OS/network errors. Directory/backing files must stay private and immutable.
func OpenDurableLogger(options DurableOptions) (*DurableLogger, error) {
	return OpenDurableLoggerContext(context.Background(), options)
}

// OpenDurableLoggerContext checks cancellation between bounded recovery IO.
// Native filesystem calls (including fsync) cannot be interrupted by Go ctx.
func OpenDurableLoggerContext(ctx context.Context, options DurableOptions) (*DurableLogger, error) {
	if ctx == nil {
		return nil, spoolError("context", ErrSyslogConfig)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	o, err := normalizeDurable(options)
	if err != nil {
		return nil, err
	}
	l := &DurableLogger{options: o, workers: map[string]*durableWorker{}, wake: make(chan struct{}, 1)}
	l.lifecycleCtx, l.lifecycleCancel = context.WithCancel(context.Background())
	l.recoveryCtx = ctx
	if err = l.openDisk(); err != nil {
		l.lifecycleCancel()
		if l.lock != nil {
			unlockSpool(l.lock)
		}
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		l.lifecycleCancel()
		unlockSpool(l.lock)
		return nil, err
	}
	for _, d := range o.Destinations {
		l.workers[d.ID] = newDurableWorker(d, l)
	}
	l.recoveryCtx = context.Background()
	return l, nil
}

func (l *DurableLogger) now() time.Time { return l.options.Now().UTC() }
func (l *DurableLogger) usable() error {
	if l.closed || l.lifecycleCtx.Err() != nil {
		return ErrSpoolClosed
	}
	if l.poisoned {
		return spoolError("persistence_uncertain", ErrDurableSpool)
	}
	return nil
}
func (l *DurableLogger) signal() {
	select {
	case l.wake <- struct{}{}:
	default:
	}
	for _, w := range l.workers {
		w.signal()
	}
}
func (l *DurableLogger) fault(stage string) error {
	if l.options.Fault != nil {
		return l.options.Fault(stage)
	}
	return nil
}

type boundedAuditWriter struct {
	ctx     context.Context
	buf     bytes.Buffer
	maximum int
}

func (w *boundedAuditWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if len(p) > w.maximum-w.buf.Len() {
		return 0, spoolError("record_limit", ErrSpoolFull)
	}
	return w.buf.Write(p)
}
func (l *DurableLogger) marshal(ctx context.Context, m Message) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(m.Participants) > l.options.MaxRecordBytes || len(m.Sources) > l.options.MaxRecordBytes || len(m.Objects) > l.options.MaxRecordBytes {
		return nil, spoolError("record_limit", ErrSpoolFull)
	}
	if err := m.validate(); err != nil {
		return nil, spoolError("message", ErrAuditMessage)
	}
	w := boundedAuditWriter{ctx: ctx, maximum: l.options.MaxRecordBytes}
	if _, err := w.Write([]byte(xml.Header)); err != nil {
		return nil, err
	}
	if err := xml.NewEncoder(&w).Encode(m); err != nil {
		return nil, err
	}
	return w.buf.Bytes(), nil
}

// Log and LogClinical accept independently of Run. They copy the encoded
// bytes before returning; callers retain ownership of their Message.
func (l *DurableLogger) Log(m Message) error { return l.LogSource(context.Background(), 0, m) }
func (l *DurableLogger) LogSource(ctx context.Context, source uint64, m Message) error {
	if ctx == nil {
		return spoolError("context", ErrSyslogConfig)
	}
	if suppressed(l.options.Suppression, m) {
		return l.advance(ctx, source, true)
	}
	p, err := l.marshal(ctx, m)
	if err != nil {
		return err
	}
	at, _ := time.Parse(time.RFC3339Nano, m.Event.DateTime)
	sev := 5
	if m.Event.OutcomeIndicator != OutcomeSuccess {
		sev = 4
	}
	return l.LogRawSource(ctx, source, sev, at, p)
}
func (l *DurableLogger) LogClinical(policy ClinicalPolicy, m Message) error {
	return l.LogClinicalContext(context.Background(), policy, m)
}

// LogClinicalContext checks cancellation before bounded clinical encoding and
// before admission. Once a disk commit starts, native sync cannot be canceled.
func (l *DurableLogger) LogClinicalContext(ctx context.Context, policy ClinicalPolicy, m Message) error {
	if ctx == nil {
		return spoolError("context", ErrSyslogConfig)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if suppressed(l.options.Suppression, m) {
		return l.advance(ctx, 0, true)
	}
	p, err := policy.Normalize()
	if err != nil {
		return err
	}
	p.Limits.MaxMessageBytes = min(p.Limits.MaxMessageBytes, l.options.MaxRecordBytes)
	b, err := m.MarshalClinical(p)
	if err != nil {
		return err
	}
	at, _ := time.Parse(time.RFC3339Nano, m.Event.DateTime)
	sev := 5
	if m.Event.OutcomeIndicator != OutcomeSuccess {
		sev = 4
	}
	return l.LogRawSource(ctx, 0, sev, at, b)
}
func (l *DurableLogger) LogRaw(severity int, at time.Time, payload []byte) error {
	return l.LogRawSource(context.Background(), 0, severity, at, payload)
}
func (l *DurableLogger) LogRawSource(ctx context.Context, source uint64, severity int, at time.Time, payload []byte) error {
	if ctx == nil || severity < 0 || severity > 7 {
		return spoolError("record_configuration", ErrSyslogConfig)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(payload) > l.options.MaxRecordBytes {
		return spoolError("record_limit", ErrSpoolFull)
	}
	if at.IsZero() {
		at = l.now()
	}
	record := (&SyslogLogger{AppName: l.options.AppName, Hostname: l.options.Hostname}).record(severity, at, payload)
	if len(record) > l.options.MaxRecordBytes {
		return spoolError("record_limit", ErrSpoolFull)
	}
	for _, d := range l.options.Destinations {
		if d.Network == SyslogUDP && len(record) > MaxSyslogUDPMessage {
			return spoolError("udp_record_limit", ErrSpoolFull)
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.usable(); err != nil {
		return err
	}
	if source > 0 && source <= l.state.SourceCursor {
		return nil
	}
	if err := l.expireLocked(); err != nil {
		return err
	}
	var size int64
	for _, r := range l.state.Records {
		size += r.Bytes
	}
	if len(l.state.Records) >= l.options.MaxMessages || int64(len(record)) > l.options.MaxBytes-size {
		next := l.cloneState()
		next.Rejected++
		if err := l.commit(next); err != nil {
			return err
		}
		return ErrSpoolFull
	}
	if len(l.workers) == 0 {
		return spoolError("no_active_destination", ErrSyslogConfig)
	}
	if l.state.Next == ^uint64(0) {
		return spoolError("sequence_limit", ErrDurableSpool)
	}
	targets := make([]string, 0, len(l.workers))
	for id := range l.workers {
		targets = append(targets, id)
	}
	sort.Strings(targets)
	digest := sha256.Sum256(record)
	meta := recordMeta{Sequence: l.state.Next, Source: source, Created: l.now(), Bytes: int64(len(record)), Digest: hex.EncodeToString(digest[:]), Targets: targets}
	if err := l.writeRecord(meta, record); err != nil {
		return l.diskFailure(err)
	}
	next := l.cloneState()
	next.Next++
	next.Records = append(next.Records, meta)
	if source > 0 {
		next.SourceCursor = source
	}
	if err := l.commit(next); err != nil {
		return err
	}
	l.signal()
	return nil
}

// AdvanceSource retires a catalog event intentionally, without queueing a
// payload. Used for disabled epochs or unreconstructable clinical captures.
func (l *DurableLogger) AdvanceSource(ctx context.Context, source uint64) error {
	return l.advance(ctx, source, false)
}

// PauseSource persists a disabled eligibility epoch. ResumeSource on open
// closes it at the catalog highwater, including crash between save and apply.
func (l *DurableLogger) PauseSource(ctx context.Context, through uint64) error {
	if ctx == nil {
		return spoolError("context", ErrSyslogConfig)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.usable(); err != nil {
		return err
	}
	next := l.cloneState()
	next.SourcePaused = true
	if through > next.SourceCursor {
		next.SourceCursor = through
		next.SourceRetirements++
	}
	return l.commit(next)
}
func (l *DurableLogger) advance(ctx context.Context, source uint64, isSuppressed bool) error {
	if ctx == nil {
		return spoolError("context", ErrSyslogConfig)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.usable(); err != nil {
		return err
	}
	if source > 0 && source <= l.state.SourceCursor {
		return nil
	}
	next := l.cloneState()
	if source > 0 {
		next.SourceCursor = source
	}
	if isSuppressed {
		next.Suppressed++
	} else if source > 0 {
		next.SourceRetirements++
	}
	return l.commit(next)
}
func (l *DurableLogger) SourceCursor() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.state.SourceCursor
}
func (l *DurableLogger) Stats() DurableStats {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := DurableStats{Pending: len(l.state.Records), SourceCursor: l.state.SourceCursor, SourcePaused: l.state.SourcePaused, Expired: l.state.Expired, Suppressed: l.state.Suppressed, Sent: l.state.Sent, Failed: l.state.Failed, Rejected: l.state.Rejected, SourceRetirements: l.state.SourceRetirements, PersistenceFailures: l.persistenceFailures, Corruptions: l.corruptions, Destinations: map[string]int{}}
	for _, r := range l.state.Records {
		s.PendingBytes += r.Bytes
		for _, id := range r.Targets {
			if l.state.Destinations[id].Cursor < r.Sequence {
				s.Destinations[id]++
			}
		}
	}
	return s
}
