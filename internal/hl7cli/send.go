package hl7cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/ThalesMMS/dicom-go/hl7"
	"github.com/ThalesMMS/dicom-go/net/mllp"
)

func send(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	f := flag.NewFlagSet("hl7snd", flag.ContinueOnError)
	f.SetOutput(stderr)
	address := f.String("address", "127.0.0.1:2575", "MLLP destination host:port")
	timeout := f.Duration("timeout", 30*time.Second, "dial and response timeout (positive, at most 5m)")
	release := f.Int("mllp-release", 1, "wire release (1 default; 2 framed transport ACK/NAK)")
	transportTimeout := f.Duration("transport-ack-timeout", 30*time.Second, "MLLP2 transport confirmation timeout (positive, at most 5m)")
	max := f.Int("max-bytes", defaultBytes, "input and ACK frame byte limit (1..67108864)")
	f.Usage = func() {
		fmt.Fprintln(f.Output(), "Usage: hl7snd [options] message.hl7 (or - for stdin)")
		f.PrintDefaults()
	}
	code, ok := parseFlags(f, args)
	if !ok {
		return code
	}
	if f.NArg() != 1 || !validBytes(*max) || !validTimeout(*timeout) || !validTimeout(*transportTimeout) || (*release != 1 && *release != 2) {
		return usage(stderr, "hl7snd: require one input and valid finite limits")
	}
	host, port, err := net.SplitHostPort(*address)
	if err != nil || host == "" || port == "" {
		return usage(stderr, "hl7snd: invalid address")
	}
	data, err := readInput(f.Arg(0), stdin, *max)
	if err != nil {
		return refusal(stderr, "hl7snd: input unavailable or over limit")
	}
	message, err := hl7.ParseWithOptions(data, hl7.ParseOptions{MaxBytes: *max})
	if err != nil {
		return refusal(stderr, "hl7snd: malformed or unsupported HL7 input")
	}
	if message.Header().ControlID == "" {
		return refusal(stderr, "hl7snd: MSH-10 control ID is required")
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	client, err := mllp.Dial(ctx, *address, mllp.ClientOptions{MaxFrameBytes: *max, ResponseTimeout: *timeout, Release: mllp.Release(*release - 1), TransportACKTimeout: *transportTimeout})
	if err != nil {
		return refusal(stderr, "hl7snd: connection failed")
	}
	defer client.Close()
	response, err := client.Send(ctx, message)
	if err != nil {
		if errors.Is(err, mllp.ErrTransportNAK) {
			return refusal(stderr, "hl7snd: transport NAK; no application acknowledgment and no retry")
		}
		if errors.Is(err, mllp.ErrTransportConfirmation) {
			return refusal(stderr, "hl7snd: malformed transport confirmation; request result unknown; no retry")
		}
		var exchange *mllp.ExchangeError
		if errors.As(err, &exchange) {
			switch exchange.RequestState {
			case mllp.RequestNotSent:
				return refusal(stderr, "hl7snd: transport failed before sending the request")
			case mllp.RequestAcknowledged:
				return refusal(stderr, "hl7snd: transport acknowledged request; application result unavailable or uncorrelated; no retry")
			default:
				return refusal(stderr, "hl7snd: request transport result unknown; no retry")
			}
		}
		return refusal(stderr, "hl7snd: transport, timeout, framing or acknowledgment correlation failure")
	}
	if len(response.All("MSA")) != 1 {
		return refusal(stderr, "hl7snd: response must contain exactly one acknowledgment")
	}
	ack, err := hl7.ParseAcknowledgment(response)
	if err != nil {
		return refusal(stderr, "hl7snd: response has no valid acknowledgment")
	}
	if ack.ControlID != message.Header().ControlID {
		return refusal(stderr, "hl7snd: acknowledgment correlation failure")
	}
	switch ack.Code {
	case hl7.AckAccept:
		if err := writeOutput("-", []byte("AA\n"), stdout); err != nil {
			return refusal(stderr, "hl7snd: acknowledgment summary could not be written")
		}
		return 0
	case hl7.AckError:
		return refusal(stderr, "hl7snd: AE acknowledgment")
	case hl7.AckReject:
		return refusal(stderr, "hl7snd: AR acknowledgment")
	default:
		return refusal(stderr, "hl7snd: enhanced acknowledgment mode is unsupported")
	}
}
