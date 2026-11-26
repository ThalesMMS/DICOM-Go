// Package hl7cli implements bounded command wrappers over hl7 and net/mllp.
package hl7cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/ThalesMMS/dicom-go/internal/privatefs"
)

const defaultBytes = 4 << 20

// Run executes one HL7 tool. Exit codes are 0 success/help, 2 usage/input,
// and 1 transport, acknowledgment, conversion or output refusal.
func Run(ctx context.Context, command string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	switch command {
	case "hl7snd":
		return send(ctx, args, stdin, stdout, stderr)
	case "hl7rcv":
		return receive(ctx, args, stdout, stderr)
	case "hl72xml", "xml2hl7":
		return convert(ctx, command, args, stdin, stdout, stderr)
	default:
		fmt.Fprintln(stderr, "unknown HL7 command")
		return 2
	}
}

func parseFlags(f *flag.FlagSet, args []string) (int, bool) {
	original := f.Output()
	var diagnostics bytes.Buffer
	f.SetOutput(&diagnostics)
	err := f.Parse(args)
	f.SetOutput(original)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = original.Write(diagnostics.Bytes())
			return 0, false
		}
		fmt.Fprintln(original, "invalid command arguments")
		return 2, false
	}
	return 0, true
}
func validBytes(n int) bool                    { return n > 0 && n <= 64<<20 }
func validTimeout(d time.Duration) bool        { return d > 0 && d <= 5*time.Minute }
func refusal(w io.Writer, category string) int { fmt.Fprintln(w, category); return 1 }
func usage(w io.Writer, text string) int       { fmt.Fprintln(w, text); return 2 }

func readInput(path string, stdin io.Reader, max int) ([]byte, error) {
	var r io.Reader = stdin
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return nil, errors.New("input could not be opened")
		}
		defer f.Close()
		r = f
	}
	data, err := io.ReadAll(io.LimitReader(r, int64(max)+1))
	if err != nil {
		return nil, errors.New("input could not be read")
	}
	if len(data) > max {
		return nil, errors.New("input exceeds byte limit")
	}
	return data, nil
}

// createOutput publishes a complete, file-synced temporary inode with Link.
// Link is atomic and cannot replace an existing destination, including symlinks.
func createOutput(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".hl7-write-*")
	if err != nil {
		return errors.New("output staging failed")
	}
	name := f.Name()
	defer os.Remove(name)
	closed := false
	defer func() {
		if !closed {
			_ = f.Close()
		}
	}()
	if err = privatefs.Secure(name); err != nil {
		return errors.New("output permissions failed")
	}
	if _, err = f.Write(data); err != nil {
		return errors.New("output write failed")
	}
	if err = f.Sync(); err != nil {
		return errors.New("output sync failed")
	}
	err = f.Close()
	closed = true
	if err != nil {
		return errors.New("output close failed")
	}
	if err = os.Link(name, path); err != nil {
		return errors.New("output publication refused (destination exists or linking unavailable)")
	}
	return nil
}

func writeOutput(path string, data []byte, stdout io.Writer) error {
	if path == "-" {
		n, err := stdout.Write(data)
		if err != nil {
			return err
		}
		if n != len(data) {
			return io.ErrShortWrite
		}
		return nil
	}
	return createOutput(path, data)
}
