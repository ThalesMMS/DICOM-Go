// Package srcli wraps SR renderers and KOS constructors.
package srcli

import (
	"context"
	"fmt"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/wrappercli"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/sr"
	"io"
	"strings"
)

func Execute(ctx context.Context, tool string, args []string, out, errout io.Writer) error {
	f := wrappercli.Flags(tool, "[flags] input.dcm [output]", errout)
	if tool != "mkkos" {
		var codes bool
		var maxItems, maxPoints int
		f.BoolVar(&codes, "show-codes", false, "show coded concepts")
		f.IntVar(&maxItems, "max-items", 100000, "maximum rendered content items")
		f.IntVar(&maxPoints, "max-points", 32, "maximum displayed coordinate points")
		if err := wrappercli.Parse(f, args, 1, 2); err != nil {
			return err
		}
		if maxItems <= 0 || maxItems > 100000 || maxPoints <= 0 || maxPoints > 10000 {
			return wrappercli.ErrUsage
		}
		file, err := wrappercli.ReadDICOM(f.Arg(0), true)
		if err != nil {
			return err
		}
		defer file.Close()
		doc, err := sr.ReadDocument(file.Dataset)
		if err != nil {
			return err
		}
		render := func(w io.Writer) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			opts := sr.RenderOptions{ShowCodes: codes, MaxItems: maxItems, MaxPoints: maxPoints}
			if tool == "dsr2html" {
				return sr.RenderHTML(w, doc, opts)
			}
			return sr.RenderText(w, doc, opts)
		}
		if f.NArg() == 2 {
			return wrappercli.WriteNew(f.Arg(1), render)
		}
		return render(out)
	}
	var output, title, reason, refType string
	f.StringVar(&output, "o", "", "new output KOS file (required)")
	f.StringVar(&title, "title", "113000", "DCM title: 113000 Of Interest,113030 Manifest,113001 Quality,113037 Patient Safety,113038 Incorrect Worklist,113039 Retention Expired")
	f.StringVar(&reason, "quality-reason", "", "one CID7011 coded reason as code|scheme|meaning (Quality title only)")
	f.StringVar(&refType, "reference-type", "IMAGE", "rejection reference type IMAGE,WAVEFORM,COMPOSITE")
	if err := wrappercli.Parse(f, args, 1, wrappercli.MaxFiles); err != nil {
		return err
	}
	if output == "" {
		return wrappercli.ErrUsage
	}
	uid, err := wrappercli.UID()
	if err != nil {
		return err
	}
	series, err := wrappercli.UID()
	if err != nil {
		return err
	}
	var refs []sr.ImageReference
	var rejects []sr.KOSRejectionReference
	var common object.DerivedCommon
	study := ""
	for i, path := range f.Args() {
		if err := ctx.Err(); err != nil {
			return err
		}
		file, err := wrappercli.ReadDICOM(path, true)
		if err != nil {
			return err
		}
		ds := file.Dataset
		if i == 0 {
			common.Source = ds
			study, _ = ds.GetUID(core.NewTag(0x0020, 0x000D))
		}
		ref := sr.ImageReference{}
		ref.StudyInstanceUID, _ = ds.GetUID(core.NewTag(0x0020, 0x000D))
		ref.SeriesInstanceUID, _ = ds.GetUID(core.NewTag(0x0020, 0x000E))
		ref.SOPClassUID, _ = ds.GetUID(core.NewTag(0x0008, 0x0016))
		ref.SOPInstanceUID, _ = ds.GetUID(core.NewTag(0x0008, 0x0018))
		file.Close()
		if !core.IsValidUID(ref.StudyInstanceUID) || !core.IsValidUID(ref.SeriesInstanceUID) || !core.IsValidUID(ref.SOPClassUID) || !core.IsValidUID(ref.SOPInstanceUID) {
			return fmt.Errorf("input %d has missing or invalid identity", i+1)
		}
		if ref.StudyInstanceUID != study {
			return fmt.Errorf("input %d belongs to another study", i+1)
		}
		refs = append(refs, ref)
		rejects = append(rejects, sr.KOSRejectionReference{ValueType: sr.ValueType(refType), StudyInstanceUID: ref.StudyInstanceUID, SeriesInstanceUID: ref.SeriesInstanceUID, SOPClassUID: ref.SOPClassUID, SOPInstanceUID: ref.SOPInstanceUID})
	}
	var ds *object.Object
	if title == "113000" || title == "113030" {
		if reason != "" || refType != "IMAGE" {
			return wrappercli.ErrUsage
		}
		doc := sr.NewKeyObjectSelection(uid, refs, nil)
		if title == "113030" {
			doc.Title = sr.KOSTitleManifest
		}
		doc.SeriesInstanceUID = series
		doc.Common = common
		ds, err = doc.Dataset()
	} else {
		kind := map[string]sr.KOSRejectionKind{"113001": sr.KOSRejectionQuality, "113037": sr.KOSRejectionPatientSafety, "113038": sr.KOSRejectionIncorrectWorklist, "113039": sr.KOSRejectionRetentionExpired}[title]
		if kind == 0 {
			return wrappercli.ErrUsage
		}
		opts := sr.KOSRejectionOptions{SOPInstanceUID: uid, StudyInstanceUID: study, SeriesInstanceUID: series, References: rejects, Common: common}
		if reason != "" {
			parts := strings.Split(reason, "|")
			if len(parts) != 3 {
				return wrappercli.ErrUsage
			}
			opts.QualityReasons = []sr.CodedEntry{{CodeValue: parts[0], CodingSchemeDesignator: parts[1], CodeMeaning: parts[2]}}
		}
		ds, err = sr.NewKOSRejection(ctx, kind, opts)
	}
	if err != nil {
		return err
	}
	file, err := wrappercli.DatasetFile(ds)
	if err != nil {
		return err
	}
	return wrappercli.WriteFile(output, file)
}
