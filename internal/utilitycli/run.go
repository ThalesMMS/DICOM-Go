// Package utilitycli wraps templates, the Part10 marker check and private shutdown negotiation.
package utilitycli

import (
	"context"
	"errors"
	"fmt"
	"github.com/ThalesMMS/dicom-go/internal/dimsecli"
	"github.com/ThalesMMS/dicom-go/internal/nofollow"
	"github.com/ThalesMMS/dicom-go/internal/wrappercli"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/pathtemplate"
	"io"
)

// PrivateShutdownUID is the DCMTK-specific negotiation request, not a standard DICOM service.
const PrivateShutdownUID = "1.2.276.0.7230010.3.4.1915765545.18030.917282194.0"

func Execute(ctx context.Context, tool string, args []string, out, errout io.Writer) error {
	f := wrappercli.Flags(tool, "[flags] file...", errout)
	if tool == "termscu" {
		var c dimsecli.ConnectionFlags
		c.Register(f, "TERMSCU")
		if err := wrappercli.Parse(f, args, 0, 0); err != nil {
			return err
		}
		if err := c.Validate(); err != nil {
			return wrappercli.ErrUsage
		}
		op, cancel := context.WithTimeout(ctx, c.Timeout)
		defer cancel()
		a, err := c.Dial(op, []ul.PresentationContext{{ID: 1, AbstractSyntaxUID: PrivateShutdownUID, TransferSyntaxUIDs: []string{ul.ExplicitVRLittleEndian, ul.ImplicitVRLittleEndian}}}, nil)
		if err != nil {
			return fmt.Errorf("private shutdown association: %w (shutdown outcome is not confirmed)", err)
		}
		defer a.Close()
		if err = dimsecli.Release(a); err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, "Private shutdown context accepted and association released; shutdown outcome is not confirmed")
		return err
	}
	var pattern string
	if tool == "dcm2str" {
		f.StringVar(&pattern, "p", "", "relative path template (required)")
	}
	if err := wrappercli.Parse(f, args, 1, wrappercli.MaxFiles); err != nil {
		return err
	}
	var template *pathtemplate.Template
	var err error
	if tool == "dcm2str" {
		if pattern == "" {
			return wrappercli.ErrUsage
		}
		template, err = pathtemplate.Compile(pattern, pathtemplate.Options{})
		if err != nil {
			return err
		}
	}
	bad := false
	for _, path := range f.Args() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if tool == "dcmftest" {
			ok := hasMarker(path)
			label := "no"
			if ok {
				label = "yes"
			} else {
				bad = true
			}
			if _, err = fmt.Fprintf(out, "%s: %s\n", label, path); err != nil {
				return err
			}
			continue
		}
		file, err := wrappercli.ReadDICOM(path, true)
		if err != nil {
			return err
		}
		value, err := template.Format(file.Dataset)
		file.Close()
		if err != nil {
			return err
		}
		if _, err = fmt.Fprintln(out, value); err != nil {
			return err
		}
	}
	if bad {
		return errors.New("one or more inputs do not have a Part10 DICM marker")
	}
	return nil
}
func hasMarker(path string) bool {
	file, err := nofollow.OpenFile(path)
	if err != nil {
		return false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	var marker [4]byte
	n, err := file.ReadAt(marker[:], 128)
	return err == nil && n == 4 && string(marker[:]) == "DICM"
}
