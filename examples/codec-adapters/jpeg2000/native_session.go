package jpeg2000

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/ThalesMMS/dicom-go/pixeldata"
	j2k "github.com/mrjoshuak/go-jpeg2000"
)

// NativeBackend selects one independently qualified native decoder.
type NativeBackend string

const (
	NativeOpenJPEG        NativeBackend = "openjpeg"
	NativeOpenJPH         NativeBackend = "openjph"
	nativeProtocolVersion               = 1
	nativeMaximumBytes                  = 64 << 20
)

var (
	ErrNativeHelperUnavailable = errors.New("jpeg2000adapter: native helper unavailable")
	ErrNativeHelperProtocol    = errors.New("jpeg2000adapter: native helper protocol error")
	ErrNativeHelperLimit       = errors.New("jpeg2000adapter: native helper resource limit")
)

// NativeSessionConfig configures one caller-owned, serialized native worker.
// No helper is discovered or registered implicitly. Compile/package the helper
// separately and Close the session when its registry/consumer is released.
type NativeSessionConfig struct {
	Executable      string
	Backend         NativeBackend
	Timeout         time.Duration // per call, including waiting for this session; default 30 s
	MaxEncodedBytes uint32        // default/hard ceiling 64 MiB
	MaxDecodedBytes uint32        // default/hard ceiling 64 MiB
	// MaxNativeBytes requires the helper to enforce an OS memory ceiling
	// before accepting any request. Zero preserves the standalone legacy
	// protocol invocation.
	MaxNativeBytes uint64
	MaxRestarts    int // default 2; negative disables restarts after process/protocol failures
	// Admit optionally charges encoded + decoded IPC storage for the active call.
	// The owner separately reserves the process memory allowed by MaxNativeBytes.
	Admit func(context.Context, int64) (func(), error)
}

// NativeSession implements ContextDecoder and owns one persistent pipe worker.
// Cancellation kills and collects that worker before returning, discards its
// reply and permits a clean restart on the next call. No automatic CLI fallback
// occurs; malformed/unsupported inputs are terminal for that call.
type NativeSession struct {
	cfg       NativeSessionConfig
	token     chan struct{}
	closed    chan struct{}
	mu        sync.Mutex
	process   *nativeProcess
	failures  int
	nextID    uint64
	starts    uint64
	frames    uint64
	closeOnce sync.Once
}

// NativeSessionStats is a snapshot for lifecycle/resource accounting. PID is
// zero after collection; the owner may sample that live child's memory.
type NativeSessionStats struct {
	PID            int
	Starts, Frames uint64
	Failures       int
	Closed         bool
}

