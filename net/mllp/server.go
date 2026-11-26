package mllp

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/ThalesMMS/dicom-go/hl7"
)

// Request is one received message.
type Request struct {
	Message    *hl7.Message
	RemoteAddr net.Addr
	// TLS is the connection state under TLS, nil otherwise.
	TLS *tls.ConnectionState
}

// Handler processes one message and returns the response to send, usually
// an acknowledgment from hl7.ACKFor or hl7.NewACK; nil sends AA.
type Handler interface {
	ServeHL7(context.Context, *Request) *hl7.Message
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(context.Context, *Request) *hl7.Message

// ServeHL7 calls f.
func (f HandlerFunc) ServeHL7(ctx context.Context, r *Request) *hl7.Message { return f(ctx, r) }

// ServerOptions configure Serve. Zero values take the defaults.
type ServerOptions struct {
	// Release selects MLLP1 (default) or explicit framed-ACK MLLP2.
	Release Release
	// TransportACKTimeout bounds MLLP2 confirmations; default 30s. Sending
	// a confirmation is additionally bounded by WriteTimeout and context.
	TransportACKTimeout time.Duration
	// Exactly one of Handler and OriginalACKHandler is required.
	Handler Handler
	// OriginalACKHandler is an opt-in MLLP1 handler for encoded ACKs,
	// including a response committed by the application before a retry.
	OriginalACKHandler OriginalACKHandler
	// MaxFrameBytes bounds a received payload; default 1 MiB.
	MaxFrameBytes int
	// ParseOptions bound the parse; MaxBytes follows MaxFrameBytes.
	ParseOptions hl7.ParseOptions
	// IdleTimeout bounds the wait for the next frame; default 5 minutes.
	IdleTimeout time.Duration
	// ReadTimeout bounds receiving one frame once it started; default 30s.
	ReadTimeout time.Duration
	// WriteTimeout bounds sending a response; default 30s.
	WriteTimeout time.Duration
	// MaxConnections bounds concurrent connections; default 64. Further
	// connections are closed at once.
	MaxConnections int
	// TLSConfig, when set, serves MLLP over TLS.
	TLSConfig *tls.Config
	// OnError receives connection errors: framing, timeouts, unparseable
	// messages. They name conditions and sizes, never content.
	OnError func(remote net.Addr, err error)
}

func (o ServerOptions) withDefaults() ServerOptions {
	if o.TransportACKTimeout <= 0 {
		o.TransportACKTimeout = 30 * time.Second
	}
	if o.MaxFrameBytes <= 0 {
		o.MaxFrameBytes = DefaultMaxFrameBytes
	}
	if o.ParseOptions.MaxBytes <= 0 || o.ParseOptions.MaxBytes > o.MaxFrameBytes {
		o.ParseOptions.MaxBytes = o.MaxFrameBytes
	}
	if o.IdleTimeout <= 0 {
		o.IdleTimeout = 5 * time.Minute
	}
	if o.ReadTimeout <= 0 {
		o.ReadTimeout = 30 * time.Second
	}
	if o.WriteTimeout <= 0 {
		o.WriteTimeout = 30 * time.Second
	}
	if o.MaxConnections <= 0 {
		o.MaxConnections = 64
	}
	return o
}

// Serve accepts connections on listener until ctx ends, then closes the
// listener and the open connections and waits for their handlers.
func Serve(ctx context.Context, listener net.Listener, options ServerOptions) error {
	if !options.Release.valid() {
		return ErrUnsupportedRelease
	}
	if (options.Handler == nil) == (options.OriginalACKHandler == nil) {
		return errors.New("mllp: exactly one server handler is required")
	}
	if options.OriginalACKHandler != nil && options.Release != MLLP1 {
		return ErrUnsupportedRelease
	}
	options = options.withDefaults()
	if options.TLSConfig != nil {
		listener = tls.NewListener(listener, options.TLSConfig)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	slots := make(chan struct{}, options.MaxConnections)
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
	}()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var temporary interface{ Temporary() bool }
			if errors.As(err, &temporary) && temporary.Temporary() {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			return err
		}
		select {
		case slots <- struct{}{}:
		default:
			report(options, conn.RemoteAddr(), errors.New("mllp: connection limit reached"))
			_ = conn.Close()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			serveConn(ctx, conn, options)
		}()
	}
}

func report(options ServerOptions, remote net.Addr, err error) {
	if options.OnError != nil && err != nil {
		options.OnError(remote, err)
	}
}

