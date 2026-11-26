package jpeg2000

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/examples/codec-adapters/jpeg2000/internal/j2kcore"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/codecfixture"
	"github.com/ThalesMMS/dicom-go/transfer"
)

const libraryRoot = "../../.."

// coreQualificationReport is the reproducible record written when
// DICOM_GO_J2K_CORE_REPORT names a file. It holds sizes, positions and
// metrics, never sample values.
type coreQualificationReport struct {
	Backend    string                       `json:"backend"`
	Platform   string                       `json:"platform"`
	Go         string                       `json:"go"`
	Fixtures   []coreQualificationResult    `json:"fixtures"`
	Exclusions []coreQualificationExclusion `json:"exclusions"`
	Totals     struct {
		Fixtures int    `json:"fixtures"`
		Passed   int    `json:"passed"`
		Samples  uint64 `json:"samples"`
	} `json:"totals"`
}

type coreQualificationResult struct {
	ID         string                         `json:"id"`
	Oracle     string                         `json:"oracle"`
	Policy     string                         `json:"policy"`
	Report     *codecfixture.ComparisonReport `json:"report,omitempty"`
	Source     *codecfixture.ComparisonReport `json:"source,omitempty"`
	Refusal    string                         `json:"refusal,omitempty"`
	Passed     bool                           `json:"passed"`
	FailReason string                         `json:"fail_reason,omitempty"`
}

type coreQualificationExclusion struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

func corpusMetadata(input codecfixture.CorpusInput) pixeldata.Metadata {
	return pixeldata.Metadata{
		Rows: uint16(input.Rows), Columns: uint16(input.Columns), SamplesPerPixel: uint16(input.Components),
		BitsAllocated: uint16(input.BitsAllocated), BitsStored: uint16(input.BitsStored), HighBit: uint16(input.HighBit),
		PixelRepresentation: boolUint16(input.Signed), PhotometricInterpretation: input.Photometric,
		PlanarConfiguration: uint16(input.PlanarConfiguration), PlanarConfigurationPresent: input.Components > 1,
		NumberOfFrames: input.Frames,
	}
}

func boolUint16(v bool) uint16 {
	if v {
		return 1
	}
	return 0
}

func corpusLayout(input codecfixture.CorpusInput) codecfixture.SampleLayout {
	return codecfixture.SampleLayout{Rows: input.Rows, Columns: input.Columns, Components: input.Components, BitsAllocated: input.BitsAllocated, BitsStored: input.BitsStored, HighBit: input.HighBit, Signed: input.Signed}
}

