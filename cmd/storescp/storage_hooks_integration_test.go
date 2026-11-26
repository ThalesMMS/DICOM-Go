package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"github.com/ThalesMMS/dicom-go/object"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

var storageHookExtraHelper = func(_ string, _ []string) bool { return false }

type storageHelperRecord struct {
	Args, Environment []string
	Complete          bool
	Directory         string
}

func storageHelperArguments() []string {
	for i, s := range os.Args {
		if s == "--storescp-hook-helper" {
			return os.Args[i+1:]
		}
	}
	return nil
}
func storageHelperArgv(t *testing.T, mode string, args ...string) []string {
	t.Helper()
	bin, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	return append([]string{bin, "-test.run=^TestStorageHookProcessHelper$", "--", "--storescp-hook-helper", mode}, args...)
}
func TestStorageHookProcessHelper(t *testing.T) {
	args := storageHelperArguments()
	if len(args) == 0 {
		return
	}
	mode := args[0]
	args = args[1:]
	switch mode {
	case "record", "reception", "study":
		if len(args) < 1 {
			os.Exit(80)
		}
		record := storageHelperRecord{Args: append([]string(nil), args[1:]...), Environment: os.Environ()}
		record.Directory, _ = os.Getwd()
		if mode == "reception" {
			if len(args) < 2 {
				os.Exit(81)
			}
			f, err := object.OpenFile(args[1])
			if err != nil {
				os.Exit(82)
			}
			record.Complete = f.Dataset != nil
			f.Close()
		}
		file, err := os.OpenFile(args[0], os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			os.Exit(83)
		}
		if err = json.NewEncoder(file).Encode(record); err != nil {
			os.Exit(84)
		}
		if err = file.Close(); err != nil {
			os.Exit(85)
		}
	case "nonzero":
		os.Exit(9)
	case "output":
		_, _ = io.WriteString(os.Stdout, strings.Repeat("X", 5000))
		_, _ = io.WriteString(os.Stderr, strings.Repeat("Y", 5000))
	case "wait":
		if len(args) > 0 {
			_ = os.WriteFile(args[0], []byte("READY"), 0600)
		}
		time.Sleep(time.Minute)
	default:
		if !storageHookExtraHelper(mode, args) {
			os.Exit(86)
		}
	}
	os.Exit(0)
}
func readStorageHelperRecords(t *testing.T, path string) []storageHelperRecord {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []storageHelperRecord
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		var r storageHelperRecord
		if err := json.Unmarshal(scan.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err = scan.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
func waitStorageFile(ctx context.Context, path string) error {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
func TestStorageHookRealArgvEnvironmentAndFailureRecovery(t *testing.T) {
	if err := storageHookPlatformReady(); err != nil {
		t.Skip("execution unsupported on host")
	}
	t.Setenv("STORESCP_SYNTHETIC_SECRET", "must-not-inherit")
	root := t.TempDir()
	log := filepath.Join(root, "log.jsonl")
	args := []string{"space and \"quotes\"", "; $(touch SHOULD_NOT_EXIST)", "$plain", "", "line\nfeed"}
	argv := storageHelperArgv(t, "record", append([]string{log}, args...)...)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := runStorageHook(ctx, argv, root); err != nil {
		t.Fatal(err)
	}
	records := readStorageHelperRecords(t, log)
	if len(records) != 1 || !reflect.DeepEqual(records[0].Args, args) || !sameStorageDirectory(records[0].Directory, root) {
		t.Fatalf("literal argv/directory %+v", records)
	}
	for _, env := range records[0].Environment {
		if strings.HasPrefix(env, "STORESCP_SYNTHETIC_SECRET=") || strings.HasPrefix(env, "HOME=") || strings.HasPrefix(env, "PATH=") {
			t.Fatalf("inherited environment %s", env)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "SHOULD_NOT_EXIST")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("shell expansion")
	}
	if err := runStorageHook(ctx, storageHelperArgv(t, "nonzero"), root); !errors.Is(err, errStorageHook) {
		t.Fatalf("exit %v", err)
	}
	if err := runStorageHook(ctx, storageHelperArgv(t, "output"), root); !errors.Is(err, errStorageHookOutput) {
		t.Fatalf("output %v", err)
	}
	if err := runStorageHook(ctx, storageHelperArgv(t, "record", log, "after-failures"), root); err != nil {
		t.Fatal(err)
	}
	if records = readStorageHelperRecords(t, log); len(records) != 2 || records[1].Args[0] != "after-failures" {
		t.Fatal(records)
	}
}
func TestStorageHookRealTimeoutAndCancelledQueue(t *testing.T) {
	if err := storageHookPlatformReady(); err != nil {
		t.Skip("execution unsupported on host")
	}
	root := t.TempDir()
	ready := filepath.Join(root, "ready")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err := runStorageHook(ctx, storageHelperArgv(t, "wait", ready), root); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout %v", err)
	}
	if time.Since(started) > 3*time.Second {
		t.Fatal("hook cleanup exceeded finite bound")
	}
	// A controlled worker exercises cancellation between events: the second event
	// was admitted before shutdown, but must never invoke the runner.
	path, _ := filepath.Abs(os.Args[0])
	entered := make(chan struct{})
	count := 0
	r := testStorageRuntime(t, []string{"-exec-on-reception", storageHookJSON([]string{path, "${filename}"})}, storageRuntimeDependencies{manualPoll: true, execute: func(ctx context.Context, _ []string, _ string) error {
		count++
		if count == 1 {
			close(entered)
			<-ctx.Done()
			return ctx.Err()
		}
		t.Error("queued hook invoked after cancellation")
		return nil
	}})
	p := prepareStorageTest(t, r, "AE")
	p.stored(filepath.Join(r.root, "first.dcm"))
	<-entered
	p = prepareStorageTest(t, r, "AE")
	p.stored(filepath.Join(r.root, "second.dcm"))
	stopped, stop := context.WithCancel(context.Background())
	stop()
	if err := r.Close(stopped); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if count != 1 || r.Metrics().Cancelled != 2 {
		t.Fatalf("queue cancellation %d/%+v", count, r.Metrics())
	}
}

func sameStorageDirectory(a, b string) bool {
	x, err := os.Stat(a)
	if err != nil {
		return false
	}
	y, err := os.Stat(b)
	return err == nil && os.SameFile(x, y)
}
