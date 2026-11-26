package dimsecli

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/ThalesMMS/dicom-go/net/dimse"
)

// WriteNormalizedStatus prints one normalized-service outcome line:
// "<label> status=0xHHHH (<class>)" followed by the Error Comment and
// Error ID when the peer returned them.
func WriteNormalizedStatus(w io.Writer, label string, status uint16, fields *dimse.NormalizedStatusFields, extra ...string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s status=0x%04X (%s)", label, status, dimse.ClassifyNormalizedStatus(status))
	for _, value := range extra {
		if value != "" {
			b.WriteString(" ")
			b.WriteString(value)
		}
	}
	if fields != nil {
		if fields.ErrorIDOrNil != nil {
			fmt.Fprintf(&b, " error-id=0x%04X", *fields.ErrorIDOrNil)
		}
		if fields.ErrorComment != "" {
			fmt.Fprintf(&b, " comment=%q", fields.ErrorComment)
		}
		for _, tag := range fields.OffendingElements {
			fmt.Fprintf(&b, " offending=%s", tag)
		}
		for _, tag := range fields.AttributeIdentifierList {
			fmt.Fprintf(&b, " attribute=%s", tag)
		}
	}
	b.WriteString("\n")
	_, err := io.WriteString(w, b.String())
	return err
}

// IgnoreNormalizedWarning returns nil for a normalized warning status, which
// the tools report but do not treat as a failure, and err otherwise.
func IgnoreNormalizedWarning(err error) error {
	var statusErr *dimse.NormalizedStatusError
	if errors.As(err, &statusErr) && statusErr.Class == dimse.NormalizedStatusWarning {
		return nil
	}
	return err
}
