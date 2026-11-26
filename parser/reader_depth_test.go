package parser

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"runtime"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/internal/testutil"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// deepNestingStream builds levels nested undefined-length sequences, each
// holding one undefined-length item (36 bytes per level). With outerDefined,
// the whole nest sits in one defined-length sequence and item, so every
// header inside it is bounded by that far-away container.
func deepNestingStream(levels int, outerDefined bool) []byte {
	le := binary.LittleEndian
	seq := dicomtest.SequenceHeaderBytes(transfer.ExplicitVRLittleEndian, core.NewTag(0x0040, 0xa730), 0xFFFFFFFF)
	item := dicomtest.SequenceControlBytes(le, core.TagItem, 0xFFFFFFFF)
	itemEnd := dicomtest.SequenceControlBytes(le, core.TagItemDelimitationItem, 0)
	seqEnd := dicomtest.SequenceControlBytes(le, core.TagSequenceDelimitationItem, 0)
	var nest bytes.Buffer
	for i := 0; i < levels; i++ {
		nest.Write(seq)
		nest.Write(item)
	}
	for i := 0; i < levels; i++ {
		nest.Write(itemEnd)
		nest.Write(seqEnd)
	}
	if !outerDefined {
		return nest.Bytes()
	}
	body := append(dicomtest.SequenceControlBytes(le, core.TagItem, uint32(nest.Len())), nest.Bytes()...)
	outer := dicomtest.SequenceHeaderBytes(transfer.ExplicitVRLittleEndian, core.NewTag(0x0008, 0x1115), uint32(len(body)))
	return append(outer, body...)
}

func readDeepNesting(data []byte, opts ReaderOptions) (*Reader, int, error) {
	reader := NewReader(bytes.NewReader(data), transfer.ExplicitVRLittleEndian, opts)
	tokens := 0
	for {
		_, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return reader, tokens, nil
		}
		if err != nil {
			return reader, tokens, err
		}
		tokens++
	}
}

func TestNextSequenceDepthDefaultAndOverrides(t *testing.T) {
	levelsAtDefault := DefaultMaxSequenceDepth / 2
	tests := []struct {
		name    string
		levels  int
		depth   int
		wantErr bool
	}{
		{name: "default admits 64 nested sequences", levels: levelsAtDefault},
		{name: "default refuses 65 nested sequences", levels: levelsAtDefault + 1, wantErr: true},
		{name: "raised limit admits deeper nesting", levels: 200, depth: 400},
		{name: "raised limit still refuses beyond it", levels: 201, depth: 400, wantErr: true},
		{name: "explicit smaller limit", levels: 3, depth: 4, wantErr: true},
		{name: "unlimited", levels: 5000, depth: UnlimitedSequenceDepth},
	}
	for _, test := range tests {
		for _, outerDefined := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/outerDefined=%v", test.name, outerDefined), func(t *testing.T) {
				levels := test.levels
				if outerDefined {
					levels-- // the outer sequence counts as one level
				}
				_, tokens, err := readDeepNesting(deepNestingStream(levels, outerDefined), ReaderOptions{Dictionary: std.Dictionary, MaxSequenceDepth: test.depth})
				if !test.wantErr {
					if err != nil {
						t.Fatalf("unexpected error: %v", err)
					}
					if want := 4 * test.levels; tokens != want {
						t.Fatalf("tokens = %d, want %d", tokens, want)
					}
					return
				}
				if !errors.Is(err, ErrMaxDepthExceeded) {
					t.Fatalf("error = %v, want ErrMaxDepthExceeded", err)
				}
				var parseErr *ParseError
				if !errors.As(err, &parseErr) || parseErr.Op != OpCheckDepth {
					t.Fatalf("error = %#v, want a ParseError with Op %q", err, OpCheckDepth)
				}
			})
		}
	}
}

// TestNextRejectsHugeNestingEarly: the 100k-level input from #1020 (3.6 MB,
// 65 s before the fix) is refused by the default limit after reading only
// the frames up to it.
func TestNextRejectsHugeNestingEarly(t *testing.T) {
	data := deepNestingStream(100_000, false)
	reader, _, err := readDeepNesting(data, ReaderOptions{Dictionary: std.Dictionary})
	if !errors.Is(err, ErrMaxDepthExceeded) {
		t.Fatalf("error = %v, want ErrMaxDepthExceeded", err)
	}
	// 20 bytes per sequence plus item header pair: the reader stops at the
	// first frame beyond the limit.
	if limit := int64(DefaultMaxSequenceDepth/2+1) * 20; reader.Position() > limit {
		t.Fatalf("reader consumed %d of %d bytes before refusing, want at most %d", reader.Position(), len(data), limit)
	}
}

