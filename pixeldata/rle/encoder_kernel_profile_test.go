package rle

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

type kernelStartContext struct {
	context.Context
	started chan struct{}
	checks  int
}

func (c *kernelStartContext) Err() error {
	c.checks++
	if c.checks == 3 { // both encoders: first bounded input work, after allocation
		close(c.started)
	}
	return c.Context.Err()
}

func TestEncoderKernelCancellationProfile(t *testing.T) {
	path := os.Getenv("DICOMGO_RLE_CANCEL_ARTIFACT")
	if path == "" {
		t.Skip("set DICOMGO_RLE_CANCEL_ARTIFACT for measured large-frame cancellation")
	}
	frame := kernelPixels(4096*4096*2, "noise")
	metadata := encoderMetadata(4096, 4096, 1, 16, "MONOCHROME2")
	type observation struct {
		Version   string `json:"version"`
		LatencyNS int64  `json:"cancel_to_return_ns"`
		Checks    int    `json:"context_checks"`
	}
	var rows []observation
	for i := 0; i < 24; i++ {
		for _, version := range []string{"baseline", "current"} {
			var encode func(context.Context, []byte, pixeldata.Metadata) (pixeldata.EncodedFrame, error)
			if version == "baseline" {
				encode = baselineNewEncoder().EncodeFrame
			} else {
				encode = NewEncoder().EncodeFrame
			}
			ctx, cancel := context.WithCancel(context.Background())
			started := &kernelStartContext{Context: ctx, started: make(chan struct{})}
			done := make(chan error, 1)
			go func() { _, err := encode(started, frame, metadata); done <- err }()
			select {
			case <-started.started:
			case <-time.After(5 * time.Second):
				cancel()
				t.Fatal("encoder did not enter input work")
			}
			time.Sleep(time.Millisecond)
			began := time.Now()
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation result: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("encoder did not honor cancellation")
			}
			rows = append(rows, observation{version, time.Since(began).Nanoseconds(), started.checks})
		}
	}
	data, err := json.MarshalIndent(map[string]any{"frame_bytes": len(frame), "sample_count_per_version": 24, "rows": rows, "scope": "callback wall after context cancel, after entering input work; host scheduler dependent, not a hard real-time guarantee"}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
}
