package sr

import (
	"context"
	"github.com/ThalesMMS/dicom-go/core"
)

// Promotion admission hashes/bounds the complete model before cloning. Every
// mutable field in the existing ContentItem contract is detached; no serializer,
// deferred provider or Value method is invoked by this copy.
func cloneLibrarySlice[S ~[]E, E any](source S) S {
	if source == nil {
		return nil
	}
	out := make(S, len(source))
	copy(out, source)
	return out
}
func cloneImageLibraryGroups(ctx context.Context, source []ImageLibraryGroup) ([]ImageLibraryGroup, error) {
	out := cloneLibrarySlice(source)
	for i, g := range source {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		out[i].Order = cloneLibrarySlice(g.Order)
		var err error
		out[i].Header, err = cloneLibraryItem(ctx, g.Header)
		if err != nil {
			return nil, err
		}
		out[i].Descriptors, err = cloneLibraryItems(ctx, g.Descriptors)
		if err != nil {
			return nil, err
		}
		out[i].Entries = cloneLibrarySlice(g.Entries)
		for j, e := range g.Entries {
			out[i].Entries[j].Image.Frames = cloneLibrarySlice(e.Image.Frames)
			out[i].Entries[j].Header, err = cloneLibraryItem(ctx, e.Header)
			if err != nil {
				return nil, err
			}
			out[i].Entries[j].Descriptors, err = cloneLibraryItems(ctx, e.Descriptors)
			if err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}
func cloneLibraryItems(ctx context.Context, source []ContentItem) ([]ContentItem, error) {
	out := cloneLibrarySlice(source)
	for i, item := range source {
		var err error
		out[i], err = cloneLibraryItem(ctx, item)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
func cloneLibraryItem(ctx context.Context, item ContentItem) (ContentItem, error) {
	if err := ctx.Err(); err != nil {
		return ContentItem{}, err
	}
	out := item
	if item.Measurement != nil {
		m := *item.Measurement
		out.Measurement = &m
	}
	out.Image.Frames = cloneLibrarySlice(item.Image.Frames)
	out.Waveform.Channels = cloneLibrarySlice(item.Waveform.Channels)
	out.Spatial.Coordinates = cloneLibrarySlice(item.Spatial.Coordinates)
	out.Spatial3D.Coordinates = cloneLibrarySlice(item.Spatial3D.Coordinates)
	out.Temporal.SamplePositions = cloneLibrarySlice(item.Temporal.SamplePositions)
	out.Temporal.TimeOffsets = cloneLibrarySlice(item.Temporal.TimeOffsets)
	out.Temporal.DateTimes = cloneLibrarySlice(item.Temporal.DateTimes)
	out.ReferencedContentItemIdentifier = cloneLibrarySlice(item.ReferencedContentItemIdentifier)
	var err error
	out.ValueElements, err = cloneLibraryElements(ctx, item.ValueElements)
	if err != nil {
		return ContentItem{}, err
	}
	out.Children, err = cloneLibraryItems(ctx, item.Children)
	if err != nil {
		return ContentItem{}, err
	}
	return out, nil
}
func cloneLibraryElements(ctx context.Context, source []core.Element) ([]core.Element, error) {
	out := cloneLibrarySlice(source)
	for i, e := range source {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		switch v := e.Value.(type) {
		case nil:
		case core.RawValue:
			out[i].Value = cloneLibrarySlice(v)
		case core.StringValue:
			out[i].Value = cloneLibrarySlice(v)
		case core.Uint16Value:
			out[i].Value = cloneLibrarySlice(v)
		case core.Int16Value:
			out[i].Value = cloneLibrarySlice(v)
		case core.Uint32Value:
			out[i].Value = cloneLibrarySlice(v)
		case core.Int32Value:
			out[i].Value = cloneLibrarySlice(v)
		case core.Uint64Value:
			out[i].Value = cloneLibrarySlice(v)
		case core.Int64Value:
			out[i].Value = cloneLibrarySlice(v)
		case core.Float32Value:
			out[i].Value = cloneLibrarySlice(v)
		case core.Float64Value:
			out[i].Value = cloneLibrarySlice(v)
		case core.TagValue:
			out[i].Value = cloneLibrarySlice(v)
		case core.BulkDataValue: // The URI string is immutable; no resolution occurs.
		case core.FragmentSequence:
			c := v
			c.OffsetTable = cloneLibrarySlice(v.OffsetTable)
			c.Fragments = cloneLibrarySlice(v.Fragments)
			for j, f := range v.Fragments {
				c.Fragments[j] = cloneLibrarySlice(f)
			}
			out[i].Value = c
		case core.SequenceValue:
			c := v
			c.Items = cloneLibrarySlice(v.Items)
			for j, d := range v.Items {
				var err error
				c.Items[j].Elements, err = cloneLibraryElements(ctx, d.Elements)
				if err != nil {
					return nil, err
				}
			}
			out[i].Value = c
		default:
			return nil, imageLibraryError("nonresident-or-custom-value", "")
		}
	}
	return out, nil
}

// Bound all retained topology, including Header fields replaced when encoding,
// before hashing or recursive copying. Sequence payloads have a separate finite
// attribute budget, matching object admission; no Value methods are consulted.
func preflightLibraryClone(ctx context.Context, groups []ImageLibraryGroup, o ImageLibraryOptions) error {
	nodes, attributes := 0, 0
	limit := func(n int) error {
		if n > o.MaxItems {
			return imageLibraryError("resource-limit", "")
		}
		return nil
	}
	var elements func([]core.Element, int) error
	elements = func(es []core.Element, depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if depth > o.MaxDepth || len(es) > o.MaxItems*64-attributes {
			return imageLibraryError("resource-limit", "")
		}
		attributes += len(es)
		for _, e := range es {
			switch v := e.Value.(type) {
			case core.StringValue:
				if len(v) > o.MaxItems*64-attributes {
					return imageLibraryError("resource-limit", "")
				}
				attributes += len(v)
			case core.SequenceValue:
				if len(v.Items) > o.MaxItems*64-attributes {
					return imageLibraryError("resource-limit", "")
				}
				attributes += len(v.Items)
				for _, d := range v.Items {
					if err := elements(d.Elements, depth+1); err != nil {
						return err
					}
				}
			case core.FragmentSequence:
				if err := limit(len(v.Fragments)); err != nil {
					return err
				}
			}
		}
		return nil
	}
	var item func(ContentItem, int) error
	item = func(v ContentItem, depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if depth > o.MaxDepth || nodes >= o.MaxItems {
			return imageLibraryError("resource-limit", "")
		}
		nodes++
		for _, n := range []int{len(v.Children), len(v.Image.Frames), len(v.Waveform.Channels), len(v.Spatial.Coordinates), len(v.Spatial3D.Coordinates), len(v.Temporal.SamplePositions), len(v.Temporal.TimeOffsets), len(v.Temporal.DateTimes), len(v.ReferencedContentItemIdentifier)} {
			if err := limit(n); err != nil {
				return err
			}
		}
		if len(v.Children) > o.MaxItems-nodes {
			return imageLibraryError("resource-limit", "")
		}
		if err := elements(v.ValueElements, depth); err != nil {
			return err
		}
		for _, c := range v.Children {
			if err := item(c, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := limit(len(groups)); err != nil {
		return err
	}
	for _, g := range groups {
		if err := item(g.Header, 2); err != nil {
			return err
		}
		if err := limit(len(g.Order)); err != nil {
			return err
		}
		for _, d := range g.Descriptors {
			if err := item(d, 3); err != nil {
				return err
			}
		}
		if err := limit(len(g.Entries)); err != nil {
			return err
		}
		for _, e := range g.Entries {
			if err := limit(len(e.Image.Frames)); err != nil {
				return err
			}
			if err := item(e.Header, 3); err != nil {
				return err
			}
			for _, d := range e.Descriptors {
				if err := item(d, 4); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
