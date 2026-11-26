package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/internal/hl7cli"
)

func TestCommandHelp(t *testing.T) {
	var out, err bytes.Buffer
	if code := hl7cli.Run(context.Background(), "xml2hl7", []string{"-h"}, strings.NewReader(""), &out, &err); code != 0 {
		t.Fatalf("help exit %d", code)
	}
	if !strings.Contains(err.String(), "Usage: xml2hl7") {
		t.Fatal("command usage missing")
	}
	if out.Len() != 0 {
		t.Fatal("help wrote payload output")
	}
}