func (s *NativeSession) Stats() NativeSessionStats {
	if s == nil {
		return NativeSessionStats{Closed: true}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r := NativeSessionStats{Starts: s.starts, Frames: s.frames, Failures: s.failures}
	if s.process != nil {
		r.PID = s.process.cmd.Process.Pid
	}
	select {
	case <-s.closed:
		r.Closed = true
	default:
	}
	return r
}

type nativeProcess struct {
	cmd     *exec.Cmd
	input   io.WriteCloser
	output  io.ReadCloser
	stopped chan struct{}
	waitErr error
	once    sync.Once
}

// OpenNativeSession verifies the helper's protocol, engine and runtime version.
// Its default build remains pure Go; only the explicitly supplied external
// executable links OpenJPEG/OpenJPH. Nothing is downloaded or compiled here.
func OpenNativeSession(ctx context.Context, cfg NativeSessionConfig) (*NativeSession, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if cfg.Executable == "" || (cfg.Backend != NativeOpenJPEG && cfg.Backend != NativeOpenJPH) {
		return nil, fmt.Errorf("%w: executable/backend required", ErrNativeHelperUnavailable)
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.MaxEncodedBytes == 0 {
		cfg.MaxEncodedBytes = nativeMaximumBytes
	}
	if cfg.MaxDecodedBytes == 0 {
		cfg.MaxDecodedBytes = nativeMaximumBytes
	}
	if cfg.MaxEncodedBytes > nativeMaximumBytes || cfg.MaxDecodedBytes > nativeMaximumBytes {
		return nil, ErrNativeHelperLimit
	}
	if cfg.MaxRestarts == 0 {
		cfg.MaxRestarts = 2
	} else if cfg.MaxRestarts < 0 {
		cfg.MaxRestarts = 0
	}
	s := &NativeSession{cfg: cfg, token: make(chan struct{}, 1), closed: make(chan struct{})}
	s.token <- struct{}{}
	run, cancel := boundDecoderContext(ctx, cfg.Timeout)
	defer cancel()
	if e := s.ensureProcess(run); e != nil {
		_ = s.Close()
		if mapped := mapBoundDecoderError(ctx, run); mapped != nil {
			return nil, mapped
		}
		return nil, e
	}
	return s, nil
}

func (s *NativeSession) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		close(s.closed)
		s.mu.Lock()
		p := s.process
		s.process = nil
		s.mu.Unlock()
		if p != nil {
			p.stop()
		}
	})
	return nil
}
func (s *NativeSession) DecodeFrame(b []byte, m pixeldata.Metadata) ([]byte, error) {
	return s.DecodeFrameContext(context.Background(), b, m)
}
func (s *NativeSession) DecodeFrameContext(ctx context.Context, b []byte, m pixeldata.Metadata) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	if s == nil {
		return nil, ErrNativeHelperUnavailable
	}
	if e := validateMetadata(m); e != nil {
		return nil, e
	}
	if m.PixelRepresentation != 0 {
		return nil, fmt.Errorf("%w: persistent helper currently qualifies unsigned samples", ErrUnsupportedMetadata)
	}
	n := uint64(m.Rows) * uint64(m.Columns) * uint64(m.SamplesPerPixel) * uint64(m.BitsAllocated/8)
	if n == 0 || n > uint64(s.cfg.MaxDecodedBytes) || len(b) == 0 || uint64(len(b)) > uint64(s.cfg.MaxEncodedBytes) {
		return nil, ErrNativeHelperLimit
	}
	if (s.cfg.Backend == NativeOpenJPH) != isHTJ2KCodestream(b) {
		return nil, fmt.Errorf("%w: codestream/backend mismatch", ErrUnsupportedMetadata)
	}
	header, e := j2k.DecodeMetadata(bytes.NewReader(b))
	if e != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformedCodestream, e)
	}
	if e = validateCodestreamMetadata(m, header); e != nil {
		return nil, e
	}
	run, cancel := boundDecoderContext(ctx, s.cfg.Timeout)
	defer cancel()
	select {
	case <-run.Done():
		return nil, mapBoundDecoderError(ctx, run)
	case <-s.closed:
		return nil, ErrNativeHelperUnavailable
	case <-s.token:
	}
	defer func() { s.token <- struct{}{} }()
	if e = run.Err(); e != nil {
		return nil, mapBoundDecoderError(ctx, run)
	}
	select {
	case <-s.closed:
		return nil, ErrNativeHelperUnavailable
	default:
	}
	release := func() {}
	if s.cfg.Admit != nil {
		release, e = s.cfg.Admit(run, int64(len(b))+int64(n))
		if e != nil {
			if mapped := mapBoundDecoderError(ctx, run); mapped != nil {
				return nil, mapped
			}
			return nil, e
		}
		if release == nil {
			release = func() {}
		}
	}
	defer release()
	if e = s.ensureProcess(run); e != nil {
		if mapped := mapBoundDecoderError(ctx, run); mapped != nil {
			return nil, mapped
		}
		return nil, e
	}
	s.mu.Lock()
	p := s.process
	s.nextID++
	id := s.nextID
	s.mu.Unlock()
	if p == nil {
		return nil, ErrNativeHelperUnavailable
	}
	reply, e := s.exchange(run, p, nativeRequest{opcode: 2, id: id, rows: uint32(m.Rows), cols: uint32(m.Columns), samples: uint32(m.SamplesPerPixel), bits: uint32(m.BitsAllocated), stored: uint32(m.BitsStored), payload: b}, uint32(n))
	if e != nil {
		if mapped := mapBoundDecoderError(ctx, run); mapped != nil {
			return nil, mapped
		}
		return nil, e
	}
	if reply.id != id || reply.rows != uint32(m.Rows) || reply.cols != uint32(m.Columns) || reply.samples != uint32(m.SamplesPerPixel) {
		s.invalidate(p, true)
		return nil, ErrNativeHelperProtocol
	}
	switch reply.status {
	case 0:
		if uint64(len(reply.payload)) != n {
			s.invalidate(p, true)
			return nil, pixeldata.ErrPixelDataSizeMismatch
		}
	case 1:
		return nil, ErrMalformedCodestream
	case 2:
		return nil, ErrUnsupportedMetadata
	case 3:
		return nil, ErrImageSizeMismatch
	case 4:
		return nil, ErrNativeHelperLimit
	default:
		s.invalidate(p, true)
		return nil, ErrNativeHelperProtocol
	}
	if e = ctx.Err(); e != nil {
		return nil, e
	}
	select {
	case <-s.closed:
		return nil, ErrNativeHelperUnavailable
	default:
	}
	s.mu.Lock()
	s.frames++
	s.mu.Unlock()
	return reply.payload, nil
}

