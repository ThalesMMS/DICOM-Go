package mllp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/ThalesMMS/dicom-go/hl7"
	"github.com/ThalesMMS/dicom-go/internal/sourceport"
)

// ErrResponseMismatch reports a response that acknowledges another message.
var ErrResponseMismatch = errors.New("mllp: response does not acknowledge the message sent")

// ClientOptions configure a Client. Zero values take the defaults.
type ClientOptions struct {
	// Release selects MLLP1 (default) or explicit framed-ACK MLLP2.
	Release Release
	// TransportACKTimeout bounds each MLLP2 confirmation phase, additionally
	// capped by ResponseTimeout and context; default 30s. Ignored in MLLP1.
	TransportACKTimeout time.Duration
	// MaxFrameBytes bounds a received payload; default 1 MiB.
	MaxFrameBytes int
	// ParseOptions bound the response parse.
	ParseOptions hl7.ParseOptions
	// ResponseTimeout bounds one send and its response; default 30s.
	ResponseTimeout time.Duration
	// TLSConfig, when set, connects over TLS.
	TLSConfig *tls.Config
}

// Client sends messages on one connection, one at a time, each answered
// by one response.
type Client struct {
	conn    net.Conn
	reader  *Reader
	options ClientOptions

	gate   chan struct{}
	broken error
}

// Dial connects to address.
func Dial(ctx context.Context, address string, options ClientOptions) (*Client, error) {
	if !options.Release.valid() {
		return nil, ErrUnsupportedRelease
	}
	conn, err := sourceport.DialContext(&net.Dialer{})(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	if options.TLSConfig != nil {
		tlsConn := tls.Client(conn, options.TLSConfig)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("mllp: TLS handshake: %w", err)
		}
		conn = tlsConn
	}
	return NewClient(conn, options), nil
}

// NewClient owns an established connection. Invalid releases produce an
// unusable client; the caller still closes it with Close.
func NewClient(conn net.Conn, options ClientOptions) *Client {
	if options.MaxFrameBytes <= 0 {
		options.MaxFrameBytes = DefaultMaxFrameBytes
	}
	if options.ParseOptions.MaxBytes <= 0 || options.ParseOptions.MaxBytes > options.MaxFrameBytes {
		options.ParseOptions.MaxBytes = options.MaxFrameBytes
	}
	if options.ResponseTimeout <= 0 {
		options.ResponseTimeout = 30 * time.Second
	}
	if options.TransportACKTimeout <= 0 {
		options.TransportACKTimeout = 30 * time.Second
	}
	client := &Client{conn: conn, reader: NewReader(conn, options.MaxFrameBytes), options: options, gate: make(chan struct{}, 1)}
	if !options.Release.valid() {
		client.broken = ErrUnsupportedRelease
	}
	return client
}

