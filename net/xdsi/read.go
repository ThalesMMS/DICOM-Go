package xdsi

import (
	"bytes"
	"context"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/validation"
)

// Parse the dataset through the validation reader before Object's last-wins
// tag map can hide duplicate identities. Values and aggregate retained
// metadata are bounded even for compressed/deflated images.
func readPart10(ctx context.Context, data []byte, limits Limits) (*object.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	source := bytes.NewReader(data)
	opts := object.ReadFileOptions{
		MaxTotalBytes: limits.TotalBytes, MaxElementBytes: limits.MetadataBytes,
		MaxPixelDataBytes: limits.TotalBytes, MaxSequenceDepth: 24,
		MaxElements: 100000, MaxFragments: 100000, SkipPixelData: true,
	}
	header, err := object.ReadPart10Header(source, opts)
	if err != nil {
		return nil, err
	}
	remaining := limits.MetadataBytes - header.DataSetOffset
	if remaining <= 0 {
		return nil, ErrLimit
	}
	hooks, err := validation.NewHookChain(validation.HookRegistration{
		Name: "xdsi-metadata-budget", Points: []validation.HookPoint{validation.HookElementHeaderRead},
		Hook: validation.HookFunc(func(_ context.Context, event validation.HookEvent) (validation.HookDecision, error) {
			cost := int64(12)
			h := event.Header
			if h.VR != core.VRSQ && !h.Length.IsUndefined() && h.Tag != core.TagPixelData &&
				h.Tag != core.NewTag(0x7fe0, 0x0008) && h.Tag != core.NewTag(0x7fe0, 0x0009) {
				cost += int64(h.Length)
			}
			remaining -= cost
			if remaining < 0 {
				return validation.HookDecision{}, ErrLimit
			}
			return validation.HookDecision{}, nil
		}),
	})
	if err != nil {
		return nil, err
	}
	opts.MaxTotalBytes = limits.TotalBytes - header.DataSetOffset
	dataset, _, err := object.ReadDataSetWithValidation(ctx, source, header.TransferSyntax, opts, validation.Options{
		Mode: validation.ModeStrict, MaxFindings: 32, MaxElements: 100000, MaxDepth: 12, Hooks: hooks,
	})
	if err != nil {
		if dataset != nil {
			dataset.Close()
		}
		return nil, err
	}
	return &object.File{Preamble: header.Preamble, Meta: header.Meta, Dataset: dataset, TransferSyntax: header.TransferSyntax}, nil
}
