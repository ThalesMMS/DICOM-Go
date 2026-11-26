package rle

import (
	"bytes"
	"context"
	"sync"
	"testing"
)

func TestEncoderKernelMatchesFrozenByteOutputAtEverySupportedStride(t *testing.T) {
	for _, samples := range []uint16{1, 3} {
		for bits := uint16(8); int(samples)*int(bits/8) <= 15; bits += 8 {
			for _, grid := range [][2]uint16{{1, 1}, {2, 129}, {3, 65535}, {257, 509}} {
				for _, pattern := range []string{"noise", "runs", "short-runs"} {
					metadata := encoderMetadata(grid[0], grid[1], samples, bits, "MONOCHROME2")
					if samples == 3 {
						metadata.PhotometricInterpretation = "RGB"
					}
					frame := kernelPixels(int(grid[0])*int(grid[1])*int(samples)*int(bits/8), pattern)
					want, err := baselineNewEncoder().EncodeFrame(context.Background(), frame, metadata)
					if err != nil {
						t.Fatal(err)
					}
					got, err := NewEncoder().EncodeFrame(context.Background(), frame, metadata)
					if err != nil || !bytes.Equal(got.Data, want.Data) {
						t.Fatalf("%dx%d samples%d bits%d %s: byte-output changed: %v", grid[0], grid[1], samples, bits, pattern, err)
					}
					decoded, _, err := decodeFrame(context.Background(), got.Data, int(grid[0]), int(grid[1]), int(samples), int(bits), false)
					if err != nil || !bytes.Equal(decoded, frame) {
						t.Fatalf("samples%d bits%d %s: decoded pixels changed: %v", samples, bits, pattern, err)
					}
				}
			}
		}
	}
}

func TestEncoderWorkspaceBoundAndReturnedFrameOwnership(t *testing.T) {
	encoder := NewEncoder()
	metadata := encoderMetadata(2, 65535, 3, 40, "RGB")
	frame := kernelPixels(2*65535*15, "noise")
	first, err := encoder.EncodeFrame(context.Background(), frame, metadata)
	if err != nil {
		t.Fatal(err)
	}
	want := append([]byte(nil), first.Data...)
	var workers sync.WaitGroup
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if _, err := encoder.EncodeFrame(context.Background(), frame, metadata); err != nil {
				t.Error(err)
			}
		}()
	}
	workers.Wait()
	if !bytes.Equal(first.Data, want) {
		t.Fatal("later encodes mutated caller-owned output")
	}
	retained := 0
	for len(encoder.workspaces) != 0 {
		workspace := <-encoder.workspaces
		if cap(workspace) > encoderWorkspaceLimit {
			t.Fatal("oversized workspace retained")
		}
		retained += cap(workspace)
	}
	if retained > 2*encoderWorkspaceLimit {
		t.Fatalf("retained %d workspace bytes", retained)
	}
}

func TestEncoderZeroAndNilReceiverRemainUsable(t *testing.T) {
	metadata := encoderMetadata(2, 129, 1, 16, "MONOCHROME2")
	frame := kernelPixels(2*129*2, "short-runs")
	want, err := baselineNewEncoder().EncodeFrame(nil, frame, metadata)
	if err != nil {
		t.Fatal(err)
	}
	for _, encoder := range []*Encoder{{}, nil} {
		got, err := encoder.EncodeFrame(nil, frame, metadata)
		if err != nil || !bytes.Equal(got.Data, want.Data) {
			t.Fatalf("zero/nil receiver: %v", err)
		}
	}
}

func kernelPixels(length int, pattern string) []byte {
	frame := make([]byte, length)
	state := uint32(0x9e3779b9)
	for i := range frame {
		switch pattern {
		case "runs":
			frame[i] = byte(i / 384)
		case "short-runs":
			frame[i] = byte(i / 2)
		default:
			state = state*1664525 + 1013904223
			frame[i] = byte(state >> 24)
		}
	}
	return frame
}
