package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/net/audit"

	"github.com/ThalesMMS/dicom-go/internal/privatefs"
)

func TestCollectorPrivatePersistenceAndAdmission(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "records")
	s, err := openSink(directory, 32768, 1)
	if err != nil {
		t.Fatal(err)
	}
	record := audit.SyslogRecord{Raw: []byte("controlled raw record")}
	if err = s.handle(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if err = s.handle(context.Background(), record); err == nil {
		t.Fatal("full collector accepted")
	}
	s, err = openSink(directory, 32768, 1)
	if err != nil || s.messages != 1 {
		t.Fatalf("restart: %v", err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 1 {
		t.Fatal("collector publication incomplete")
	}
	_, err = entries[0].Info()
	if err != nil || privatefs.Check(filepath.Join(directory, entries[0].Name())) != nil {
		t.Fatal("collector payload not private")
	}
}
func TestCollectorCLIRefusesConfigurationWithoutAddressDiagnostics(t *testing.T) {
	var output bytes.Buffer
	err := run(context.Background(), []string{"-directory", t.TempDir(), "-transport", "smtp", "-listen", "SYNTHETIC.example:123"}, &output)
	if err == nil || bytes.Contains(output.Bytes(), []byte("SYNTHETIC")) {
		t.Fatal("invalid configuration accepted or destination logged")
	}
}
