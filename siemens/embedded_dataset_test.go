package siemens

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/parser"
)

// An independent EVRLE macro fixture, not emitted by the DICOM/CSA writers.
// 20-byte directionality + 16-byte b-value + 32-byte orientation = 68 bytes.
func embeddedDiffusionWire() []byte {
	out := []byte{0x18, 0, 0x75, 0x90, 'C', 'S', 12, 0}
	out = append(out, "DIRECTIONAL "...)
	out = append(out, 0x18, 0, 0x87, 0x90, 'F', 'D', 8, 0)
	out = binary.LittleEndian.AppendUint64(out, math.Float64bits(1200))
	out = append(out, 0x18, 0, 0x89, 0x90, 'F', 'D', 24, 0)
	for _, value := range []float64{0.6, -0.8, 0} {
		out = binary.LittleEndian.AppendUint64(out, math.Float64bits(value))
	}
	return out
}

func embeddedTestHeader(name, encoded string) *CSAHeader {
	return &CSAHeader{Format: CSAFormatSV10, Elements: []CSAElement{{Name: name, Values: []string{encoded}}}}
}

func TestEmbeddedDatasetsKnownFieldsAndExactLimits(t *testing.T) {
	wire := embeddedDiffusionWire()
	if len(wire) != 68 {
		t.Fatal("independent fixture length changed")
	}
	encoded := base64.StdEncoding.EncodeToString(wire)
	for _, name := range []string{"MRDiffusion", "MR_ASL", "FmriAcquisitionDescriptionSequence", "FmriConditionsDataSequence", "FmriResultSequence"} {
		t.Run(name, func(t *testing.T) {
			// The literal CSA writer is separate from EncodeCSA and new readers.
			blob := buildCSA(true, []testElement{{name: name, vm: 1, vr: "OB", items: []string{encoded + "\x00"}}})
			header, err := ParseCSA(blob)
			if err != nil {
				t.Fatal(err)
			}
			dataset, err := header.EmbeddedDatasetContext(nil, name, EmbeddedDatasetLimits{
				MaxEncodedBytes: len(encoded), MaxDecodedBytes: len(wire),
				MaxTotalEncodedBytes: len(encoded), MaxTotalDecodedBytes: len(wire),
				MaxDatasets: 1, MaxHeaderElements: 1, MaxElements: 3,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer dataset.Close()
			if b, err := dataset.GetFloats(core.NewTag(0x0018, 0x9087)); err != nil || len(b) != 1 || b[0] != 1200 {
				t.Fatalf("b-value = %v, %v", b, err)
			}
			if vector, err := dataset.GetFloats(core.NewTag(0x0018, 0x9089)); err != nil || len(vector) != 3 || vector[0] != 0.6 || vector[1] != -0.8 || vector[2] != 0 {
				t.Fatalf("vector = %v, %v", vector, err)
			}
			// No borrowed CSA/source data or cached mutable output is exposed.
			for i := range blob {
				blob[i] = 0
			}
			dataset.SetFD(core.NewTag(0x0018, 0x9087), 7)
			again, err := header.EmbeddedDatasetContext(nil, name, EmbeddedDatasetLimits{})
			if err != nil {
				t.Fatal(err)
			}
			defer again.Close()
			if b, _ := again.GetFloats(core.NewTag(0x0018, 0x9087)); b[0] != 1200 {
				t.Fatal("output mutation changed input/next decode")
			}
		})
	}
}

func TestEmbeddedDatasetsOrderEmptyAndAmbiguity(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString(embeddedDiffusionWire())
	header := &CSAHeader{Elements: []CSAElement{
		{Name: "ArbitraryText", Values: []string{"invalid secret"}},
		{Name: "MR_ASL", Values: []string{"", encoded}},
		{Name: "MRDiffusion", Values: []string{encoded, encoded}},
		{Name: "MR_ASL", Values: []string{encoded}},
	}}
	list, err := header.EmbeddedDatasetsContext(nil, EmbeddedDatasetLimits{})
	if err != nil || len(list) != 4 {
		t.Fatalf("list = %d, %v", len(list), err)
	}
	for i, pair := range [][2]int{{1, 1}, {2, 0}, {2, 1}, {3, 0}} {
		defer list[i].Dataset.Close()
		if list[i].ElementIndex != pair[0] || list[i].ValueIndex != pair[1] {
			t.Fatalf("item %d location = %+v", i, list[i])
		}
	}
	for _, name := range []string{"MR_ASL", "MRDiffusion"} {
		if dataset, err := header.EmbeddedDatasetContext(nil, name, EmbeddedDatasetLimits{}); dataset != nil || !errors.Is(err, ErrAmbiguousEmbeddedDataset) {
			t.Fatalf("ambiguous %s: %v", name, err)
		}
	}
	for _, header := range []*CSAHeader{nil, {}, {Elements: []CSAElement{{Name: "MRDiffusion", Values: []string{""}}}}} {
		if _, err := header.EmbeddedDatasetsContext(nil, EmbeddedDatasetLimits{}); !errors.Is(err, ErrNoEmbeddedDataset) {
			t.Fatalf("empty = %v", err)
		}
	}
	if _, err := header.EmbeddedDatasetContext(nil, "FmriUnknown", EmbeddedDatasetLimits{}); !errors.Is(err, ErrNoEmbeddedDataset) {
		t.Fatalf("unknown field = %v", err)
	}
}

func TestEmbeddedBase64StrictAndLineBreaks(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString(embeddedDiffusionWire())
	withLines := encoded[:8] + "\r\n" + encoded[8:25] + "\n" + encoded[25:]
	obj, err := embeddedTestHeader("MRDiffusion", withLines).EmbeddedDatasetContext(nil, "MRDiffusion", EmbeddedDatasetLimits{})
	if err != nil {
		t.Fatal(err)
	}
	_ = obj.Close()
	// Last quartet AAA= decodes 2 bytes; changing only unused trailing bits
	// to AAB= must be rejected rather than tolerated by a lenient decoder.
	noncanonical := encoded[:len(encoded)-2] + "B="
	for name, value := range map[string]string{
		"space": encoded[:8] + " " + encoded[8:], "tab": encoded[:8] + "\t" + encoded[8:],
		"raw": strings.TrimRight(encoded, "="), "padding": encoded + "=", "suffix": encoded + "AAAA",
		"url alphabet": "____", "noncanonical bits": noncanonical, "line breaks only": "\r\n",
		"nul": encoded + "\x00", "contents": "sensitive-value-must-not-appear",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := embeddedTestHeader("MRDiffusion", value).EmbeddedDatasetContext(nil, "MRDiffusion", EmbeddedDatasetLimits{})
			if !errors.Is(err, ErrInvalidEmbeddedDataset) || strings.Contains(err.Error(), value) {
				t.Fatalf("unredacted/incorrect rejection: %v", err)
			}
		})
	}
}