func (s *NativeSession) ensureProcess(ctx context.Context) error {
	s.mu.Lock()
	if s.process != nil {
		s.mu.Unlock()
		return nil
	}
	if s.failures > s.cfg.MaxRestarts {
		s.mu.Unlock()
		return ErrNativeHelperUnavailable
	}
	s.mu.Unlock()
	if e := ctx.Err(); e != nil {
		return e
	}
	select {
	case <-s.closed:
		return ErrNativeHelperUnavailable
	default:
	}
	args := []string{string(s.cfg.Backend)}
	if s.cfg.MaxNativeBytes > 0 {
		args = append(args, strconv.FormatUint(s.cfg.MaxNativeBytes, 10))
	}
	cmd := exec.Command(s.cfg.Executable, args...)
	configureNativeHelperCommand(cmd)
	in, e := cmd.StdinPipe()
	if e != nil {
		return e
	}
	out, e := cmd.StdoutPipe()
	if e != nil {
		in.Close()
		return e
	}
	// Keep launch and publication together so Close cannot release the
	// worker's reservation while a replacement is still starting.
	s.mu.Lock()
	select {
	case <-s.closed:
		s.mu.Unlock()
		in.Close()
		out.Close()
		return ErrNativeHelperUnavailable
	default:
	}
	if e = cmd.Start(); e != nil {
		s.failures++
		s.mu.Unlock()
		in.Close()
		out.Close()
		return fmt.Errorf("%w: %w", ErrNativeHelperUnavailable, e)
	}
	p := &nativeProcess{cmd: cmd, input: in, output: out, stopped: make(chan struct{})}
	s.process = p
	s.starts++
	s.mu.Unlock()
	go func() { p.waitErr = cmd.Wait(); close(p.stopped) }()
	reply, e := s.exchange(ctx, p, nativeRequest{opcode: 1, id: 0}, 128)
	version := QualifiedOpenJPEGVersion
	engine := uint32(1)
	if s.cfg.Backend == NativeOpenJPH {
		version = QualifiedOpenJPHVersion
		engine = 2
	}
	if e != nil {
		return e
	}
	if reply.id != 0 || reply.status != 0 || reply.samples != engine || string(reply.payload) != version {
		s.invalidate(p, true)
		return fmt.Errorf("%w: engine/version handshake", ErrNativeHelperProtocol)
	}
	return nil
}
func (s *NativeSession) invalidate(p *nativeProcess, failure bool) {
	s.mu.Lock()
	if s.process == p {
		s.process = nil
		if failure {
			s.failures++
		}
	}
	s.mu.Unlock()
	p.stop()
}
func (p *nativeProcess) stop() {
	p.once.Do(func() { _ = killNativeHelperProcess(p.cmd); _ = p.input.Close(); _ = p.output.Close(); <-p.stopped })
}

