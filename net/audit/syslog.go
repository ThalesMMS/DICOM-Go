package audit

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/ThalesMMS/dicom-go/internal/sourceport"
)

// Syslog transport for audit messages: RFC 5424 messages over TLS (RFC
// 5425, octet-counted framing) or UDP (RFC 5426, one message per
// datagram). Messages wait in a bounded in-memory spool; a failed send
// keeps them for the next attempt and a full spool drops the oldest.

// ErrSyslogConfig reports an unusable SyslogLogger configuration.
var ErrSyslogConfig = errors.New("dicom: invalid syslog audit configuration")

// Syslog transports.
const (
	SyslogTLS = "tls"
	SyslogUDP = "udp"
	// SyslogTCP is octet-counted TCP without TLS, for trusted networks and
	// tests only.
	SyslogTCP = "tcp"
)

// Defaults of SyslogLogger.
const (
	DefaultSyslogSpoolMessages = 1000
	DefaultSyslogSpoolBytes    = 16 << 20
	DefaultSyslogRetry         = 5 * time.Second
	// MaxSyslogUDPMessage is the largest datagram sent over UDP; a longer
	// message is refused rather than truncated.
	MaxSyslogUDPMessage    = 65507
	syslogFacilityAuthPriv = 10
)

// SyslogLogger sends audit messages to an audit record repository.
type SyslogLogger struct {
	// Network is SyslogTLS, SyslogUDP or SyslogTCP; Address is host:port.
	Network string
	Address string
	// TLS configures SyslogTLS (server verification and client
	// certificate); it is required for SyslogTLS.
	TLS *tls.Config
	// AppName and Hostname fill the RFC 5424 header ("dicom-go" and the
	// OS host name by default).
	AppName  string
	Hostname string
	// MaxSpoolMessages and MaxSpoolBytes bound the spool (defaults
	// DefaultSyslogSpoolMessages and DefaultSyslogSpoolBytes).
	MaxSpoolMessages int
	MaxSpoolBytes    int
	// RetryInterval is the first wait of Run after a failed flush, doubled
	// up to one minute (DefaultSyslogRetry).
	RetryInterval time.Duration
	// Dial replaces the network dial (tests).
	Dial func(ctx context.Context, network, address string) (net.Conn, error)

	mu      sync.Mutex
	spool   [][]byte
	bytes   int
	dropped int
	conn    net.Conn
	wake    chan struct{}
	flushMu sync.Mutex
}

// SyslogStats reports the spool.
type SyslogStats struct {
	Spooled      int
	SpooledBytes int
	Dropped      int
}

// Log formats message as an RFC 5424 record and spools it.
func (l *SyslogLogger) Log(message Message) error {
	payload, err := message.Marshal()
	if err != nil {
		return err
	}
	severity := 5 // notice
	if message.Event.OutcomeIndicator != OutcomeSuccess {
		severity = 4 // warning
	}
	at, err := time.Parse(time.RFC3339Nano, message.Event.DateTime)
	if err != nil {
		at = time.Now()
	}
	return l.LogRaw(severity, at, payload)
}

// LogRaw spools an already encoded audit message with a syslog severity.
func (l *SyslogLogger) LogRaw(severity int, at time.Time, payload []byte) error {
	return l.logRaw(severity, at, payload, false)
}

// LogClinical refuses a full spool instead of evicting an accepted record.
// The caller can retry Finish after flushing; this is still an in-memory spool,
// not crash-safe delivery or a durable audit repository acknowledgement.
func (l *SyslogLogger) LogClinical(policy ClinicalPolicy, message Message) error {
	payload, err := message.MarshalClinical(policy)
	if err != nil {
		return err
	}
	at, _ := time.Parse(time.RFC3339Nano, message.Event.DateTime)
	severity := 5
	if message.Event.OutcomeIndicator != OutcomeSuccess {
		severity = 4
	}
	if err := l.logRaw(severity, at, payload, true); err != nil {
		return clinicalError("spool", "Logger")
	}
	return nil
}

func (l *SyslogLogger) logRaw(severity int, at time.Time, payload []byte, strict bool) error {
	if err := l.check(); err != nil {
		return err
	}
	record := l.record(severity, at, payload)
	if l.Network == SyslogUDP && len(record) > MaxSyslogUDPMessage {
		return fmt.Errorf("%w: %d-byte message exceeds a UDP datagram", ErrSyslogConfig, len(record))
	}
	maxMessages, maxBytes := l.limits()
	if len(record) > maxBytes {
		return fmt.Errorf("%w: %d-byte message exceeds MaxSpoolBytes", ErrSyslogConfig, len(record))
	}
	l.mu.Lock()
	if strict && (len(l.spool) >= maxMessages || len(record) > maxBytes-l.bytes) {
		l.mu.Unlock()
		return clinicalError("spool", "Logger")
	}
	l.spool = append(l.spool, record)
	l.bytes += len(record)
	for len(l.spool) > maxMessages || l.bytes > maxBytes {
		l.bytes -= len(l.spool[0])
		l.spool = l.spool[1:]
		l.dropped++
	}
	wake := l.wakeChannel()
	l.mu.Unlock()
	select {
	case wake <- struct{}{}:
	default:
	}
	return nil
}

func (l *SyslogLogger) check() error {
	switch l.Network {
	case SyslogTLS:
		if l.TLS == nil {
			return fmt.Errorf("%w: TLS needs a tls.Config", ErrSyslogConfig)
		}
	case SyslogUDP, SyslogTCP:
	default:
		return fmt.Errorf("%w: network %q", ErrSyslogConfig, l.Network)
	}
	if l.Address == "" {
		return fmt.Errorf("%w: no address", ErrSyslogConfig)
	}
	return nil
}