func serveConn(ctx context.Context, conn net.Conn, options ServerOptions) {
	defer conn.Close()
	stop := closeOnCancel(ctx, conn)
	defer stop()
	var state *tls.ConnectionState
	if tlsConn, ok := conn.(*tls.Conn); ok {
		if err := tlsConn.SetDeadline(boundedDeadline(ctx, options.ReadTimeout, time.Time{})); err != nil {
			report(options, conn.RemoteAddr(), err)
			return
		}
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			report(options, conn.RemoteAddr(), fmt.Errorf("mllp: TLS handshake: %w", err))
			return
		}
		cs := tlsConn.ConnectionState()
		state = &cs
	}
	reader := NewReader(conn, options.MaxFrameBytes)
	// The idle timeout runs until a frame starts, the read timeout from
	// its start block to its end.
	reader.onStart = func() error { return conn.SetReadDeadline(boundedDeadline(ctx, options.ReadTimeout, time.Time{})) }
	for {
		if ctx.Err() != nil {
			return
		}
		if err := conn.SetReadDeadline(boundedDeadline(ctx, options.IdleTimeout, time.Time{})); err != nil {
			report(options, conn.RemoteAddr(), err)
			return
		}
		payload, err := reader.ReadFrame()
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, io.EOF) {
				if options.Release == MLLP2 && (errors.Is(err, ErrFraming) || errors.Is(err, ErrFrameTooLarge)) {
					// A refusal never attempts stream resynchronization or retry.
					_ = serverConfirmation(ctx, conn, options, false)
				}
				report(options, conn.RemoteAddr(), err)
			}
			return
		}
		if ctx.Err() != nil {
			return
		}
		if options.Release == MLLP2 {
			if err := serverConfirmation(ctx, conn, options, true); err != nil {
				if ctx.Err() == nil {
					report(options, conn.RemoteAddr(), transportPhaseError("request transport confirmation", err))
				}
				return
			}
		}
		response := respond(ctx, payload, conn.RemoteAddr(), state, options)
		if response == nil || ctx.Err() != nil {
			return
		}
		if options.Release == MLLP2 && len(response) > options.MaxFrameBytes {
			report(options, conn.RemoteAddr(), ErrFrameTooLarge)
			return
		}
		if err := conn.SetWriteDeadline(boundedDeadline(ctx, options.WriteTimeout, time.Time{})); err != nil {
			report(options, conn.RemoteAddr(), err)
			return
		}
		if err := WriteFrame(conn, response); err != nil {
			if ctx.Err() == nil {
				report(options, conn.RemoteAddr(), fmt.Errorf("mllp: write response: %w", err))
			}
			return
		}
		if options.Release == MLLP2 {
			if ctx.Err() != nil {
				return
			}
			if err := conn.SetReadDeadline(boundedDeadline(ctx, options.TransportACKTimeout, time.Time{})); err != nil {
				report(options, conn.RemoteAddr(), err)
				return
			}
			if err := reader.readConfirmation(); err != nil {
				if ctx.Err() == nil {
					report(options, conn.RemoteAddr(), transportPhaseError("response transport confirmation", err))
				}
				return
			}
		}
	}
}

func serverConfirmation(ctx context.Context, conn net.Conn, options ServerOptions, accepted bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ceiling := boundedDeadline(ctx, options.WriteTimeout, time.Time{})
	if err := conn.SetWriteDeadline(boundedDeadline(ctx, options.TransportACKTimeout, ceiling)); err != nil {
		return err
	}
	return writeConfirmation(conn, accepted)
}

// respond parses a payload, runs the handler and encodes the response; nil
// closes the connection (no acknowledgment can be addressed).
func respond(ctx context.Context, payload []byte, remote net.Addr, state *tls.ConnectionState, options ServerOptions) []byte {
	message, err := hl7.ParseWithOptions(payload, options.ParseOptions)
	if err != nil {
		report(options, remote, err)
		header, headerErr := parseHeaderOnly(payload, options.ParseOptions)
		if headerErr != nil {
			return nil
		}
		code := hl7.ErrorCodeDataType
		if errors.Is(err, hl7.ErrLimit) {
			code = hl7.ErrorCodeApplicationInternalError
		}
		return marshalServerResponse(header, hl7.NewACK(header, hl7.AckReject, hl7.AckOptions{ErrorCode: code, Text: "Message could not be parsed"}), remote, options)
	}
	request := &Request{Message: message, RemoteAddr: remote, TLS: state}
	if options.OriginalACKHandler != nil {
		return respondOriginalACK(ctx, request, payload, options)
	}
	response := callHandler(ctx, options.Handler, request, options)
	if response == nil {
		response = hl7.NewACK(message, hl7.AckAccept, hl7.AckOptions{})
	}
	return marshalServerResponse(message, response, remote, options)
}

func callHandler(ctx context.Context, handler Handler, request *Request, options ServerOptions) (response *hl7.Message) {
	defer func() {
		if recover() != nil {
			report(options, request.RemoteAddr, errors.New("mllp: handler panicked"))
			response = hl7.NewACK(request.Message, hl7.AckReject, hl7.AckOptions{ErrorCode: hl7.ErrorCodeApplicationInternalError, Text: "Application internal error"})
		}
	}()
	return handler.ServeHL7(ctx, request)
}

func marshalServerResponse(original, response *hl7.Message, remote net.Addr, options ServerOptions) []byte {
	marshal := func(message *hl7.Message) ([]byte, error) {
		if options.Release == MLLP2 || options.OriginalACKHandler != nil {
			return message.MarshalWithLimit(options.MaxFrameBytes)
		}
		return message.Marshal()
	}
	encoded, err := marshal(response)
	if errors.Is(err, hl7.ErrLimit) {
		report(options, remote, ErrFrameTooLarge)
		return nil
	}
	if err == nil && len(encoded) > 0 && bytes.IndexAny(encoded, string([]byte{StartBlock, EndBlock})) < 0 {
		return encoded
	}
	fallback, err := marshal(hl7.NewACK(original, hl7.AckError, hl7.AckOptions{ErrorCode: hl7.ErrorCodeApplicationInternalError, Text: "Application internal error"}))
	if errors.Is(err, hl7.ErrLimit) {
		report(options, remote, ErrFrameTooLarge)
		return nil
	}
	if err != nil {
		return nil
	}
	return fallback
}

// parseHeaderOnly reads only the MSH line, to address a rejection of a
// message whose body does not parse.
func parseHeaderOnly(payload []byte, options hl7.ParseOptions) (*hl7.Message, error) {
	line := payload
	if end := bytes.IndexAny(line, "\r\n"); end >= 0 {
		line = line[:end]
	}
	return hl7.ParseWithOptions(line, options)
}
