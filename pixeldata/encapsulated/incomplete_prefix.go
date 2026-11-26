package encapsulated

import (
	"context"
	"fmt"
	"math"
)

// PrefixReport distinguishes complete codestreams from retained complete Items
// belonging to an interrupted terminal frame. No missing sample is supplied.
type PrefixReport struct {
	DeclaredFrames, RetainedFrames, RetainedFragments, DiscardedFragments int
	DiscardedBytes                                                        uint64
}

type prefixSource struct {
	Source
	count int
}

func (s prefixSource) FragmentCount() int { return s.count }

// CompletePrefix verifies JPEG/JPEG-LS codestream endings before planning a
// contiguous complete prefix. It is an explicit malformed-input inspection
// API, never the default Plan.New policy. EOT recovery is refused in this
// profile. BOT conflicts and structurally invalid middle markers stay errors.
func CompletePrefix(ctx context.Context, source Source, tables Tables, frames int, format Format, limits Limits) (*Plan, PrefixReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, PrefixReport{}, err
	}
	l, err := limits.normalized()
	if err != nil {
		return nil, PrefixReport{}, err
	}
	if source == nil || frames < 1 || (format != JPEG && format != JPEGLS) || tables.ExtendedPresent || tables.LengthsPresent || len(tables.Extended) > 0 || len(tables.Lengths) > 0 {
		return nil, PrefixReport{}, ErrLayout
	}
	n := source.FragmentCount()
	if frames > l.MaxFrames || n > l.MaxFragments {
		return nil, PrefixReport{}, ErrResourceLimit
	}
	if n < 1 {
		return nil, PrefixReport{}, ErrFrameCount
	}
	p := &Plan{source: source, sizes: make([]uint64, n)}
	itemStarts := make([]uint64, n)
	var offset uint64
	for i := range p.sizes {
		if err := ctx.Err(); err != nil {
			return nil, PrefixReport{}, err
		}
		size, e := source.FragmentSize(i)
		if e != nil {
			return nil, PrefixReport{}, fmt.Errorf("%w: %w", ErrSource, e)
		}
		if size == 0 || size&1 != 0 {
			return nil, PrefixReport{}, ErrLayout
		}
		if size > math.MaxInt || size > l.MaxBytes-p.inputBytes {
			return nil, PrefixReport{}, ErrResourceLimit
		}
		p.sizes[i], itemStarts[i] = size, offset
		p.inputBytes += size
		offset, e = nextFragmentItemOffset(offset, int(size))
		if e != nil {
			return nil, PrefixReport{}, e
		}
	}
	// Admission covers the original table count before a smaller prefix index.
	if len(tables.Basic) > 0 {
		if _, err := basicFrameStarts(tables.Basic, itemStarts, frames); err != nil {
			return nil, PrefixReport{}, err
		}
	}
	starts, retained, items, err := scanFrameStarts(ctx, p, frames, format, l, true)
	if err != nil {
		return nil, PrefixReport{}, err
	}
	if retained < 1 {
		return nil, PrefixReport{}, ErrFrameCount
	}
	basic := tables.Basic
	if len(basic) > 0 {
		declaredStarts, _ := basicFrameStarts(basic, itemStarts, frames)
		for i := 0; i < retained; i++ {
			if declaredStarts[i] != starts[i] {
				return nil, PrefixReport{}, ErrLayout
			}
		}
		basic = basic[:retained*4]
	}
	report := PrefixReport{DeclaredFrames: frames, RetainedFrames: retained, RetainedFragments: items, DiscardedFragments: n - items}
	for _, size := range p.sizes[items:] {
		report.DiscardedBytes += size
	}
	plan, err := New(ctx, prefixSource{Source: source, count: items}, Tables{Basic: basic}, retained, format, l)
	if err != nil {
		return nil, PrefixReport{}, err
	}
	return plan, report, nil
}
