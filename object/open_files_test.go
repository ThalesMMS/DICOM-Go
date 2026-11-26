package object

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/internal/parallel"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestOpenFilesContextOrderedMaterializedAndDeferred(t *testing.T) {
	paths := make([]string, 12)
	for index := range paths {
		elements := dicomtest.DatasetWithPixelData()
		for i := range elements {
			if elements[i].Tag() == core.TagPixelData {
				elements[i] = dicomtest.NewOBElement(core.TagPixelData, bytes.Repeat([]byte{byte(index)}, 64))
			}
		}
		encoded, err := dicomtest.Part10File(transfer.ExplicitVRLittleEndian, elements...)
		if err != nil {
			t.Fatal(err)
		}
		paths[index] = filepath.Join(t.TempDir(), fmt.Sprintf("%02d.dcm", index))
		if err := os.WriteFile(paths[index], encoded, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, deferred := range []bool{false, true} {
		files, err := OpenFilesContext(context.Background(), paths, ReadFileOptions{DeferPixelData: deferred, MaxTotalBytes: 1 << 20}, 4)
		if err != nil {
			t.Fatal(err)
		}
		for index, file := range files {
			var got bytes.Buffer
			if _, err := file.Dataset.CopyValueTo(core.TagPixelData, &got); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Bytes(), bytes.Repeat([]byte{byte(index)}, 64)) {
				t.Fatalf("input %d deferred=%v changed pixels/order", index, deferred)
			}
			if deferred != (file.source != nil) {
				t.Fatalf("input %d source ownership differs from defer option", index)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
}

type batchSource struct{ closed atomic.Bool }

func (s *batchSource) Close() error { s.closed.Store(true); return nil }

func TestOpenFilesContextJoinsAndClosesOnCancellation(t *testing.T) {
	previous := parallel.SetBudget(1)
	defer parallel.SetBudget(previous)
	parallel.ResetPeak()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan *batchSource, 2)
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		files, err := openFilesContext(ctx, []string{"0", "1", "2", "3"}, ReadFileOptions{}, 8, func(string, ReadFileOptions) (*File, error) {
			source := &batchSource{}
			started <- source
			<-release
			return &File{source: source}, nil
		})
		if files != nil {
			done <- errors.New("canceled batch published files")
			return
		}
		done <- err
	}()
	first, second := <-started, <-started
	cancel()
	select {
	case <-done:
		t.Fatal("batch returned while an opened source was still in flight")
	default:
	}
	close(release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled batch: %v", err)
	}
	if !first.closed.Load() || !second.closed.Load() || parallel.InUse() != 0 || parallel.Peak() != 1 {
		t.Fatal("batch did not close sources or respect/release the shared helper budget")
	}
}

func TestOpenFilesContextFirstErrorAndPartialSourceCleanup(t *testing.T) {
	previous := parallel.SetBudget(1)
	defer parallel.SetBudget(previous)
	low, high := errors.New("first source-order error"), errors.New("earlier completion")
	highFinished := make(chan struct{})
	sources := [2]*batchSource{{}, {}}
	files, err := openFilesContext(context.Background(), []string{"0", "1", "2"}, ReadFileOptions{}, 2, func(path string, _ ReadFileOptions) (*File, error) {
		switch path {
		case "0":
			<-highFinished
			return &File{source: sources[0]}, low
		case "1":
			close(highFinished)
			return &File{source: sources[1]}, high
		default:
			t.Error("batch opened an input beyond an already known error")
			return nil, nil
		}
	})
	if files != nil || !errors.Is(err, low) || !sources[0].closed.Load() || !sources[1].closed.Load() {
		t.Fatalf("batch published partial files, reordered the error or leaked a partial source: %v", err)
	}
}

func TestOpenFilesContextAdmission(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if files, err := OpenFilesContext(ctx, []string{"missing"}, ReadFileOptions{}, 2); files != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled batch = %v, %v", files, err)
	}
	if _, err := OpenFilesContext(nil, nil, ReadFileOptions{FrameSink: FrameSinkFunc(func(Frame) error { return nil })}, 2); !errors.Is(err, ErrSharedFileSink) {
		t.Fatalf("shared streaming sink was accepted: %v", err)
	}
	if files, err := OpenFilesContext(nil, nil, ReadFileOptions{}, 2); len(files) != 0 || err != nil {
		t.Fatalf("empty batch = %v, %v", files, err)
	}
}
