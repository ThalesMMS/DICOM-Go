package audit

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

var ErrSyslogRecord = errors.New("dicom: invalid syslog record")

// SyslogRecord owns Raw and Message; slices remain valid after Handle returns.
// ParseSyslogRecord copies input. Receiver transfers each newly allocated
// record to Handle and never reuses its backing storage.
type SyslogRecord struct {
	Priority                                           int
	Timestamp, Hostname, AppName, ProcessID, MessageID string
	StructuredData                                     string
	Raw, Message                                       []byte
}

func parseSyslogRecord(raw []byte) (SyslogRecord, error) {
	fail := func() (SyslogRecord, error) { return SyslogRecord{}, ErrSyslogRecord }
	r := SyslogRecord{Raw: raw}
	if len(raw) < 4 || raw[0] != '<' {
		return fail()
	}
	end := bytes.IndexByte(raw, '>')
	if end < 2 || end > 4 {
		return fail()
	}
	priority := raw[1:end]
	if len(priority) > 1 && priority[0] == '0' {
		return fail()
	}
	for _, digit := range priority {
		if digit < '0' || digit > '9' {
			return fail()
		}
	}
	p, err := strconv.Atoi(string(raw[1:end]))
	if err != nil || p < 0 || p > 191 {
		return fail()
	}
	r.Priority = p
	rest := raw[end+1:]
	fields := make([]string, 0, 6)
	for i := 0; i < 6; i++ {
		sep := bytes.IndexByte(rest, ' ')
		if sep < 1 {
			return fail()
		}
		field := rest[:sep]
		for _, c := range field {
			if c < 33 || c > 126 {
				return fail()
			}
		}
		fields = append(fields, string(field))
		rest = rest[sep+1:]
	}
	if fields[0] != "1" || len(fields[1]) > 64 || len(fields[2]) > 255 || len(fields[3]) > 48 || len(fields[4]) > 128 || len(fields[5]) > 32 {
		return fail()
	}
	if fields[1] != "-" && !validSyslogTimestamp(fields[1]) {
		return fail()
	}
	r.Timestamp, r.Hostname, r.AppName, r.ProcessID, r.MessageID = fields[1], fields[2], fields[3], fields[4], fields[5]
	if len(rest) == 0 {
		return fail()
	}
	offset, ok := structuredDataEnd(rest)
	if !ok {
		return fail()
	}
	if offset < len(rest) && rest[offset] != ' ' {
		return fail()
	}
	r.StructuredData = string(rest[:offset])
	if offset == len(rest) {
		rest = nil
	} else {
		rest = rest[offset+1:]
	}
	if bytes.HasPrefix(rest, []byte{0xef, 0xbb, 0xbf}) {
		rest = rest[3:]
		if !utf8.Valid(rest) {
			return fail()
		}
	}
	r.Message = rest
	return r, nil
}