// Both writes and reads are supervised. A helper which never consumes stdin
// cannot trap cancellation in a blocked pipe write. The I/O goroutine is joined.
func (s *NativeSession) exchange(ctx context.Context, p *nativeProcess, req nativeRequest, limit uint32) (nativeReply, error) {
	type answer struct {
		reply nativeReply
		err   error
	}
	done := make(chan answer, 1)
	go func() {
		e := writeNativeRequest(p.input, req)
		var r nativeReply
		if e == nil {
			r, e = readNativeReply(p.output, limit)
		}
		done <- answer{r, e}
	}()
	select {
	case <-ctx.Done():
		s.invalidate(p, false)
		<-done
		return nativeReply{}, ctx.Err()
	case <-s.closed:
		s.invalidate(p, false)
		<-done
		return nativeReply{}, ErrNativeHelperUnavailable
	case a := <-done:
		if e := ctx.Err(); e != nil {
			s.invalidate(p, false)
			return nativeReply{}, e
		}
		if a.err != nil {
			s.invalidate(p, true)
			return nativeReply{}, fmt.Errorf("%w: %w", ErrNativeHelperProtocol, a.err)
		}
		return a.reply, nil
	}
}

type nativeRequest struct {
	opcode                            uint32
	id                                uint64
	rows, cols, samples, bits, stored uint32
	payload                           []byte
}
type nativeReply struct {
	id                          uint64
	status, rows, cols, samples uint32
	payload                     []byte
}

func writeNativeRequest(w io.Writer, r nativeRequest) error {
	var h [44]byte
	copy(h[:], "J2KH")
	binary.LittleEndian.PutUint32(h[4:], nativeProtocolVersion)
	binary.LittleEndian.PutUint32(h[8:], r.opcode)
	binary.LittleEndian.PutUint64(h[12:], r.id)
	for i, v := range []uint32{r.rows, r.cols, r.samples, r.bits, r.stored, uint32(len(r.payload))} {
		binary.LittleEndian.PutUint32(h[20+4*i:], v)
	}
	if e := nativeWriteAll(w, h[:]); e != nil {
		return e
	}
	return nativeWriteAll(w, r.payload)
}
func nativeWriteAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, e := w.Write(b)
		if e != nil {
			return e
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}
func readNativeReply(r io.Reader, limit uint32) (nativeReply, error) {
	var h [36]byte
	if _, e := io.ReadFull(r, h[:]); e != nil {
		return nativeReply{}, e
	}
	if string(h[:4]) != "J2KH" || binary.LittleEndian.Uint32(h[4:]) != nativeProtocolVersion {
		return nativeReply{}, ErrNativeHelperProtocol
	}
	reply := nativeReply{id: binary.LittleEndian.Uint64(h[8:]), status: binary.LittleEndian.Uint32(h[16:]), rows: binary.LittleEndian.Uint32(h[20:]), cols: binary.LittleEndian.Uint32(h[24:]), samples: binary.LittleEndian.Uint32(h[28:])}
	n := binary.LittleEndian.Uint32(h[32:])
	if n > limit || (reply.status != 0 && n != 0) {
		return nativeReply{}, ErrNativeHelperProtocol
	}
	reply.payload = make([]byte, n)
	_, e := io.ReadFull(r, reply.payload)
	return reply, e
}