// TestNextDeepNestingScalesLinearly guards against per-header work that
// grows with the nesting depth. Before #1020 every header scanned the whole
// container stack, and under SequenceRecovery rendered its structural path,
// so parsing was quadratic in the depth. Sixteen times the depth must cost
// about sixteen times the CPU time and the allocated bytes of the shallow
// nest; quadratic parsing costs 256 times. Both are compared per level
// against a 4x bound, the geometric middle. CPU time (the fastest of five
// runs) instead of wall time keeps the comparison independent of how busy the
// machine is, and allocated bytes are exact.
func TestNextDeepNestingScalesLinearly(t *testing.T) {
	if testing.Short() {
		t.Skip("timing comparison skipped in -short mode")
	}
	const small, large = 1_000, 16_000
	const maxPerLevelRatio = 4
	variants := []struct {
		name         string
		outerDefined bool
		opts         ReaderOptions
	}{
		{name: "undefined lengths", opts: ReaderOptions{}},
		{name: "defined outer container", outerDefined: true, opts: ReaderOptions{}},
		{name: "recovery enabled", outerDefined: true, opts: ReaderOptions{
			Dictionary:                std.Dictionary,
			RecoverContainerOverruns:  true,
			RecoverDelimiterAnomalies: true,
			SequenceRecovery: SequenceRecoveryOptions{
				UndefinedLengthOBOWAsSQ: true, VOILUTSequenceOWAsSQ: true, ReplaceWrongDelimiter: true,
			},
		}},
	}
	for _, variant := range variants {
		t.Run(variant.name, func(t *testing.T) {
			opts := variant.opts
			opts.MaxSequenceDepth = UnlimitedSequenceDepth
			// measure returns the CPU time and the bytes allocated per level.
			measure := func(levels int) (float64, float64) {
				data := deepNestingStream(levels, variant.outerDefined)
				parse := func() {
					if _, _, err := readDeepNesting(data, opts); err != nil {
						t.Fatalf("levels %d: %v", levels, err)
					}
				}
				var before, after runtime.MemStats
				runtime.ReadMemStats(&before)
				parse()
				runtime.ReadMemStats(&after)
				best := time.Duration(math.MaxInt64)
				for run := 0; run < 5; run++ {
					runtime.GC()
					start := testutil.CPUTime()
					parse()
					best = min(best, testutil.CPUTime()-start)
				}
				return float64(best) / float64(levels), float64(after.TotalAlloc-before.TotalAlloc) / float64(levels)
			}
			smallCPU, smallBytes := measure(small)
			largeCPU, largeBytes := measure(large)
			t.Logf("per level: %.0f ns and %.0f bytes at %d levels, %.0f ns and %.0f bytes at %d levels",
				smallCPU, smallBytes, small, largeCPU, largeBytes, large)
			if largeBytes > maxPerLevelRatio*smallBytes {
				t.Errorf("allocation per level grew %.1fx from %d to %d levels: parsing allocates more than linearly in the depth",
					largeBytes/smallBytes, small, large)
			}
			if smallCPU > 0 && largeCPU > maxPerLevelRatio*smallCPU {
				t.Errorf("CPU time per level grew %.1fx from %d to %d levels: parsing is not linear in the depth",
					largeCPU/smallCPU, small, large)
			}
		})
	}
}

func TestInnermostItemOffsetFollowsCachedIndexes(t *testing.T) {
	// Sequence > item > sequence > item, then read a value inside: the
	// recorded ItemOffset is that of the innermost Item header.
	le := binary.LittleEndian
	stream := bytes.Join([][]byte{
		dicomtest.SequenceHeaderBytes(transfer.ExplicitVRLittleEndian, core.NewTag(0x0008, 0x1115), 0xFFFFFFFF),
		dicomtest.SequenceControlBytes(le, core.TagItem, 0xFFFFFFFF),
		dicomtest.SequenceHeaderBytes(transfer.ExplicitVRLittleEndian, core.NewTag(0x0008, 0x1140), 0xFFFFFFFF),
		dicomtest.SequenceControlBytes(le, core.TagItem, 0xFFFFFFFF),
		dicomtest.EncodeElement(dicomtest.NewPNElement(core.NewTag(0x0010, 0x0010), "NEST^TEST"), transfer.ExplicitVRLittleEndian),
		dicomtest.SequenceControlBytes(le, core.TagItemDelimitationItem, 0),
		dicomtest.SequenceControlBytes(le, core.TagSequenceDelimitationItem, 0),
		dicomtest.SequenceControlBytes(le, core.TagItemDelimitationItem, 0),
		dicomtest.SequenceControlBytes(le, core.TagSequenceDelimitationItem, 0),
	}, nil)
	reader := NewReader(bytes.NewReader(stream), transfer.ExplicitVRLittleEndian, ReaderOptions{Dictionary: std.Dictionary})
	for {
		tok, err := reader.Next()
		if err != nil {
			t.Fatalf("no element token: %v", err)
		}
		if tok.Kind != TokenElement {
			continue
		}
		offset, ok := reader.itemOffset()
		// Outer sequence (12) + item (8) + inner sequence (12) = 32.
		if !ok || offset != 32 {
			t.Fatalf("itemOffset = %d, %v; want 32, true", offset, ok)
		}
		if index, _, ok := reader.definedContainer(); ok {
			t.Fatalf("definedContainer = %d, want none for undefined lengths", index)
		}
		return
	}
}

func BenchmarkReaderNextDeepNesting(b *testing.B) {
	for _, levels := range []int{1_000, 10_000, 100_000} {
		data := deepNestingStream(levels, true)
		b.Run(fmt.Sprintf("Levels_%06d", levels), func(b *testing.B) {
			b.SetBytes(int64(len(data)))
			opts := ReaderOptions{MaxSequenceDepth: UnlimitedSequenceDepth}
			for i := 0; i < b.N; i++ {
				if _, _, err := readDeepNesting(data, opts); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(levels), "ns/level")
		})
	}
}