// decodeCorpusFixture decodes a bare codestream or a DICOM file with the core
// backend.
func decodeCorpusFixture(t *testing.T, fixture codecfixture.CorpusFixture, options CoreOptions) ([][]byte, error) {
	t.Helper()
	path, err := codecfixture.CorpusFilePath(libraryRoot, fixture.Path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasSuffix(strings.ToLower(fixture.Path), ".dcm") {
		file, err := object.OpenFile(path)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		pixel, err := pixeldata.Extract(file.Dataset)
		if err != nil {
			t.Fatal(err)
		}
		frames, err := NewCore(options).DecodeContext(context.Background(), pixel, file.Dataset)
		return frames.Data, err
	}
	codestream, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := NewCoreDecoder(options).(ContextDecoder).DecodeFrameContext(context.Background(), codestream, corpusMetadata(fixture.Input))
	if err != nil {
		return nil, err
	}
	return [][]byte{frame}, nil
}

func referenceFrames(t testing.TB, fixture codecfixture.CorpusFixture) ([][]byte, string) {
	t.Helper()
	if fixture.Reconstruction != nil {
		_, frames, err := codecfixture.ReadFullReconstruction(libraryRoot, fixture.ID)
		if err != nil {
			t.Fatal(err)
		}
		return frames, fixture.Reconstruction.Backend + " " + fixture.Reconstruction.Version
	}
	path, err := codecfixture.CorpusFilePath(libraryRoot, fixture.ReferencePath)
	if err != nil {
		t.Fatal(err)
	}
	file, err := object.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	native, err := pixeldata.ExtractNativeFrames(file.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	return native.Data, "independently encoded native twin " + fixture.ReferencePath
}

// TestCoreQualificationCorpus runs every JPEG 2000 Part 1 corpus fixture
// through the core backend. Lossless fixtures must equal their oracle and, when
// recorded, their source samples exactly; lossy fixtures must meet their
// per-fixture policy against an independent decoder; refusal fixtures must be
// refused by name. Part 2 and HTJ2K fixtures are listed as exclusions.
func TestCoreQualificationCorpus(t *testing.T) {
	fixtures, err := codecfixture.CorpusFixtures(libraryRoot)
	if err != nil {
		t.Fatal(err)
	}
	var report coreQualificationReport
	report.Backend = "examples/codec-adapters/jpeg2000 core (amrshadid/go-dicom v1.6.0 adapted)"
	report.Platform = runtime.GOOS + "/" + runtime.GOARCH
	report.Go = runtime.Version()
	for _, fixture := range fixtures {
		switch fixture.Family {
		case "jpeg-2000-part-2", "htj2k":
			report.Exclusions = append(report.Exclusions, coreQualificationExclusion{fixture.ID, "not implemented by the core backend; stays with OpenJPEG/OpenJPH"})
			continue
		case "jpeg-2000-part-1":
		default:
			continue
		}
		result := coreQualificationResult{ID: fixture.ID}
		frames, decodeErr := decodeCorpusFixture(t, fixture, CoreOptions{})
		switch fixture.Comparison {
		case "expected-rejection":
			result.Policy = "refusal by name"
			result.Passed = errors.Is(decodeErr, ErrUnsupportedCodestreamFeature)
			if decodeErr != nil {
				result.Refusal = decodeErr.Error()
			}
			if !result.Passed {
				result.FailReason = fmt.Sprintf("want ErrUnsupportedCodestreamFeature, got %v", decodeErr)
			}
		default:
			if decodeErr != nil {
				result.FailReason = decodeErr.Error()
				break
			}
			reference, oracle := referenceFrames(t, fixture)
			result.Oracle = oracle
			policy := codecfixture.SamplePolicy{}
			result.Policy = "exact"
			if fixture.Lossy {
				if fixture.SamplePolicy == nil {
					result.FailReason = "lossy fixture without a sample policy"
					break
				}
				policy = *fixture.SamplePolicy
				result.Policy = policy.Rationale
			}
			layout := corpusLayout(fixture.Input)
			comparison, err := codecfixture.CompareSamples(frames, layout, reference, layout, policy)
			if err != nil {
				result.FailReason = err.Error()
				break
			}
			result.Report = &comparison
			result.Passed = comparison.Qualified
			if !comparison.Qualified {
				result.FailReason = comparison.Reason
			}
			if fixture.SourceSamplesPath != "" {
				source, err := codecfixture.ReadSourceSamples(libraryRoot, fixture.ID)
				if err != nil {
					t.Fatal(err)
				}
				exact, err := codecfixture.CompareSamples(frames, layout, source, layout, codecfixture.SamplePolicy{})
				if err != nil {
					t.Fatal(err)
				}
				result.Source = &exact
				if !exact.Qualified {
					result.Passed = false
					result.FailReason = "differs from source samples: " + exact.Reason
				}
			}
			report.Totals.Samples += comparison.Samples
		}
		report.Totals.Fixtures++
		if result.Passed {
			report.Totals.Passed++
		}
		report.Fixtures = append(report.Fixtures, result)
		if !result.Passed {
			first := ""
			if result.Report != nil && result.Report.First != nil {
				first = fmt.Sprintf(" first deviation frame %d row %d column %d component %d", result.Report.First.Frame, result.Report.First.Row, result.Report.First.Column, result.Report.First.Component)
			}
			t.Errorf("%s: %s%s", fixture.ID, result.FailReason, first)
		}
	}
	if report.Totals.Fixtures < 25 {
		t.Fatalf("only %d JPEG 2000 Part 1 fixtures ran", report.Totals.Fixtures)
	}
	if path := os.Getenv("DICOM_GO_J2K_CORE_REPORT"); path != "" {
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// The synthetic equivalent of J2K_pixelrep_mismatch.dcm: an unsigned 12-bit
// codestream in a data set that declares signed pixels. The #933 policy
// refuses it by default and, on request, reads two's complement of BitsStored.
func TestCorePixelRepresentationMismatchCorpus(t *testing.T) {
	fixtures, err := codecfixture.CorpusFixtures(libraryRoot)
	if err != nil {
		t.Fatal(err)
	}
	var fixture codecfixture.CorpusFixture
	for _, f := range fixtures {
		if f.ID == "jpeg2000-core-u12-tiles" {
			fixture = f
		}
	}
	signed := fixture
	signed.Input.Signed = true
	if _, err := decodeCorpusFixture(t, signed, CoreOptions{}); !errors.Is(err, ErrSignednessMismatch) {
		t.Fatalf("default policy: %v", err)
	}
	frames, err := decodeCorpusFixture(t, signed, CoreOptions{SignednessMismatch: ReinterpretUnsignedAsSigned})
	if err != nil {
		t.Fatal(err)
	}
	source, err := codecfixture.ReadSourceSamples(libraryRoot, fixture.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(source[0]); i += 2 {
		v := int(source[0][i]) | int(source[0][i+1])<<8
		if v >= 1<<11 {
			v -= 1 << 12
		}
		got := int(int16(uint16(frames[0][i]) | uint16(frames[0][i+1])<<8))
		if got != v {
			t.Fatalf("sample %d = %d, want %d", i/2, got, v)
		}
	}
}

// Multi-frame, multi-Item layouts built from corpus codestreams: two Items per
// frame with a populated Basic Offset Table, and the same frames without it.
func TestCoreMultiFrameCorpusLayouts(t *testing.T) {
	fixtures, err := codecfixture.CorpusFixtures(libraryRoot)
	if err != nil {
		t.Fatal(err)
	}
	var fixture codecfixture.CorpusFixture
	for _, f := range fixtures {
		if f.ID == "jpeg2000-core-s16-extremes" {
			fixture = f
		}
	}
	path, err := codecfixture.CorpusFilePath(libraryRoot, fixture.Path)
	if err != nil {
		t.Fatal(err)
	}
	codestream, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	source, err := codecfixture.ReadSourceSamples(libraryRoot, fixture.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(codestream)%2 != 0 {
		codestream = append(codestream, 0)
	}
	split := len(codestream) / 2 &^ 1
	a, b := codestream[:split], codestream[split:]
	bot := make([]byte, 12)
	for i := 1; i < 3; i++ {
		offset := uint32(i * (8 + len(a) + 8 + len(b)))
		bot[i*4], bot[i*4+1], bot[i*4+2], bot[i*4+3] = byte(offset), byte(offset>>8), byte(offset>>16), byte(offset>>24)
	}
	for _, offsets := range [][]byte{bot, nil} {
		metadata := corpusMetadata(fixture.Input)
		elements := []core.Element{
			{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0002), VR: core.VRUS}, Value: core.Uint16Value{metadata.SamplesPerPixel}},
			{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0004), VR: core.VRCS}, Value: core.StringValue{metadata.PhotometricInterpretation}},
			{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0008), VR: core.VRIS}, Value: core.StringValue{"3"}},
			{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0010), VR: core.VRUS}, Value: core.Uint16Value{metadata.Rows}},
			{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0011), VR: core.VRUS}, Value: core.Uint16Value{metadata.Columns}},
			{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0100), VR: core.VRUS}, Value: core.Uint16Value{metadata.BitsAllocated}},
			{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0101), VR: core.VRUS}, Value: core.Uint16Value{metadata.BitsStored}},
			{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0102), VR: core.VRUS}, Value: core.Uint16Value{metadata.HighBit}},
			{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0103), VR: core.VRUS}, Value: core.Uint16Value{metadata.PixelRepresentation}},
			{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB, Length: core.UndefinedLength, LengthSet: true}, Value: core.FragmentSequence{OffsetTable: offsets, Fragments: [][]byte{a, b, a, b, a, b}}},
		}
		dataset := object.FromElements(elements, nil)
		pixel, err := pixeldata.Extract(dataset)
		if err != nil {
			t.Fatal(err)
		}
		frames, err := NewCore(CoreOptions{}).DecodeContext(context.Background(), pixel, dataset)
		if err != nil {
			t.Fatalf("BOT %d bytes: %v", len(offsets), err)
		}
		layout := corpusLayout(fixture.Input)
		layout.Signed = true
		report, err := codecfixture.CompareSamples(frames.Data, layout, [][]byte{source[0], source[0], source[0]}, layout, codecfixture.SamplePolicy{})
		if err != nil || !report.Qualified {
			t.Fatalf("BOT %d bytes: %+v %v", len(offsets), report, err)
		}
	}
	_ = transfer.JPEG2000LosslessOnly
}