func (l *SyslogLogger) limits() (int, int) {
	messages, bytes := l.MaxSpoolMessages, l.MaxSpoolBytes
	if messages <= 0 {
		messages = DefaultSyslogSpoolMessages
	}
	if bytes <= 0 {
		bytes = DefaultSyslogSpoolBytes
	}
	return messages, bytes
}

func (l *SyslogLogger) wakeChannel() chan struct{} {
	if l.wake == nil {
		l.wake = make(chan struct{}, 1)
	}
	return l.wake
}

// record builds "<PRI>1 TIMESTAMP HOST APP PROCID MSGID - MSG" with the
// IHE message ID "IHE+RFC-3881" and a UTF-8 BOM before the XML.
func (l *SyslogLogger) record(severity int, at time.Time, payload []byte) []byte {
	host := l.Hostname
	if host == "" {
		host, _ = os.Hostname()
	}
	app := l.AppName
	if app == "" {
		app = "dicom-go"
	}
	header := fmt.Sprintf("<%d>1 %s %s %s %d IHE+RFC-3881 - \xef\xbb\xbf",
		syslogFacilityAuthPriv*8+severity, at.UTC().Format("2006-01-02T15:04:05.000000Z"),
		syslogField(host), syslogField(app), os.Getpid())
	return append([]byte(header), payload...)
}

// syslogField keeps PRINTUSASCII, at most 48 characters, "-" when empty.
func syslogField(value string) string {
	out := make([]byte, 0, len(value))
	for i := 0; i < len(value) && len(out) < 48; i++ {
		if c := value[i]; c >= 33 && c <= 126 {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return "-"
	}
	return string(out)
}

// Stats reports the spool size and the messages dropped so far.
func (l *SyslogLogger) Stats() SyslogStats {
	l.mu.Lock()
	defer l.mu.Unlock()
	return SyslogStats{Spooled: len(l.spool), SpooledBytes: l.bytes, Dropped: l.dropped}
}

// Flush sends the spooled messages in order. A failure closes the
// connection and keeps the unsent messages; the next Flush reconnects.
func (l *SyslogLogger) Flush(ctx context.Context) error {
	l.flushMu.Lock()
	defer l.flushMu.Unlock()
	if err := l.check(); err != nil {
		return err
	}
	for {
		l.mu.Lock()
		if len(l.spool) == 0 {
			l.mu.Unlock()
			return nil
		}
		record := l.spool[0]
		conn := l.conn
		l.mu.Unlock()
		if conn == nil {
			var err error
			if conn, err = l.dial(ctx); err != nil {
				return err
			}
			l.mu.Lock()
			l.conn = conn
			l.mu.Unlock()
		}
		if err := l.send(ctx, conn, record); err != nil {
			l.mu.Lock()
			if l.conn == conn {
				l.conn = nil
			}
			l.mu.Unlock()
			_ = conn.Close()
			return err
		}
		l.mu.Lock()
		if len(l.spool) > 0 && &l.spool[0][0] == &record[0] {
			l.bytes -= len(record)
			l.spool = l.spool[1:]
		}
		l.mu.Unlock()
	}
}

func (l *SyslogLogger) dial(ctx context.Context) (net.Conn, error) {
	network := "tcp"
	if l.Network == SyslogUDP {
		network = "udp"
	}
	dial := l.Dial
	if dial == nil {
		// A TCP or TLS connect whose source port the system could not use
		// is made again from a fresh port; UDP is dialed once.
		dial = sourceport.DialContext(&net.Dialer{})
	}
	conn, err := dial(ctx, network, l.Address)
	if err != nil {
		return nil, err
	}
	if l.Network != SyslogTLS {
		return conn, nil
	}
	config := l.TLS.Clone()
	if config.ServerName == "" {
		host, _, _ := net.SplitHostPort(l.Address)
		config.ServerName = host
	}
	client := tls.Client(conn, config)
	if err := client.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return client, nil
}

func (l *SyslogLogger) send(ctx context.Context, conn net.Conn, record []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetWriteDeadline(deadline)
	} else {
		_ = conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	}
	if l.Network == SyslogUDP {
		n, err := conn.Write(record)
		if err == nil && n != len(record) {
			return io.ErrShortWrite
		}
		return err
	}
	for _, part := range [][]byte{[]byte(strconv.Itoa(len(record)) + " "), record} {
		for len(part) > 0 {
			n, err := conn.Write(part)
			if n < 0 || n > len(part) {
				return io.ErrShortWrite
			}
			part = part[n:]
			if err != nil {
				return err
			}
			if n == 0 {
				return io.ErrShortWrite
			}
		}
	}
	return ctx.Err()
}

// Run flushes whenever messages are spooled, retrying a failed flush with
// a backoff from RetryInterval up to one minute, until ctx ends; it then
// closes the connection and returns ctx's error. Messages still spooled
// stay for a later Flush.
func (l *SyslogLogger) Run(ctx context.Context) error {
	l.mu.Lock()
	wake := l.wakeChannel()
	l.mu.Unlock()
	retry := l.RetryInterval
	if retry <= 0 {
		retry = DefaultSyslogRetry
	}
	wait := retry
	for {
		var timer <-chan time.Time
		if err := l.Flush(ctx); err != nil {
			timer = time.After(wait)
			wait = min(2*wait, time.Minute)
		} else {
			wait = retry
		}
		select {
		case <-ctx.Done():
			l.Close()
			return ctx.Err()
		case <-wake:
		case <-timer:
		}
	}
}

// Close closes the connection; spooled messages are kept.
func (l *SyslogLogger) Close() error {
	l.mu.Lock()
	conn := l.conn
	l.conn = nil
	l.mu.Unlock()
	if conn != nil {
		return conn.Close()
	}
	return nil
}
