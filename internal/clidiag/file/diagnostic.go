// Package file classifies file, parser, transfer syntax and standard library
// I/O errors without importing DICOM networking or pixel codecs.
package file

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"

	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
)

type Class string

const (
	ClassError    Class = "error"
	ClassFile     Class = "file"
	ClassParse    Class = "parse"
	ClassTransfer Class = "transfer"
	ClassCodec    Class = "codec"
	ClassMedia    Class = "media"
	ClassNetwork  Class = "network"
)

// Classify recognizes errors available to file tools. ClassCodec and ClassMedia
// are shared labels for the complete classifier; this package does not emit them.
func Classify(err error) Class {
	if err == nil {
		return ClassError
	}
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
		return ClassFile
	}
	if isParseError(err) {
		return ClassParse
	}
	if errors.Is(err, transfer.ErrUnknownTransferSyntax) || errors.Is(err, transfer.ErrUnsupportedTransferSyntax) {
		return ClassTransfer
	}
	var netErr net.Error
	if errors.As(err, &netErr) || errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) || errors.Is(err, net.ErrClosed) {
		return ClassNetwork
	}
	return ClassError
}

func Fprintln(w io.Writer, tool string, err error) {
	if err == nil {
		return
	}
	if tool == "" {
		_, _ = fmt.Fprintf(w, "%s: %v\n", Classify(err), err)
		return
	}
	_, _ = fmt.Fprintf(w, "%s: %s: %v\n", tool, Classify(err), err)
}

func isParseError(err error) bool {
	var parseErr *parser.ParseError
	if errors.As(err, &parseErr) {
		return true
	}
	return errors.Is(err, object.ErrMissingPreamble) ||
		errors.Is(err, object.ErrMissingTransferSyntax) ||
		errors.Is(err, object.ErrFileMeta) ||
		errors.Is(err, object.ErrDataSet) ||
		errors.Is(err, object.ErrInvalidFileMetaGroupLength) ||
		errors.Is(err, object.ErrInvalidFileMetaGroupLengthValue) ||
		errors.Is(err, object.ErrInvalidPreambleLength) ||
		errors.Is(err, parser.ErrOddElementLength) ||
		errors.Is(err, parser.ErrMaxDepthExceeded) ||
		errors.Is(err, parser.ErrMaxElementBytesExceeded) ||
		errors.Is(err, parser.ErrMaxElementsExceeded) ||
		errors.Is(err, parser.ErrMaxFragmentsExceeded) ||
		errors.Is(err, parser.ErrMaxTotalBytesExceeded) ||
		errors.Is(err, parser.ErrUnexpectedItemDelimiter) ||
		errors.Is(err, parser.ErrUnexpectedSequenceDelimiter) ||
		errors.Is(err, io.ErrUnexpectedEOF)
}