// corpusCodestreams returns the bare Part 1 codestreams the core decodes.
func corpusCodestreams(t *testing.T) []codecfixture.CorpusFixture {
	t.Helper()
	fixtures, err := codecfixture.CorpusFixtures(libraryRoot)
	if err != nil {
		t.Fatal(err)
	}
	var out []codecfixture.CorpusFixture
	for _, f := range fixtures {
		if f.Family == "jpeg-2000-part-1" && strings.HasSuffix(f.Path, ".j2k") && f.Comparison != "expected-rejection" {
			out = append(out, f)
		}
	}
	if len(out) < 11 {
		t.Fatalf("only %d core codestreams", len(out))
	}
	return out
}

// Every corpus codestream cut short anywhere after the main header is refused
// as malformed; nothing is published and nothing panics.
func TestCoreRefusesTruncatedCorpusCodestreams(t *testing.T) {
	for _, fixture := range corpusCodestreams(t) {
		path, err := codecfixture.CorpusFilePath(libraryRoot, fixture.Path)
		if err != nil {
			t.Fatal(err)
		}
		codestream, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		decoder := NewCoreDecoder(CoreOptions{}).(ContextDecoder)
		metadata := corpusMetadata(fixture.Input)
		for _, cut := range []int{2, len(codestream) / 8, len(codestream) / 3, len(codestream) / 2, len(codestream) - 3} {
			frame, err := decoder.DecodeFrameContext(context.Background(), codestream[:cut], metadata)
			if !errors.Is(err, ErrMalformedCodestream) || frame != nil {
				t.Fatalf("%s cut at %d/%d: %v (%d bytes)", fixture.ID, cut, len(codestream), err, len(frame))
			}
		}
	}
}

