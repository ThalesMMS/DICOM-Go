package dcmtkoracle

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"
)

const versionHelperEnv = "DICOMGO_DCMTK_VERSION_TEST_HELPER"

func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		switch os.Getenv(versionHelperEnv) {
		case "hang":
			time.Sleep(time.Minute)
			os.Exit(0)
		case "version":
			fmt.Fprintln(os.Stderr, "Warning: deprecated tool")
			fmt.Println("$dcmtk: dcmfake v3.7.0 2025-12-15 $")
			os.Exit(0)
		}
	}
	if len(os.Args) == 2 && os.Args[1] == "--help" {
		switch os.Getenv(versionHelperEnv) {
		case "select-process":
			fmt.Println("  -s  --single-process       single process mode")
			os.Exit(0)
		case "single-process-only":
			fmt.Println("This version of dcmqrscp supports only single process mode.")
			os.Exit(0)
		}
	}
	os.Exit(m.Run())
}

func TestQRSCPProcessOptionsReadsThePeerHelp(t *testing.T) {
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		mode string
		want []string
	}{
		{"select-process", []string{"--single-process"}},
		{"single-process-only", nil},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			t.Setenv(versionHelperEnv, tc.mode)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if got := QRSCPProcessOptions(ctx, t, path); !slices.Equal(got, tc.want) {
				t.Fatalf("single-process options = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestVersionHandlesOutputAndTimeout(t *testing.T) {
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		mode, want string
	}{
		{mode: "version", want: "$dcmtk: dcmfake v3.7.0 2025-12-15 $"},
		{mode: "hang", want: "unknown version"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			t.Setenv(versionHelperEnv, tc.mode)
			versions.Delete(path)
			t.Cleanup(func() { versions.Delete(path) })
			start := time.Now()
			if got := Version(path); got != tc.want {
				t.Fatalf("Version = %q, want %q", got, tc.want)
			}
			if elapsed := time.Since(start); elapsed > 10*time.Second {
				t.Fatalf("version probe took %v, want at most 10s", elapsed)
			}
		})
	}
}

func TestToolSkipsWhenDisabledOrMissing(t *testing.T) {
	t.Setenv(EnvGate, "")
	var skipped bool
	t.Run("disabled", func(t *testing.T) {
		defer func() { skipped = t.Skipped() }()
		Tool(t, "dcmdump")
		t.Error("Tool returned with the oracles disabled")
	})
	if !skipped {
		t.Fatal("a disabled oracle did not skip")
	}
	if _, ok := Lookup("dcmdump"); ok {
		t.Fatal("Lookup found a tool with the oracles disabled")
	}

	t.Setenv(EnvGate, "1")
	t.Setenv(EnvBin, t.TempDir())
	t.Run("missing", func(t *testing.T) {
		defer func() { skipped = t.Skipped() }()
		Tool(t, "dicomgo-no-such-dcmtk-tool")
		t.Error("Tool returned a missing tool")
	})
	if !skipped {
		t.Fatal("a missing tool did not skip")
	}
}

func TestLookupPrefersTheConfiguredDirectory(t *testing.T) {
	dir := t.TempDir()
	name := "dcmfake"
	file := filepath.Join(dir, name)
	if runtime.GOOS == "windows" {
		file += ".exe"
	}
	if err := os.WriteFile(file, nil, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvGate, "1")
	t.Setenv(EnvBin, dir)
	if path, ok := Lookup(name); !ok || path != file {
		t.Fatalf("Lookup = %q, %v; want %q", path, ok, file)
	}
}

func TestCommandLineQuotesArgumentsWithSpaces(t *testing.T) {
	if got := commandLine("/opt/dcmtk/bin/dcmdjpeg", []string{"+w6", "in file.dcm", "out.dcm"}); got != `dcmdjpeg +w6 "in file.dcm" out.dcm` {
		t.Fatalf("commandLine = %s", got)
	}
}
