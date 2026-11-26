package deid

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/builtin"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpeglossless"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpegls"
	"github.com/ThalesMMS/dicom-go/pixeldata/rle"
	"github.com/ThalesMMS/dicom-go/transfer"
)

const (
	redactionRows    = 20
	redactionColumns = 24
	redactionFrames  = 3
	redactionPatient = "SYNTHETIC^REDACTION"
	redactionPID     = "SYN-REDACT-7"
)

// redactionHeader covers the synthetic burned-in text band on every frame.
var redactionHeader = PixelRegion{Frame: PixelRegionAllFrames, X: 1, Y: 1, Width: 18, Height: 3}

// redactionTestFile returns a synthetic, non-PHI 12-bit multi-frame image in
// syntax whose top band holds bright text-like blocks, with Burned In
// Annotation YES, and its native frames.
func redactionTestFile(t *testing.T, syntax transfer.Syntax) (*object.File, [][]uint16) {
	t.Helper()
	us := func(g, e, v uint16) core.Element {
		return core.NewRawElement(core.NewTag(g, e), core.VRUS, binary.LittleEndian.AppendUint16(nil, v))
	}
	frames := make([][]uint16, redactionFrames)
	var raw []byte
	for f := range frames {
		frames[f] = make([]uint16, redactionRows*redactionColumns)
		for y := 0; y < redactionRows; y++ {
			for x := 0; x < redactionColumns; x++ {
				value := uint16(200 + 9*x + 13*y + 40*f)
				if y >= 1 && y < 4 && x >= 1 && x < 19 && (x-1)%3 != 2 {
					value = 4000
				}
				frames[f][y*redactionColumns+x] = value
				raw = binary.LittleEndian.AppendUint16(raw, value)
			}
		}
	}
	dataset := object.FromElements([]core.Element{
		core.NewRawElement(core.NewTag(0x0008, 0x0016), core.VRUI, []byte("1.2.840.10008.5.1.4.1.1.7\x00")),
		core.NewRawElement(core.NewTag(0x0008, 0x0018), core.VRUI, []byte("1.2.826.0.1.3680043.10.1.1218.9\x00")),
		core.NewRawElement(tagPatientName, core.VRPN, padText(redactionPatient)),
		core.NewRawElement(tagPatientID, core.VRLO, padText(redactionPID)),
		core.NewRawElement(core.NewTag(0x0020, 0x000d), core.VRUI, []byte("1.2.826.0.1.3680043.10.1.1218.1\x00")),
		core.NewRawElement(core.NewTag(0x0020, 0x000e), core.VRUI, []byte("1.2.826.0.1.3680043.10.1.1218.2\x00")),
		us(0x0028, 0x0002, 1),
		core.NewRawElement(core.NewTag(0x0028, 0x0004), core.VRCS, []byte("MONOCHROME2 ")),
		core.NewRawElement(core.NewTag(0x0028, 0x0008), core.VRIS, []byte("3 ")),
		us(0x0028, 0x0010, redactionRows),
		us(0x0028, 0x0011, redactionColumns),
		us(0x0028, 0x0100, 16), us(0x0028, 0x0101, 12), us(0x0028, 0x0102, 11), us(0x0028, 0x0103, 0),
		core.NewRawElement(tagBurnedInAnnotation, core.VRCS, []byte("YES ")),
		core.NewRawElement(tagRecognizableVisualFeatures, core.VRCS, []byte("NO")),
		core.NewRawElement(core.TagPixelData, core.VROW, raw),
	}, nil)
	if syntax.Encapsulated {
		encoded, _, err := pixeldata.TranscodeDataSet(context.Background(), dataset, transfer.ExplicitVRLittleEndian, syntax, pixeldata.TranscodeOptions{
			EncoderRegistry: redactionEncoders(t, syntax.UID == transfer.JPEGLSNearLossless.UID), AllowLossy: syntax.UID == transfer.JPEGLSNearLossless.UID,
		})
		if err != nil {
			t.Fatal(err)
		}
		dataset = encoded
	}
	var buffer bytes.Buffer
	if err := object.WriteFile(&buffer, &object.File{Dataset: dataset, TransferSyntax: syntax}); err != nil {
		t.Fatal(err)
	}
	file, err := object.ReadFile(bytes.NewReader(buffer.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	return file, frames
}

func redactionEncoders(t *testing.T, nearLossless bool) *pixeldata.MemoryEncoderRegistry {
	t.Helper()
	encoders := pixeldata.NewMemoryEncoderRegistry()
	for _, register := range []func(pixeldata.EncoderRegistry) error{rle.RegisterEncoder, jpegls.RegisterEncoder, jpeglossless.RegisterEncoder} {
		if err := register(encoders); err != nil {
			t.Fatal(err)
		}
	}
	if nearLossless {
		if err := jpegls.RegisterNearLosslessEncoder(encoders, jpegls.EncoderOptions{Near: 3, AllowLossy: true}); err != nil {
			t.Fatal(err)
		}
	}
	return encoders
}

func redactionDecoders(t *testing.T) pixeldata.Registry {
	t.Helper()
	decoders, err := builtin.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	return decoders
}

func newTestRedaction(t *testing.T, attested bool, regions ...PixelRegion) *PixelRedaction {
	t.Helper()
	redaction := &PixelRedaction{Regions: regions, DecoderRegistry: redactionDecoders(t), EncoderRegistry: redactionEncoders(t, true)}
	if attested {
		redaction.Attestation = AttestPixelRedaction(regions, 0)
	}
	return redaction
}

func fileFrames(t *testing.T, file *object.File) [][]uint16 {
	t.Helper()
	native := file.Dataset
	if file.TransferSyntax.Encapsulated {
		var err error
		native, _, err = pixeldata.DecompressDataSet(file.Dataset, file.TransferSyntax, pixeldata.DecompressOptions{Registry: redactionDecoders(t)})
		if err != nil {
			t.Fatal(err)
		}
	}
	frames, err := pixeldata.ExtractNativeFramesView(native)
	if err != nil {
		t.Fatal(err)
	}
	out := make([][]uint16, len(frames.Data))
	for f, frame := range frames.Data {
		out[f] = make([]uint16, len(frame)/2)
		for i := range out[f] {
			out[f][i] = binary.LittleEndian.Uint16(frame[2*i:])
		}
	}
	return out
}

// assertRedacted checks every pixel: inside regions it equals fill, outside
// it equals source.
func assertRedacted(t *testing.T, got, source [][]uint16, fill uint16, regions ...PixelRegion) {
	t.Helper()
	if len(got) != len(source) {
		t.Fatalf("frames = %d, want %d", len(got), len(source))
	}
	for f := range source {
		for i, want := range source[f] {
			x, y := i%redactionColumns, i/redactionColumns
			for _, region := range regions {
				if (region.Frame == PixelRegionAllFrames || region.Frame == f) && x >= region.X && x < region.X+region.Width && y >= region.Y && y < region.Y+region.Height {
					want = fill
				}
			}
			if got[f][i] != want {
				t.Fatalf("frame %d pixel (%d,%d) = %d, want %d", f, x, y, got[f][i], want)
			}
		}
	}
}

func methodCodes(t *testing.T, obj *object.Object) []string {
	t.Helper()
	element, ok := obj.Get(tagDeidentificationMethodCodeSequence)
	if !ok {
		t.Fatal("missing De-identification Method Code Sequence")
	}
	var codes []string
	for _, item := range element.Value.(core.SequenceValue).Items {
		for _, child := range item.Elements {
			if child.Tag() == tagCodeValue {
				raw, _ := child.RawBytes()
				codes = append(codes, strings.TrimSpace(string(raw)))
			}
		}
	}
	return codes
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestPixelRedactionAttestedOnEveryFrameClearsBurnedInAnnotation(t *testing.T) {
	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.RLELossless, transfer.JPEGLSLossless, transfer.JPEGLosslessSV1} {
		t.Run(syntax.Name, func(t *testing.T) {
			source, frames := redactionTestFile(t, syntax)
			var before bytes.Buffer
			if err := object.WriteFile(&before, source); err != nil {
				t.Fatal(err)
			}
			options := DefaultBasicProfileOptions()
			options.PixelRedaction = newTestRedaction(t, true, redactionHeader)
			clone, report, err := CloneFileWithBasicProfile(context.Background(), source, options, nil)
			if err != nil {
				t.Fatal(err)
			}
			if clone.TransferSyntax.UID != syntax.UID {
				t.Fatalf("output transfer syntax = %s, want lossless %s", clone.TransferSyntax.UID, syntax.UID)
			}
			assertRedacted(t, fileFrames(t, clone), frames, 0, redactionHeader)
			if value, _ := clone.Dataset.GetString(tagBurnedInAnnotation); strings.TrimSpace(value) != "NO" {
				t.Fatalf("Burned In Annotation = %q, want NO", value)
			}
			if codes := methodCodes(t, clone.Dataset); !containsString(codes, "113100") || !containsString(codes, "113101") {
				t.Fatalf("method codes = %v, want Basic Profile and Clean Pixel Data", codes)
			}
			if values, _ := clone.Dataset.GetStrings(tagDeidentificationMethod); !containsString(values, "Operator-attested pixel region redaction") {
				t.Fatalf("De-identification Method = %q", values)
			}
			if !report.Complete || report.PixelRedaction == nil || !report.PixelRedaction.AnnotationCleared || !report.PixelRedaction.Applied ||
				report.PixelRedaction.FramesCovered != redactionFrames || report.Summary.PixelMasks != redactionFrames {
				t.Fatalf("report = complete:%t risks:%v redaction:%+v", report.Complete, report.ResidualRisks, report.PixelRedaction)
			}
			if syntax.Encapsulated != report.PixelRedaction.Reencoded {
				t.Fatalf("Reencoded = %t", report.PixelRedaction.Reencoded)
			}
			var after bytes.Buffer
			if err := object.WriteFile(&after, source); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before.Bytes(), after.Bytes()) {
				t.Fatal("redaction mutated the source file")
			}
		})
	}
}

func TestPixelRedactionOfLossySourceStoresNative(t *testing.T) {
	source, _ := redactionTestFile(t, transfer.JPEGLSNearLossless)
	decodedSource := fileFrames(t, source)
	options := DefaultBasicProfileOptions()
	options.PixelRedaction = newTestRedaction(t, true, redactionHeader)
	clone, report, err := CloneFileWithBasicProfile(context.Background(), source, options, nil)
	if err != nil {
		t.Fatal(err)
	}
	if clone.TransferSyntax.UID != transfer.ExplicitVRLittleEndian.UID || !report.PixelRedaction.DecodedToNative {
		t.Fatalf("lossy source output = %s %+v, want native", clone.TransferSyntax.UID, report.PixelRedaction)
	}
	assertRedacted(t, fileFrames(t, clone), decodedSource, 0, redactionHeader)
	if meta, _ := clone.Meta.GetString(core.NewTag(0x0002, 0x0010)); strings.TrimRight(meta, "\x00 ") != transfer.ExplicitVRLittleEndian.UID {
		t.Fatalf("File Meta transfer syntax = %q", meta)
	}
}

func TestPixelRedactionWithoutAttestationKeepsBurnedInAnnotationAndResidualRisk(t *testing.T) {
	source, frames := redactionTestFile(t, transfer.ExplicitVRLittleEndian)
	options := DefaultBasicProfileOptions()
	options.PixelRedaction = newTestRedaction(t, false, redactionHeader)
	clone, report, err := CloneFileWithBasicProfile(context.Background(), source, options, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertRedacted(t, fileFrames(t, clone), frames, 0, redactionHeader)
	if value, _ := clone.Dataset.GetString(tagBurnedInAnnotation); strings.TrimSpace(value) != "YES" {
		t.Fatalf("unattested Burned In Annotation = %q, want source YES", value)
	}
	if containsString(methodCodes(t, clone.Dataset), "113101") {
		t.Fatal("unattested redaction recorded the Clean Pixel Data Option")
	}
	if values, _ := clone.Dataset.GetStrings(tagDeidentificationMethod); !containsString(values, "Unattested pixel region redaction") {
		t.Fatalf("De-identification Method = %q", values)
	}
	assertRisks(t, report, false, "pixel-redaction-not-attested", "pixel-cleaning-not-attested", "burned-in-annotation-not-cleared")
}

func TestPixelRedactionUncoveredFramesKeepIncompleteReport(t *testing.T) {
	source, frames := redactionTestFile(t, transfer.RLELossless)
	region := PixelRegion{Frame: 1, X: 1, Y: 1, Width: 18, Height: 3}
	options := DefaultBasicProfileOptions()
	options.PixelRedaction = newTestRedaction(t, true, region)
	clone, report, err := CloneFileWithBasicProfile(context.Background(), source, options, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertRedacted(t, fileFrames(t, clone), frames, 0, region)
	if value, _ := clone.Dataset.GetString(tagBurnedInAnnotation); strings.TrimSpace(value) != "YES" {
		t.Fatalf("partially covered Burned In Annotation = %q, want YES", value)
	}
	if containsString(methodCodes(t, clone.Dataset), "113101") {
		t.Fatal("partial redaction recorded the Clean Pixel Data Option")
	}
	if report.PixelRedaction.FramesCovered != 1 || report.PixelRedaction.AnnotationCleared {
		t.Fatalf("redaction report = %+v", report.PixelRedaction)
	}
	assertRisks(t, report, false, "pixel-redaction-frames-not-covered", "burned-in-annotation-not-cleared")
}

func assertRisks(t *testing.T, report ProfileReport, complete bool, codes ...string) {
	t.Helper()
	if report.Complete != complete {
		t.Fatalf("Complete = %t, want %t (risks %v)", report.Complete, complete, report.ResidualRisks)
	}
	for _, code := range codes {
		found := false
		for _, risk := range report.ResidualRisks {
			found = found || risk.Code == code
		}
		if !found {
			t.Fatalf("missing residual risk %q in %v", code, report.ResidualRisks)
		}
	}
}

func TestPixelRedactionAttestationGates(t *testing.T) {
	source, _ := redactionTestFile(t, transfer.ExplicitVRLittleEndian)
	stale := newTestRedaction(t, true, redactionHeader)
	stale.Regions = []PixelRegion{{Frame: PixelRegionAllFrames, X: 1, Y: 1, Width: 10, Height: 3}}
	options := DefaultBasicProfileOptions()
	options.PixelRedaction = stale
	if _, _, err := CloneFileWithBasicProfile(context.Background(), source, options, nil); !errors.Is(err, ErrStalePixelRedactionAttestation) {
		t.Fatalf("stale attestation error = %v", err)
	}
	staleFill := newTestRedaction(t, true, redactionHeader)
	staleFill.Fill = 1
	options.PixelRedaction = staleFill
	if _, _, err := CloneFileWithBasicProfile(context.Background(), source, options, nil); !errors.Is(err, ErrStalePixelRedactionAttestation) {
		t.Fatalf("stale fill attestation error = %v", err)
	}
	options.PixelRedaction = newTestRedaction(t, true, redactionHeader)
	options.SelectedOptions = []ProfileOption{ProfileOptionCleanPixelData}
	options.PixelCleaner = func(context.Context, *object.Object) ([]PixelRegion, error) { return nil, nil }
	if _, _, err := CloneFileWithBasicProfile(context.Background(), source, options, nil); !errors.Is(err, ErrInvalidProfileOptions) {
		t.Fatalf("redaction plus pixel cleaner error = %v", err)
	}
	if PixelRedactionDigest([]PixelRegion{{X: 1}, {X: 2}}, 0) != PixelRedactionDigest([]PixelRegion{{X: 2}, {X: 1}}, 0) {
		t.Fatal("attestation digest depends on region order")
	}
}

func TestPixelRedactionRejectsRegionsOutsideTheImageWithoutValues(t *testing.T) {
	source, _ := redactionTestFile(t, transfer.ExplicitVRLittleEndian)
	options := DefaultBasicProfileOptions()
	options.PixelRedaction = newTestRedaction(t, true, PixelRegion{Frame: PixelRegionAllFrames, X: 20, Y: 0, Width: 5, Height: 1})
	_, _, err := CloneFileWithBasicProfile(context.Background(), source, options, nil)
	if !errors.Is(err, ErrInvalidPixelRedaction) {
		t.Fatalf("error = %v, want ErrInvalidPixelRedaction", err)
	}
	if strings.Contains(err.Error(), "SYNTHETIC") || strings.Contains(err.Error(), redactionPID) {
		t.Fatalf("error leaks identifiers: %v", err)
	}
}

func TestPixelRedactionDryRunOverDeferredPixelsIsPreviewOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "source.dcm")
	source, _ := redactionTestFile(t, transfer.RLELossless)
	var buffer bytes.Buffer
	if err := object.WriteFile(&buffer, source); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buffer.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	deferred, err := object.OpenFileWithOptions(path, object.ReadFileOptions{DeferPixelData: true})
	if err != nil {
		t.Fatal(err)
	}
	defer deferred.Close()
	options := DefaultBasicProfileOptions()
	options.PixelRedaction = newTestRedaction(t, true, redactionHeader)
	plan, err := PlanBasicProfile(context.Background(), deferred.Dataset, options, nil)
	if err != nil {
		t.Fatal(err)
	}
	report := plan.Report()
	if !report.Complete || report.PixelRedaction == nil || report.PixelRedaction.Applied || !report.PixelRedaction.AnnotationCleared {
		t.Fatalf("preview report = complete:%t %+v", report.Complete, report.PixelRedaction)
	}
	if _, err := plan.Apply(context.Background(), deferred.Dataset); !errors.Is(err, ErrDeferredValueUnavailable) {
		t.Fatalf("preview Apply error = %v, want ErrDeferredValueUnavailable", err)
	}

	// The Object-level API cannot change a transfer syntax, so loaded
	// encapsulated pixels must go through the file APIs.
	if _, err := ApplyBasicProfile(context.Background(), source.Dataset, options, nil); !errors.Is(err, ErrPixelRedactionRequiresFile) {
		t.Fatalf("object-level encapsulated redaction error = %v", err)
	}

	destination := filepath.Join(dir, "redacted.dcm")
	pathReport, err := BasicProfilePath(context.Background(), path, destination, options, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !pathReport.PixelRedaction.Applied || !pathReport.PixelRedaction.Reencoded {
		t.Fatalf("path report = %+v", pathReport.PixelRedaction)
	}
	written, err := object.OpenFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer written.Close()
	if written.TransferSyntax.UID != transfer.RLELossless.UID {
		t.Fatalf("path output transfer syntax = %s", written.TransferSyntax.UID)
	}
	_, frames := redactionTestFile(t, transfer.ExplicitVRLittleEndian)
	assertRedacted(t, fileFrames(t, written), frames, 0, redactionHeader)
}

func TestPixelRedactionNativeObjectLevelApply(t *testing.T) {
	source, frames := redactionTestFile(t, transfer.ExplicitVRLittleEndian)
	options := DefaultBasicProfileOptions()
	options.PixelRedaction = newTestRedaction(t, true, redactionHeader)
	report, err := ApplyBasicProfile(context.Background(), source.Dataset, options, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !report.PixelRedaction.Applied {
		t.Fatalf("report = %+v", report.PixelRedaction)
	}
	assertRedacted(t, fileFrames(t, source), frames, 0, redactionHeader)
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("SYNTHETIC")) || bytes.Contains(encoded, []byte(redactionPID)) {
		t.Fatalf("report leaks identifiers: %s", encoded)
	}
}