func TestEmbeddedDatasetsAdmissionLimitsAndNoPartial(t *testing.T) {
	wire := embeddedDiffusionWire()
	encoded := base64.StdEncoding.EncodeToString(wire)
	header := embeddedTestHeader("MRDiffusion", encoded)
	for name, limits := range map[string]EmbeddedDatasetLimits{
		"encoded": {MaxEncodedBytes: len(encoded) - 1}, "decoded": {MaxDecodedBytes: len(wire) - 1},
		"total encoded": {MaxTotalEncodedBytes: len(encoded) - 1}, "total decoded": {MaxTotalDecodedBytes: len(wire) - 1},
		"elements": {MaxElements: 2}, "negative": {MaxDecodedBytes: -1},
	} {
		t.Run(name, func(t *testing.T) {
			if list, err := header.EmbeddedDatasetsContext(nil, limits); list != nil || !errors.Is(err, ErrEmbeddedDatasetLimit) {
				t.Fatalf("limit = %v, %v", list, err)
			}
		})
	}
	two := &CSAHeader{Elements: []CSAElement{{Name: "MRDiffusion", Values: []string{encoded}}, {Name: "MR_ASL", Values: []string{encoded}}}}
	for _, limits := range []EmbeddedDatasetLimits{{MaxDatasets: 1}, {MaxHeaderElements: 1}, {MaxTotalEncodedBytes: 2*len(encoded) - 1}, {MaxTotalDecodedBytes: 2*len(wire) - 1}} {
		if list, err := two.EmbeddedDatasetsContext(nil, limits); list != nil || !errors.Is(err, ErrEmbeddedDatasetLimit) {
			t.Fatalf("aggregate admission = %v, %v", list, err)
		}
	}
	if list, err := two.EmbeddedDatasetsContext(nil, EmbeddedDatasetLimits{MaxTotalEncodedBytes: 2 * len(encoded), MaxTotalDecodedBytes: 2 * len(wire), MaxDatasets: 2}); err != nil || len(list) != 2 {
		t.Fatalf("exact aggregate = %v, %v", list, err)
	} else {
		for _, entry := range list {
			_ = entry.Dataset.Close()
		}
	}
	two.Elements[1].Values[0] = "%%%"
	if list, err := two.EmbeddedDatasetsContext(nil, EmbeddedDatasetLimits{}); list != nil || !errors.Is(err, ErrInvalidEmbeddedDataset) {
		t.Fatalf("late failure returned partial results: %v, %v", list, err)
	}
}

func embeddedSQ(contents []byte) []byte {
	out := []byte{0x18, 0, 0x17, 0x91, 'S', 'Q', 0, 0}
	out = binary.LittleEndian.AppendUint32(out, uint32(len(contents)))
	return append(out, contents...)
}

func embeddedItem(contents []byte) []byte {
	out := []byte{0xfe, 0xff, 0, 0xe0}
	out = binary.LittleEndian.AppendUint32(out, uint32(len(contents)))
	return append(out, contents...)
}