// Working memory is measured, not only the output size: every byte the core
// allocates while decoding a corpus codestream stays within a fixed multiple
// of the full-image work estimate (8 bytes per sample and component) plus
// the packed output. Measured totals are 4.78x to 9.70x that sum
// (see PRODUCTION_PROFILE.md), so callers size
// MaxTileWorkBytes knowing it is a lower bound, not the whole footprint.
func TestCoreWorkingMemoryTracksTileBudget(t *testing.T) {
	const factor = 10
	for _, fixture := range corpusCodestreams(t) {
		path, err := codecfixture.CorpusFilePath(libraryRoot, fixture.Path)
		if err != nil {
			t.Fatal(err)
		}
		codestream, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		in := fixture.Input
		output := int64(in.Rows * in.Columns * in.Components * in.BitsAllocated / 8)
		work := int64(in.Rows*in.Columns*in.Components) * 8
		stream, err := j2kcore.ParseCodestream(codestream)
		if err != nil {
			t.Fatal(err)
		}
		var tileWork int64
		for tile := 0; tile < stream.NumTiles(); tile++ {
			x0, y0, x1, y1 := stream.TileBounds(tile)
			tileWork = max(tileWork, int64((x1-x0)*(y1-y0)*in.Components)*8)
		}
		decoder := NewCoreDecoder(CoreOptions{}).(ContextDecoder)
		metadata := corpusMetadata(fixture.Input)
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		if _, err := decoder.DecodeFrameContext(context.Background(), codestream, metadata); err != nil {
			t.Fatal(err)
		}
		runtime.ReadMemStats(&after)
		allocated := int64(after.TotalAlloc - before.TotalAlloc)
		bound := factor*(work+output) + int64(len(codestream)) + 64<<10
		t.Logf("%s: %d bytes allocated, full-image work %d, output %d (%.2fx)", fixture.ID, allocated, work, output, float64(allocated)/float64(work+output))
		if allocated > bound {
			t.Errorf("%s allocated %d bytes, above %d", fixture.ID, allocated, bound)
		}
		// Refusal happens before that work: a budget one byte short of the
		// largest tile's charge admits nothing.
		tight := NewCoreDecoder(CoreOptions{Limits: CoreLimits{MaxTileWorkBytes: tileWork - 1}}).(ContextDecoder)
		runtime.GC()
		runtime.ReadMemStats(&before)
		if _, err := tight.DecodeFrameContext(context.Background(), codestream, metadata); !errors.Is(err, ErrResourceLimit) {
			t.Fatalf("%s with a short budget: %v", fixture.ID, err)
		}
		runtime.ReadMemStats(&after)
		if refused := int64(after.TotalAlloc - before.TotalAlloc); refused > int64(len(codestream))+64<<10 {
			t.Errorf("%s allocated %d bytes before refusing", fixture.ID, refused)
		}
	}
}