// Send sends message and returns the parsed response. A response with an
// MSA whose MSA-2 is not the message's control ID is ErrResponseMismatch.
// After a transport or framing error the client is unusable.
func (c *Client) Send(ctx context.Context, message *hl7.Message) (*hl7.Message, error) {
	select {
	case c.gate <- struct{}{}:
		defer func() { <-c.gate }()
	case <-ctx.Done():
		return nil, c.localFailure(ctx, ctx.Err())
	}
	if err := ctx.Err(); err != nil {
		return nil, c.localFailure(ctx, err)
	}
	if c.broken != nil {
		return nil, c.broken
	}
	var payload []byte
	var err error
	if c.options.Release == MLLP2 {
		payload, err = message.MarshalWithLimit(c.options.MaxFrameBytes)
		if errors.Is(err, hl7.ErrLimit) {
			err = errors.Join(ErrFrameTooLarge, err)
		}
	} else {
		payload, err = message.Marshal()
	}
	if err != nil {
		return nil, c.localFailure(ctx, err)
	}
	if c.options.Release == MLLP2 {
		if len(payload) > c.options.MaxFrameBytes {
			return nil, exchangeError(ctx, PhaseWriteRequest, RequestNotSent, ErrFrameTooLarge)
		}
		if err := validateFramePayload(payload); err != nil {
			return nil, exchangeError(ctx, PhaseWriteRequest, RequestNotSent, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, c.localFailure(ctx, err)
	}
	deadline := boundedDeadline(ctx, c.options.ResponseTimeout, time.Time{})
	if err := c.conn.SetDeadline(deadline); err != nil {
		return nil, c.phaseFailure(ctx, PhaseWriteRequest, RequestNotSent, err)
	}
	stop := closeOnCancel(ctx, c.conn)
	defer stop()
	written, err := writeFrame(c.conn, payload)
	if err != nil {
		state := RequestUnknown
		if written == 0 && !errors.Is(err, errInvalidWriteCount) {
			state = RequestNotSent
		}
		return nil, c.phaseFailure(ctx, PhaseWriteRequest, state, err)
	}
	state := RequestUnknown
	if c.options.Release == MLLP2 {
		if err := c.confirmationDeadline(ctx, deadline); err != nil {
			return nil, c.phaseFailure(ctx, PhaseRequestConfirmation, state, err)
		}
		if err := c.reader.readConfirmation(); err != nil {
			if errors.Is(err, ErrTransportNAK) {
				state = RequestRejected
			}
			return nil, c.phaseFailure(ctx, PhaseRequestConfirmation, state, err)
		}
		state = RequestAcknowledged
		if err := ctx.Err(); err != nil {
			return nil, c.phaseFailure(ctx, PhaseReadResponse, state, err)
		}
		if err := c.conn.SetDeadline(deadline); err != nil {
			return nil, c.phaseFailure(ctx, PhaseReadResponse, state, err)
		}
	}
	responsePayload, err := c.reader.ReadFrame()
	if err != nil {
		return nil, c.phaseFailure(ctx, PhaseReadResponse, state, err)
	}
	if c.options.Release == MLLP2 {
		if err := c.confirmationDeadline(ctx, deadline); err != nil {
			return nil, c.phaseFailure(ctx, PhaseResponseConfirmation, state, err)
		}
		if err := writeConfirmation(c.conn, true); err != nil {
			return nil, c.phaseFailure(ctx, PhaseResponseConfirmation, state, err)
		}
	}
	response, err := hl7.ParseWithOptions(responsePayload, c.options.ParseOptions)
	if err != nil {
		if c.options.Release == MLLP2 {
			return nil, exchangeError(ctx, PhaseParseResponse, state, err)
		}
		return nil, fmt.Errorf("mllp: response: %w", err)
	}
	if msa, ok := response.Segment("MSA"); ok && msa.Field(2).String() != message.Header().ControlID {
		if c.options.Release == MLLP2 {
			return response, exchangeError(ctx, PhaseCorrelateResponse, state, ErrResponseMismatch)
		}
		return response, ErrResponseMismatch
	}
	return response, nil
}

func (c *Client) localFailure(ctx context.Context, err error) error {
	if c.options.Release == MLLP2 {
		return exchangeError(ctx, PhaseWriteRequest, RequestNotSent, err)
	}
	return err
}

func (c *Client) confirmationDeadline(ctx context.Context, ceiling time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.conn.SetDeadline(boundedDeadline(ctx, c.options.TransportACKTimeout, ceiling))
}

func (c *Client) phaseFailure(ctx context.Context, phase ExchangePhase, state RequestTransportState, err error) error {
	if c.options.Release == MLLP2 {
		return c.fail(ctx, exchangeError(ctx, phase, state, err))
	}
	return c.fail(ctx, transportPhaseError(string(phase), err))
}

func (c *Client) fail(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		err = errors.Join(ctxErr, err)
	}
	c.broken = err
	_ = c.conn.Close()
	return err
}

// Close closes the connection.
func (c *Client) Close() error {
	return c.conn.Close()
}
