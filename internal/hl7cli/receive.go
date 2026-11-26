package hl7cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ThalesMMS/dicom-go/hl7"
	"github.com/ThalesMMS/dicom-go/internal/recordstore"
	"github.com/ThalesMMS/dicom-go/net/mllp"
)

func receive(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	f := flag.NewFlagSet("hl7rcv", flag.ContinueOnError)
	f.SetOutput(stderr)
	address := f.String("listen", "127.0.0.1:2575", "listen host:port (loopback by default)")
	duration := f.Duration("duration", 5*time.Minute, "total server lifetime (positive, at most 24h)")
	timeout := f.Duration("timeout", 30*time.Second, "idle, read and write timeout (positive, at most 5m)")
	release := f.Int("mllp-release", 1, "wire release (1 default; 2 framed transport ACK/NAK)")
	transportTimeout := f.Duration("transport-ack-timeout", 30*time.Second, "MLLP2 transport confirmation timeout (positive, at most 5m)")
	max := f.Int("max-bytes", defaultBytes, "frame byte limit (1..67108864)")
	connections := f.Int("max-connections", 16, "concurrent connection limit (1..128)")
	messages := f.Int("max-messages", 1000, "handler admission limit; excess attempt stops server (1..100000)")
	output := f.String("output-dir", "", "existing directory for private atomic message files")
	replayDir := f.String("replay-dir", "", "private durable inbox and exact ACK replay directory (MLLP1 only)")
	disk := f.Int64("max-output-bytes", 64<<20, "cumulative persisted byte quota (1..1073741824)")
	f.Usage = func() { fmt.Fprintln(f.Output(), "Usage: hl7rcv [options]"); f.PrintDefaults() }
	code, ok := parseFlags(f, args)
	if !ok {
		return code
	}
	if f.NArg() != 0 || !validBytes(*max) || !validTimeout(*timeout) || !validTimeout(*transportTimeout) || (*release != 1 && *release != 2) || *duration <= 0 || *duration > 24*time.Hour || *connections < 1 || *connections > 128 || *messages < 1 || *messages > 100000 || *disk < 1 || *disk > 1<<30 {
		return usage(stderr, "hl7rcv: invalid finite limits or positional argument")
	}
	if *replayDir != "" && (*output != "" || *release != 1 || *max > defaultBytes) {
		return usage(stderr, "hl7rcv: replay requires MLLP1, at most 4194304 frame bytes and no separate output directory")
	}
	host, port, err := net.SplitHostPort(*address)
	if err != nil || host == "" || port == "" {
		return usage(stderr, "hl7rcv: invalid listen address")
	}
	if *output != "" {
		info, err := os.Stat(*output)
		if err != nil || !info.IsDir() {
			return refusal(stderr, "hl7rcv: output directory is unavailable")
		}
	}
	var replayStore *recordstore.Store
	if *replayDir != "" {
		replayStore, err = openReplayStore(ctx, *replayDir, *max, *disk)
		if err != nil {
			return refusal(stderr, "hl7rcv: replay directory is unavailable or exceeds its limits")
		}
		defer replayStore.Close()
	}
	listener, err := net.Listen("tcp", *address)
	if err != nil {
		return refusal(stderr, "hl7rcv: listener could not be opened")
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(ctx, *duration)
	defer cancel()
	var mu sync.Mutex
	var admitted, stored, replayed, failed int
	var used int64
	admit := func() bool {
		mu.Lock()
		defer mu.Unlock()
		if admitted >= *messages {
			cancel()
			return false
		}
		admitted++
		return true
	}
	handler := mllp.HandlerFunc(func(ctx context.Context, request *mllp.Request) *hl7.Message {
		if !admit() {
			mu.Lock()
			failed++
			mu.Unlock()
			return hl7.NewACK(request.Message, hl7.AckReject, hl7.AckOptions{Text: "Receiver admission limit reached"})
		}
		if ctx.Err() != nil || request.Message.Header().ControlID == "" {
			mu.Lock()
			failed++
			mu.Unlock()
			return hl7.NewACK(request.Message, hl7.AckReject, hl7.AckOptions{Text: "Message could not be admitted"})
		}
		if *output != "" {
			data, err := request.Message.Marshal()
			if err != nil || len(data) > *max {
				mu.Lock()
				failed++
				mu.Unlock()
				return hl7.NewACK(request.Message, hl7.AckError, hl7.AckOptions{Text: "Message could not be persisted"})
			}
			mu.Lock()
			fits := int64(len(data)) <= *disk-used
			if fits {
				used += int64(len(data))
			}
			mu.Unlock()
			if !fits {
				mu.Lock()
				failed++
				mu.Unlock()
				return hl7.NewACK(request.Message, hl7.AckError, hl7.AckOptions{Text: "Receiver byte quota reached"})
			}
			var id [16]byte
			_, err = rand.Read(id[:])
			if err == nil {
				err = createOutput(filepath.Join(*output, "message-"+hex.EncodeToString(id[:])+".hl7"), data)
			}
			mu.Lock()
			if err != nil {
				used -= int64(len(data))
				failed++
			} else {
				stored++
			}
			mu.Unlock()
			if err != nil {
				return hl7.NewACK(request.Message, hl7.AckError, hl7.AckOptions{Text: "Message could not be persisted"})
			}
		}
		return hl7.NewACK(request.Message, hl7.AckAccept, hl7.AckOptions{})
	})
	options := mllp.ServerOptions{
		Handler: handler, Release: mllp.Release(*release - 1), TransportACKTimeout: *transportTimeout,
		MaxFrameBytes: *max, MaxConnections: *connections,
		IdleTimeout: *timeout, ReadTimeout: *timeout, WriteTimeout: *timeout,
		OnError: func(_ net.Addr, _ error) { mu.Lock(); failed++; mu.Unlock() },
	}
	if replayStore != nil {
		options.Handler = nil
		options.OriginalACKHandler = func(ctx context.Context, request *mllp.Request, er7 []byte) ([]byte, error) {
			if !admit() {
				return nil, context.Canceled
			}
			ack, created, err := receiveWithReplay(ctx, replayStore, request.Message, er7, *max)
			if err != nil {
				return nil, err
			}
			mu.Lock()
			if created {
				stored++
			} else {
				replayed++
			}
			mu.Unlock()
			return ack, nil
		}
	}
	if err := writeOutput("-", []byte(fmt.Sprintf("listening %s\n", listener.Addr())), stdout); err != nil {
		cancel()
		return refusal(stderr, "hl7rcv: listener summary could not be written")
	}
	err = mllp.Serve(ctx, listener, options)
	mu.Lock()
	summary := fmt.Sprintf("admitted %d; persisted %d; failures %d\n", admitted, stored, failed)
	if replayStore != nil {
		summary = fmt.Sprintf("admitted %d; persisted %d; replayed %d; failures %d\n", admitted, stored, replayed, failed)
	}
	hadFailure := failed > 0
	mu.Unlock()
	if err := writeOutput("-", []byte(summary), stdout); err != nil {
		return refusal(stderr, "hl7rcv: final summary could not be written")
	}
	if err != nil || hadFailure {
		return refusal(stderr, "hl7rcv: transport or message refusal occurred")
	}
	return 0
}
