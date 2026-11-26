package file_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"testing"

	legacy "github.com/ThalesMMS/dicom-go/internal/clidiag"
	filediag "github.com/ThalesMMS/dicom-go/internal/clidiag/file"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestClassifyFileErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want filediag.Class
	}{
		{"nil", nil, filediag.ClassError},
		{"not exist", os.ErrNotExist, filediag.ClassFile},
		{"permission", os.ErrPermission, filediag.ClassFile},
		{"path error", &os.PathError{Op: "open", Path: "fixture.dcm", Err: os.ErrNotExist}, filediag.ClassFile},
		{"parse type", &parser.ParseError{Err: errors.New("invalid token")}, filediag.ClassParse},
		{"preamble", object.ErrMissingPreamble, filediag.ClassParse},
		{"missing syntax", object.ErrMissingTransferSyntax, filediag.ClassParse},
		{"file meta", object.ErrFileMeta, filediag.ClassParse},
		{"dataset", object.ErrDataSet, filediag.ClassParse},
		{"meta group length", object.ErrInvalidFileMetaGroupLength, filediag.ClassParse},
		{"meta group length value", object.ErrInvalidFileMetaGroupLengthValue, filediag.ClassParse},
		{"preamble length", object.ErrInvalidPreambleLength, filediag.ClassParse},
		{"odd length", parser.ErrOddElementLength, filediag.ClassParse},
		{"depth", parser.ErrMaxDepthExceeded, filediag.ClassParse},
		{"element bytes", parser.ErrMaxElementBytesExceeded, filediag.ClassParse},
		{"elements", parser.ErrMaxElementsExceeded, filediag.ClassParse},
		{"fragments", parser.ErrMaxFragmentsExceeded, filediag.ClassParse},
		{"total bytes", parser.ErrMaxTotalBytesExceeded, filediag.ClassParse},
		{"item delimiter", parser.ErrUnexpectedItemDelimiter, filediag.ClassParse},
		{"sequence delimiter", parser.ErrUnexpectedSequenceDelimiter, filediag.ClassParse},
		{"unexpected eof", io.ErrUnexpectedEOF, filediag.ClassParse},
		{"unknown syntax", transfer.ErrUnknownTransferSyntax, filediag.ClassTransfer},
		{"unsupported syntax", transfer.ErrUnsupportedTransferSyntax, filediag.ClassTransfer},
		{"canceled", context.Canceled, filediag.ClassNetwork},
		{"deadline", context.DeadlineExceeded, filediag.ClassNetwork},
		{"closed", net.ErrClosed, filediag.ClassNetwork},
		{"net error", &net.DNSError{Err: "synthetic timeout", IsTimeout: true}, filediag.ClassNetwork},
		{"eof", io.EOF, filediag.ClassError},
		{"plain", errors.New("plain error"), filediag.ClassError},
		{"text is not identity", errors.New(object.ErrMissingPreamble.Error()), filediag.ClassError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, err := range []error{tt.err, fmt.Errorf("outer: %w", tt.err)} {
				if tt.err == nil && err != nil {
					continue
				}
				if got := filediag.Classify(err); got != tt.want {
					t.Fatalf("file Classify(%v) = %q, want %q", err, got, tt.want)
				}
				if got := legacy.Classify(err); got != tt.want {
					t.Fatalf("legacy Classify(%v) = %q, want %q", err, got, tt.want)
				}
			}
		})
	}
}

func TestLegacyJoinedPriority(t *testing.T) {
	// The standard network class from the file classifier must be deferred until
	// after media and codec classification in the complete classifier.
	ordered := []struct {
		err  error
		want filediag.Class
	}{
		{os.ErrPermission, filediag.ClassFile},
		{object.ErrMissingPreamble, filediag.ClassParse},
		{transfer.ErrUnsupportedTransferSyntax, filediag.ClassTransfer},
		{pixeldata.ErrMediaPayloadNotRenderable, filediag.ClassMedia},
		{pixeldata.ErrCodecNotFound, filediag.ClassCodec},
		{context.DeadlineExceeded, filediag.ClassNetwork},
	}
	for i, first := range ordered {
		for j := i + 1; j < len(ordered); j++ {
			second := ordered[j]
			t.Run(string(first.want)+" before "+string(second.want), func(t *testing.T) {
				for _, err := range []error{
					errors.Join(first.err, second.err),
					fmt.Errorf("outer: %w", errors.Join(second.err, first.err)),
				} {
					if got := legacy.Classify(err); got != first.want {
						t.Fatalf("Classify(%v) = %q, want %q", err, got, first.want)
					}
				}
			})
		}
	}
	for _, network := range []error{context.Canceled, net.ErrClosed, &net.DNSError{Err: "synthetic"}, ul.ErrAssociationRejected} {
		for _, higher := range ordered[:5] {
			if got := legacy.Classify(errors.Join(network, higher.err)); got != higher.want {
				t.Fatalf("network joined with %q = %q", higher.want, got)
			}
		}
	}
	all := []error{errors.New("unknown")}
	for i := len(ordered) - 1; i >= 0; i-- {
		all = append(all, ordered[i].err)
	}
	if got := legacy.Classify(errors.Join(all...)); got != filediag.ClassFile {
		t.Fatalf("all classes joined = %q", got)
	}
}

func TestFileJoinedPriority(t *testing.T) {
	err := errors.Join(context.Canceled, transfer.ErrUnknownTransferSyntax, io.ErrUnexpectedEOF)
	if got := filediag.Classify(err); got != filediag.ClassParse {
		t.Fatalf("Classify(parse+transfer+network) = %q", got)
	}
	if got := filediag.Classify(errors.Join(err, os.ErrNotExist)); got != filediag.ClassFile {
		t.Fatalf("Classify(file+parse+transfer+network) = %q", got)
	}
}

func TestFprintlnPreservesFileDiagnostics(t *testing.T) {
	for _, tool := range []string{"dcmdump", ""} {
		for _, err := range []error{nil, object.ErrMissingPreamble, fmt.Errorf("read: %w", transfer.ErrUnsupportedTransferSyntax), os.ErrNotExist, context.Canceled, errors.New("plain error")} {
			var got, want bytes.Buffer
			filediag.Fprintln(&got, tool, err)
			legacy.Fprintln(&want, tool, err)
			if got.String() != want.String() {
				t.Fatalf("Fprintln(%q, %v) = %q, want %q", tool, err, got.String(), want.String())
			}
		}
	}
	var buf bytes.Buffer
	filediag.Fprintln(&buf, "dcmdump", object.ErrMissingPreamble)
	if got, want := buf.String(), "dcmdump: parse: dicom: missing Part 10 preamble or DICM marker\n"; got != want {
		t.Fatalf("Fprintln() = %q, want %q", got, want)
	}
}
