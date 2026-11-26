// Package doccli implements small Encapsulated Document file tools.
package doccli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/encapdoc"
	"github.com/ThalesMMS/dicom-go/internal/wrappercli"
	"github.com/ThalesMMS/dicom-go/object"
)

func Execute(ctx context.Context, tool string, args []string, out, errout io.Writer) error {
	f := wrappercli.Flags(tool, "[flags] input output", errout)
	var opts encapdoc.DocumentOptions
	var kind, frame, identifier, source, profile string
	var max int64
	f.StringVar(&opts.PatientName, "patient-name", "", "Patient's Name")
	f.StringVar(&opts.PatientID, "patient-id", "", "Patient ID")
	f.StringVar(&opts.StudyInstanceUID, "study-uid", "", "Study UID (generated when absent)")
	f.StringVar(&opts.SeriesInstanceUID, "series-uid", "", "Series UID (generated when absent)")
	f.StringVar(&opts.SOPInstanceUID, "instance-uid", "", "SOP Instance UID (generated when absent)")
	f.StringVar(&opts.DocumentTitle, "title", "", "Document Title")
	f.StringVar(&source, "study-from", "", "inherit patient/study modules from this instance")
	f.StringVar(&frame, "frame-of-reference", "", "required STL/OBJ Frame of Reference UID")
	f.StringVar(&identifier, "hl7-instance-identifier", "", "CDA root^extension (otherwise from XML)")
	f.StringVar(&kind, "type", "", "dcmencap/dcmdecap format: pdf,cda,stl,obj,mtl")
	f.Int64Var(&max, "max-bytes", wrappercli.MaxInputBytes, "input byte limit (up to 64 MiB)")
	if tool == "cda2dcm" {
		f.StringVar(&profile, "check-profile", "", "validate the selected CDA header and local links before writing: base or ccd")
	}
	if err := wrappercli.Parse(f, args, 2, 2); err != nil {
		return err
	}
	if max <= 0 || max > wrappercli.MaxInputBytes {
		return wrappercli.ErrUsage
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	input, output := f.Arg(0), f.Arg(1)
	if tool == "dcm2pdf" || tool == "dcmdecap" {
		file, err := wrappercli.ReadDICOM(input, false)
		if err != nil {
			return err
		}
		defer file.Close()
		doc, err := encapdoc.Extract(file.Dataset)
		if err != nil {
			return err
		}
		if tool == "dcm2pdf" && doc.SOPClassUID != encapdoc.EncapsulatedPDFStorage {
			return errors.New("input is not an Encapsulated PDF")
		}
		if kind != "" && doc.MIMEType != mime(kind) {
			return errors.New("encapsulated document does not match -type")
		}
		if int64(len(doc.Data)) > max {
			return errors.New("document exceeds byte limit")
		}
		return wrappercli.WriteNew(output, func(w io.Writer) error { _, err := w.Write(doc.Data); return err })
	}
	switch tool {
	case "pdf2dcm":
		kind = "pdf"
	case "cda2dcm":
		kind = "cda"
	case "stl2dcm":
		kind = "stl"
	}
	if mime(kind) == "" {
		return wrappercli.ErrUsage
	}
	if profile != "" && profile != string(encapdoc.CDAProfileBaseHeader) && profile != string(encapdoc.CDAProfileCCDHeader) {
		return encapdoc.ErrUnsupportedCDAValidationProfile
	}
	var err error
	opts.Data, err = wrappercli.Read(input, max)
	if err != nil {
		return err
	}
	if profile != "" {
		report, err := encapdoc.ValidateCDA(ctx, opts.Data, encapdoc.CDAValidationOptions{
			Profile: encapdoc.CDAValidationProfile(profile),
			Limits:  encapdoc.PayloadLimits{MaxBytes: max},
		})
		if err != nil {
			return err
		}
		if report.HasErrors() {
			var failures strings.Builder
			for i, finding := range report.Findings {
				if i != 0 {
					failures.WriteString(", ")
				}
				fmt.Fprintf(&failures, "%s at %s (%d)", finding.Code, finding.Path, finding.Count)
			}
			return fmt.Errorf("%w: CDA %s profile failed with %s", encapdoc.ErrInvalidDocument, report.Profile, failures.String())
		}
	}
	if source != "" {
		from, err := wrappercli.ReadDICOM(source, true)
		if err != nil {
			return err
		}
		defer from.Close()
		opts.Common = object.DerivedCommon{Source: from.Dataset}
		if opts.StudyInstanceUID == "" {
			opts.StudyInstanceUID, _ = from.Dataset.GetUID(core.NewTag(0x0020, 0x000D))
		}
	}
	for _, uid := range []*string{&opts.SOPInstanceUID, &opts.StudyInstanceUID, &opts.SeriesInstanceUID} {
		if *uid == "" {
			*uid, err = wrappercli.UID()
			if err != nil {
				return err
			}
		}
	}
	var file *object.File
	switch kind {
	case "pdf":
		if !bytes.HasPrefix(opts.Data, []byte("%PDF-")) {
			return errors.New("PDF signature missing")
		}
		file, err = encapdoc.NewPDF(opts)
	case "cda":
		file, err = encapdoc.NewCDA(encapdoc.CDAOptions{DocumentOptions: opts, HL7InstanceIdentifier: identifier})
	case "stl":
		file, err = encapdoc.NewSTL(encapdoc.ModelOptions{DocumentOptions: opts, FrameOfReferenceUID: frame})
	case "obj":
		file, err = encapdoc.NewOBJ(encapdoc.ModelOptions{DocumentOptions: opts, FrameOfReferenceUID: frame})
	case "mtl":
		file, err = encapdoc.NewMTL(encapdoc.ModelOptions{DocumentOptions: opts})
	}
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err = wrappercli.WriteFile(output, file); err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, "Encapsulated document written")
	return err
}
func mime(k string) string {
	switch k {
	case "pdf":
		return encapdoc.MIMETypePDF
	case "cda":
		return encapdoc.MIMETypeCDA
	case "stl":
		return encapdoc.MIMETypeSTL
	case "obj":
		return encapdoc.MIMETypeOBJ
	case "mtl":
		return encapdoc.MIMETypeMTL
	}
	return ""
}
