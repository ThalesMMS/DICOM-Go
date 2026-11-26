// Package dcmtkoracle runs DCMTK command-line tools as opt-in test oracles.
//
// Oracles run only when DICOMGO_DCMTK_INTEGRATION=1; default test runs stay
// offline. A tool is looked up in the directory DICOMGO_DCMTK_BIN names,
// then on PATH, and a missing tool skips the test explicitly. Every run logs
// the tool's version, the command line and the tolerance of the comparison
// the test makes with its output, so a failure names what was compared and
// how strictly. Lossy results are never compared byte for byte.
package dcmtkoracle

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	// EnvGate enables the oracles when set to 1.
	EnvGate = "DICOMGO_DCMTK_INTEGRATION"
	// EnvBin optionally names the directory of the DCMTK tools.
	EnvBin = "DICOMGO_DCMTK_BIN"
	// Reference is the DCMTK revision the oracles were written against
	// (the local ../dcmtk checkout): a development revision after the 3.7.0
	// release, whose tools report "v3.7.0+ DEV".
	Reference = "DCMTK 3.7.0+DEV (8d68392d)"
)

// Tolerances the tests state for their comparisons.
const (
	// Exact compares decoded samples or values bit for bit.
	Exact = "exact"
	// Accepted only requires the tool to accept the input without errors.
	Accepted = "tool accepts the input (exit status 0, no errors)"
	// Protocol requires the exchange to complete with the stated outcome.
	Protocol = "protocol outcome"
)

// Enabled reports whether the DCMTK oracles are enabled.
func Enabled() bool { return os.Getenv(EnvGate) == "1" }

// Require skips the test unless the oracles are enabled.
func Require(t testing.TB) {
	t.Helper()
	if !Enabled() {
		t.Skipf("opt-in: %s=1 with the DCMTK tools on PATH or in %s", EnvGate, EnvBin)
	}
}

// Tool returns the path of a DCMTK tool, skipping the test when the
// oracles are disabled or the tool is not installed. The first lookup of a
// tool logs its version.
func Tool(t testing.TB, name string) string {
	t.Helper()
	Require(t)
	path, err := lookPath(name)
	if err != nil {
		t.Skipf("DCMTK %s not found in %s or PATH: %v", name, EnvBin, err)
	}
	t.Logf("oracle %s: %s at %s (written against %s)", name, Version(path), path, Reference)
	return path
}

// Lookup returns the path of a tool when the oracles are enabled and the
// tool is installed, for tests that fall back to another oracle.
func Lookup(name string) (string, bool) {
	if !Enabled() {
		return "", false
	}
	path, err := lookPath(name)
	return path, err == nil
}

func lookPath(name string) (string, error) {
	if dir := os.Getenv(EnvBin); dir != "" {
		candidate := filepath.Join(dir, name)
		if runtime.GOOS == "windows" && filepath.Ext(candidate) == "" {
			candidate += ".exe"
		}
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return exec.LookPath(name)
}

var versions sync.Map // tool path -> version line

// Version returns the first line of the tool's --version output, such as
// "$dcmtk: dcmdjpeg v3.7.0 2025-12-15 $".
func Version(path string) string {
	if v, ok := versions.Load(path); ok {
		return v.(string)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.WaitDelay = time.Second
	out, _ := cmd.CombinedOutput()
	version := "unknown version"
	for _, line := range strings.Split(string(out), "\n") {
		// Deprecated tools such as dcm2pnm print a warning banner first.
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "$dcmtk:") {
			version = line
			break
		}
	}
	versions.Store(path, version)
	return version
}

// Command returns the command running tool (a path from Tool) with args,
// after logging it with the tolerance of the comparison made with its
// result. Use it for long-running peers such as storescp.
func Command(ctx context.Context, t testing.TB, tolerance, tool string, args ...string) *exec.Cmd {
	t.Helper()
	t.Logf("oracle run: %s | tolerance: %s", commandLine(tool, args), tolerance)
	return exec.CommandContext(ctx, tool, args...)
}

// QRSCPProcessOptions selects single-process dcmqrscp when the executable
// advertises a choice. Builds with only single-process mode omit the switch.
func QRSCPProcessOptions(ctx context.Context, t testing.TB, tool string) []string {
	t.Helper()
	help, err := Command(ctx, t, Protocol, tool, "--help").CombinedOutput()
	if err != nil {
		t.Fatalf("dcmqrscp --help: %v\n%s", err, help)
	}
	if bytes.Contains(help, []byte("--single-process")) {
		return []string{"--single-process"}
	}
	if !bytes.Contains(bytes.ToLower(help), []byte("supports only single process mode")) {
		t.Fatal("dcmqrscp did not advertise a single-process mode")
	}
	return nil
}

// Run runs tool (a path from Tool) with args and returns its combined
// output, failing the test when it exits with an error.
func Run(t testing.TB, tolerance, tool string, args ...string) []byte {
	t.Helper()
	out, err := Command(context.Background(), t, tolerance, tool, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", commandLine(tool, args), err, out)
	}
	return out
}

func commandLine(tool string, args []string) string {
	var b bytes.Buffer
	b.WriteString(filepath.Base(tool))
	for _, arg := range args {
		if strings.ContainsAny(arg, " \t") {
			arg = fmt.Sprintf("%q", arg)
		}
		b.WriteByte(' ')
		b.WriteString(arg)
	}
	return b.String()
}