func TestEmbeddedDICOMTruncationStructuralLimitsAndSanitizedCauses(t *testing.T) {
	wire := embeddedDiffusionWire()
	// Every incomplete header/value boundary is rejected; exact valid prefixes
	// (20 and 36 bytes) are complete raw datasets and intentionally accepted.
	for n := 1; n < len(wire); n++ {
		if n == 20 || n == 36 {
			continue
		}
		_, err := embeddedTestHeader("MRDiffusion", base64.StdEncoding.EncodeToString(wire[:n])).EmbeddedDatasetContext(nil, "MRDiffusion", EmbeddedDatasetLimits{})
		if !errors.Is(err, ErrInvalidEmbeddedDataset) {
			t.Fatalf("truncated at %d: %v", n, err)
		}
	}
	badVR := bytes.Clone(wire)
	copy(badVR[4:6], "ZZ")
	badValue := bytes.Clone(wire[:8])
	binary.LittleEndian.PutUint16(badValue[6:], 65534)
	badReserved := embeddedSQ(embeddedItem(wire))
	badReserved[6] = 1
	part10 := append(make([]byte, 128), []byte("DICM")...)
	for _, malformed := range [][]byte{badVR, badValue, badReserved, part10, embeddedSQ(embeddedItem(wire)[:9])} {
		_, err := embeddedTestHeader("MRDiffusion", base64.StdEncoding.EncodeToString(malformed)).EmbeddedDatasetContext(nil, "MRDiffusion", EmbeddedDatasetLimits{})
		var location *EmbeddedDatasetError
		if !errors.Is(err, ErrInvalidEmbeddedDataset) || !errors.As(err, &location) || location.ElementIndex != 0 || location.ValueIndex != 0 {
			t.Fatalf("malformed = %v", err)
		}
	}
	withEmptyItems := embeddedSQ(append(embeddedItem(nil), embeddedItem(nil)...))
	_, err := embeddedTestHeader("MRDiffusion", base64.StdEncoding.EncodeToString(withEmptyItems)).EmbeddedDatasetContext(nil, "MRDiffusion", EmbeddedDatasetLimits{MaxElements: 2})
	if !errors.Is(err, ErrEmbeddedDatasetLimit) || !errors.Is(err, parser.ErrMaxElementsExceeded) {
		t.Fatalf("empty item nodes bypassed limit: %v", err)
	}
	nested := embeddedSQ(embeddedItem(embeddedSQ(embeddedItem(wire))))
	_, err = embeddedTestHeader("MRDiffusion", base64.StdEncoding.EncodeToString(nested)).EmbeddedDatasetContext(nil, "MRDiffusion", EmbeddedDatasetLimits{MaxSequenceDepth: 3})
	if !errors.Is(err, ErrEmbeddedDatasetLimit) || !errors.Is(err, parser.ErrMaxDepthExceeded) {
		t.Fatalf("depth = %v", err)
	}
}

func TestEmbeddedRawDatasetDICMValueIsNotAFileHeuristic(t *testing.T) {
	// A valid OB value may contain DICM at absolute offset 128. It must not be
	// rejected or routed to a Part 10 reader solely for those four value bytes.
	wire := []byte{0x11, 0, 0x10, 0x10, 'O', 'B', 0, 0, 120, 0, 0, 0}
	wire = append(wire, make([]byte, 120)...)
	copy(wire[128:], "DICM")
	dataset, err := embeddedTestHeader("FmriResultSequence", base64.StdEncoding.EncodeToString(wire)).EmbeddedDatasetContext(nil, "FmriResultSequence", EmbeddedDatasetLimits{})
	if err != nil {
		t.Fatal(err)
	}
	defer dataset.Close()
	if value, ok := dataset.GetRaw(core.NewTag(0x0011, 0x1010)); !ok || string(value[len(value)-4:]) != "DICM" {
		t.Fatal("raw value was changed")
	}
}

// Err checks trigger actual context cancellation at a deterministic phase,
// avoiding timing sleeps and races with an arbitrarily fast metadata parse.
type embeddedCancelContext struct {
	context.Context
	cancel context.CancelFunc
	checks int
}

func (c *embeddedCancelContext) Err() error {
	c.checks--
	if c.checks <= 0 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestEmbeddedDatasetsCancelAdmissionDecodeAndParse(t *testing.T) {
	header := embeddedTestHeader("MRDiffusion", base64.StdEncoding.EncodeToString(embeddedDiffusionWire()))
	for _, checks := range []int{1, 3, 5, 12} {
		ctx, cancel := context.WithCancel(context.Background())
		controlled := &embeddedCancelContext{Context: ctx, cancel: cancel, checks: checks}
		list, err := header.EmbeddedDatasetsContext(controlled, EmbeddedDatasetLimits{})
		cancel()
		if list != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("checks %d: %v, %v", checks, list, err)
		}
	}
}