// The core's output is consumed by pixeldata.StoredSamples with the data
// set's effective format and is interpreted exactly once: it equals the
// source samples read the same way, for signed and unsigned precisions. For
// the conflicting stream/data set case, the #933 policy decides first and the
// interpreted samples are the requested two's complement, never a blind
// reinterpretation of the codec output by the original header.
func TestCoreOutputFeedsStoredSamples(t *testing.T) {
	fixtures, err := codecfixture.CorpusFixtures(libraryRoot)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]codecfixture.CorpusFixture{}
	for _, f := range fixtures {
		byID[f.ID] = f
	}
	ctx := context.Background()
	for _, id := range []string{"jpeg2000-core-s12", "jpeg2000-core-s16-extremes", "jpeg2000-core-u12-tiles", "jpeg2000-core-u16-high-values"} {
		fixture := byID[id]
		frames, err := decodeCorpusFixture(t, fixture, CoreOptions{})
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		format := corpusMetadata(fixture.Input).SampleFormat(nil)
		got, err := pixeldata.StoredSamples[int32](ctx, frames[0], format, 0)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		source, err := codecfixture.ReadSourceSamples(libraryRoot, id)
		if err != nil {
			t.Fatal(err)
		}
		want, err := pixeldata.StoredSamples[int32](ctx, source[0], format, 0)
		if err != nil {
			t.Fatal(err)
		}
		stats, err := pixeldata.ComputeStoredSampleStatistics(ctx, frames[0], format, nil)
		if err != nil {
			t.Fatal(err)
		}
		lo, hi, _ := format.Range()
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s: sample %d = %d, want %d", id, i, got[i], want[i])
			}
		}
		if stats.Minimum < lo || stats.Maximum > hi || stats.Samples != int64(len(want)) {
			t.Fatalf("%s: statistics %+v outside %d..%d", id, stats, lo, hi)
		}
		if strings.Contains(id, "extremes") && (stats.Minimum != lo || stats.Maximum != hi) {
			t.Fatalf("%s: extremes %d..%d, want %d..%d", id, stats.Minimum, stats.Maximum, lo, hi)
		}
	}

	mismatch := byID["jpeg2000-core-u12-tiles"]
	mismatch.Input.Signed = true
	frames, err := decodeCorpusFixture(t, mismatch, CoreOptions{SignednessMismatch: ReinterpretUnsignedAsSigned})
	if err != nil {
		t.Fatal(err)
	}
	got, err := pixeldata.StoredSamples[int16](ctx, frames[0], corpusMetadata(mismatch.Input).SampleFormat(nil), 0)
	if err != nil {
		t.Fatal(err)
	}
	source, err := codecfixture.ReadSourceSamples(libraryRoot, mismatch.ID)
	if err != nil {
		t.Fatal(err)
	}
	unsigned12 := corpusMetadata(byID[mismatch.ID].Input).SampleFormat(nil)
	raw, err := pixeldata.StoredSamples[int16](ctx, source[0], unsigned12, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range raw {
		want := v
		if want >= 1<<11 {
			want -= 1 << 12
		}
		if got[i] != want {
			t.Fatalf("mismatch sample %d = %d, want %d", i, got[i], want)
		}
	}
}
