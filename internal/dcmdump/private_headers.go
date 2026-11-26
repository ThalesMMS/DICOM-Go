package dcmdump

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/vendors/headers"
)

type privateHeaderOptions struct{ decodePrivateHeaders bool }

func (o *privateHeaderOptions) register(fs *flag.FlagSet) {
	fs.BoolVar(&o.decodePrivateHeaders, "decode-additional-private-headers", false, "explicitly decode Philips/Toshiba/Canon/PMTF/PAS headers as text (may contain personal data; fixed finite limits)")
}
func dumpPrivateHeaders(ctx context.Context, w io.Writer, dataset *object.Object) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	decoded := headers.DecodePrivateHeaders(dataset, headers.Options{})
	if len(decoded) == 0 {
		_, err := fmt.Fprintln(w, "No supported Philips/Toshiba/Canon/PMTF/PAS private headers.")
		return err
	}
	failed := false
	for _, header := range decoded {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "== %s %s ==\n", header.Title, header.Tag); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(w, "(may contain personal data)"); err != nil {
			return err
		}
		if header.Err != nil {
			failed = true
			if _, err := fmt.Fprintf(w, "Could not decode: %v\n", header.Err); err != nil {
				return err
			}
			continue
		}
		for _, line := range header.Lines {
			if _, err := fmt.Fprintln(w, line); err != nil {
				return err
			}
		}
		if header.Truncated {
			if _, err := fmt.Fprintln(w, "(further lines not shown)"); err != nil {
				return err
			}
		}
	}
	if failed {
		return errors.New("one or more vendor private headers could not be decoded")
	}
	return nil
}