// validSyslogTimestamp checks RFC5424's restricted RFC3339 grammar before
// calendar validation. time.Parse alone accepts comma fractions, more than six
// fractional digits, single-digit hours and out-of-range offset components.
func validSyslogTimestamp(value string) bool {
	if len(value) < 20 || len(value) > 32 {
		return false
	}
	for i := 0; i < 19; i++ {
		var separator byte
		switch i {
		case 4, 7:
			separator = '-'
		case 10:
			separator = 'T'
		case 13, 16:
			separator = ':'
		}
		if separator != 0 {
			if value[i] != separator {
				return false
			}
		} else if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	zone := 19
	if value[zone] == '.' {
		zone++
		start := zone
		for zone < len(value) && value[zone] >= '0' && value[zone] <= '9' {
			zone++
		}
		if zone-start < 1 || zone-start > 6 {
			return false
		}
	}
	offset := value[zone:]
	if offset != "Z" {
		if len(offset) != 6 || (offset[0] != '+' && offset[0] != '-') || offset[3] != ':' {
			return false
		}
		for _, i := range []int{1, 2, 4, 5} {
			if offset[i] < '0' || offset[i] > '9' {
				return false
			}
		}
		if (offset[1]-'0')*10+offset[2]-'0' > 23 || (offset[4]-'0')*10+offset[5]-'0' > 59 {
			return false
		}
	}
	_, err := time.Parse(time.RFC3339, value)
	return err == nil
}

// ParseSyslogRecord parses RFC5424 VERSION 1 with bounded input. It validates
// the header/framing, not DICOM XML specialization or clinical content.
func ParseSyslogRecord(raw []byte, maximum int) (SyslogRecord, error) {
	if maximum < 1 || len(raw) > maximum {
		return SyslogRecord{}, ErrSyslogRecord
	}
	return parseSyslogRecord(bytes.Clone(raw))
}

type ReceiverOptions struct {
	MaxRecordBytes, MaxConnections int
	ReadTimeout                    time.Duration
	// Handle owns record bytes. An error increments Failed and closes the
	// stream. No application-layer persistence ACK is sent by this receiver.
	Handle func(context.Context, SyslogRecord) error
}
type ReceiverStats struct{ Received, Failed, Rejected uint64 }
type Receiver struct {
	options                    ReceiverOptions
	received, failed, rejected atomic.Uint64
}

func NewReceiver(options ReceiverOptions) (*Receiver, error) {
	if options.MaxRecordBytes == 0 {
		options.MaxRecordBytes = 4 << 20
	}
	if options.MaxConnections == 0 {
		options.MaxConnections = 32
	}
	if options.ReadTimeout == 0 {
		options.ReadTimeout = 30 * time.Second
	}
	if options.MaxRecordBytes < 32768 || options.MaxRecordBytes > 64<<20 || options.MaxConnections < 1 || options.MaxConnections > 1024 || options.ReadTimeout < time.Millisecond || options.ReadTimeout > time.Hour || options.Handle == nil {
		return nil, spoolError("receiver_configuration", ErrSyslogConfig)
	}
	return &Receiver{options: options}, nil
}
func (r *Receiver) Stats() ReceiverStats {
	return ReceiverStats{r.received.Load(), r.failed.Load(), r.rejected.Load()}
}
func (r *Receiver) handle(ctx context.Context, raw []byte) error {
	record, err := parseSyslogRecord(raw)
	if err != nil {
		r.rejected.Add(1)
		return ErrSyslogRecord
	}
	if err = r.options.Handle(ctx, record); err != nil {
		r.failed.Add(1)
		return spoolError("receiver_handler", ErrDurableSpool)
	}
	r.received.Add(1)
	return nil
}

func readOctetRecord(reader *bufio.Reader, maximum int) ([]byte, error) {
	size := 0
	digits := 0
	for {
		c, err := reader.ReadByte()
		if err != nil {
			return nil, err
		}
		if c == ' ' {
			if digits == 0 || size == 0 {
				return nil, ErrSyslogRecord
			}
			break
		}
		if c < '0' || c > '9' || digits >= 10 || (digits == 0 && c == '0') {
			return nil, ErrSyslogRecord
		}
		digit := int(c - '0')
		if size > (maximum-digit)/10 {
			return nil, ErrSyslogRecord
		}
		size = size*10 + digit
		digits++
	}
	if size > maximum {
		return nil, ErrSyslogRecord
	}
	raw := make([]byte, size)
	if _, err := io.ReadFull(reader, raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// Serve owns listener until it returns and closes accepted sockets on cancel.
// TLS listeners are supplied by the caller; plain TCP uses RFC6587 octet counts.
func (r *Receiver) Serve(ctx context.Context, listener net.Listener) error {
	if ctx == nil || listener == nil {
		return spoolError("receiver_configuration", ErrSyslogConfig)
	}
	var mu sync.Mutex
	connections := map[net.Conn]bool{}
	var wg sync.WaitGroup
	slots := make(chan struct{}, r.options.MaxConnections)
	stop := context.AfterFunc(ctx, func() {
		listener.Close()
		mu.Lock()
		for conn := range connections {
			conn.Close()
		}
		mu.Unlock()
	})
	defer stop()
	defer listener.Close()
	var acceptErr error
	for {
		conn, err := listener.Accept()
		if err != nil {
			acceptErr = err
			break
		}
		select {
		case slots <- struct{}{}:
		default:
			r.rejected.Add(1)
			conn.Close()
			continue
		}
		mu.Lock()
		if ctx.Err() != nil {
			mu.Unlock()
			conn.Close()
			<-slots
			break
		}
		connections[conn] = true
		mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer conn.Close()
			defer func() { mu.Lock(); delete(connections, conn); mu.Unlock(); <-slots }()
			reader := bufio.NewReaderSize(conn, 4096)
			for {
				if ctx.Err() != nil {
					return
				}
				conn.SetReadDeadline(time.Now().Add(r.options.ReadTimeout))
				raw, err := readOctetRecord(reader, r.options.MaxRecordBytes)
				if err != nil {
					if !errors.Is(err, io.EOF) && ctx.Err() == nil {
						r.rejected.Add(1)
					}
					return
				}
				if err = r.handle(ctx, raw); err != nil {
					return
				}
			}
		}()
	}
	mu.Lock()
	for conn := range connections {
		conn.Close()
	}
	mu.Unlock()
	wg.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if acceptErr != nil {
		return spoolError("receiver_accept", ErrDurableSpool)
	}
	return nil
}

// ServeUDP receives one complete RFC5424 message per datagram. Oversized
// datagrams are refused. There is no acknowledgement of reception/persistence.
func (r *Receiver) ServeUDP(ctx context.Context, conn net.PacketConn) error {
	if ctx == nil || conn == nil {
		return spoolError("receiver_configuration", ErrSyslogConfig)
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	defer conn.Close()
	maximum := min(r.options.MaxRecordBytes, MaxSyslogUDPMessage)
	buffer := make([]byte, maximum+1)
	for {
		n, _, err := conn.ReadFrom(buffer)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return spoolError("receiver_read", ErrDurableSpool)
		}
		if n > maximum {
			r.rejected.Add(1)
			continue
		}
		_ = r.handle(ctx, bytes.Clone(buffer[:n]))
	}
}

// structuredDataEnd validates RFC5424 SD-NAME and quoted parameter syntax.
func structuredDataEnd(p []byte) (int, bool) {
	if len(p) == 0 {
		return 0, false
	}
	if p[0] == '-' {
		return 1, true
	}
	offset := 0
	ids := make(map[string]struct{})
	name := func() bool {
		start := offset
		for offset < len(p) {
			c := p[offset]
			if c == ' ' || c == ']' || c == '=' {
				break
			}
			if c < 33 || c > 126 || c == '"' {
				return false
			}
			offset++
		}
		return offset > start && offset-start <= 32
	}
	for offset < len(p) && p[offset] == '[' {
		offset++
		idStart := offset
		if !name() {
			return 0, false
		}
		id := string(p[idStart:offset])
		if _, duplicate := ids[id]; duplicate {
			return 0, false
		}
		ids[id] = struct{}{}
		for offset < len(p) && p[offset] == ' ' {
			offset++
			if !name() || offset >= len(p) || p[offset] != '=' {
				return 0, false
			}
			offset++
			if offset >= len(p) || p[offset] != '"' {
				return 0, false
			}
			offset++
			closed := false
			for offset < len(p) {
				c := p[offset]
				offset++
				if c == '"' {
					closed = true
					break
				}
				if c == '\\' {
					if offset >= len(p) {
						return 0, false
					}
					next := p[offset]
					if next == '"' || next == '\\' || next == ']' {
						offset++
					}
					// RFC5424 6.3.3: an unknown escape keeps the slash and
					// subsequent character unchanged. Controls are valid
					// UTF-8 and remain intact in Raw/StructuredData too.
				} else if c == ']' {
					return 0, false
				}
			}
			if !closed {
				return 0, false
			}
		}
		if offset >= len(p) || p[offset] != ']' {
			return 0, false
		}
		offset++
	}
	return offset, offset > 0 && utf8.Valid(p[:offset])
}
