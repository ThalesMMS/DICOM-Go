package mrdiffusion

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/siemens"
)

// Wire and CSA fixtures are constructed from literal headers and IEEE values,
// independently of object.WriteDataSet/siemens.EncodeCSA/new decoding APIs.
func embeddedDWIWire() []byte {
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

func embeddedDWIItem(content []byte) []byte {
	out := []byte{0xfe, 0xff, 0, 0xe0}
	out = binary.LittleEndian.AppendUint32(out, uint32(len(content)))
	return append(out, content...)
}

func embeddedDWISequence(group, element uint16, content []byte) []byte {
	out := binary.LittleEndian.AppendUint16(nil, group)
	out = binary.LittleEndian.AppendUint16(out, element)
	out = append(out, 'S', 'Q', 0, 0)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(content)))
	return append(out, content...)
}

func embeddedDWIMacro() []byte {
	wire := embeddedDWIWire()
	gradient := embeddedDWISequence(0x0018, 0x9076, embeddedDWIItem(wire[36:]))
	macro := append(append([]byte(nil), wire[:36]...), gradient...)
	return embeddedDWISequence(0x0018, 0x9117, embeddedDWIItem(macro))
}

func embeddedDWIParent(encoded string) *object.Object {
	blob := []byte{'S', 'V', '1', '0', 4, 3, 2, 1}
	blob = binary.LittleEndian.AppendUint32(blob, 1)
	blob = binary.LittleEndian.AppendUint32(blob, 77)
	name := make([]byte, 64)
	copy(name, "MRDiffusion")
	blob = append(blob, name...)
	blob = binary.LittleEndian.AppendUint32(blob, 1)
	blob = append(blob, 'O', 'B', 0, 0)
	blob = binary.LittleEndian.AppendUint32(blob, 0)
	blob = binary.LittleEndian.AppendUint32(blob, 1)
	blob = binary.LittleEndian.AppendUint32(blob, 77)
	n := len(encoded) + 1
	for _, value := range []uint32{uint32(n), uint32(n), 77, uint32(n)} {
		blob = binary.LittleEndian.AppendUint32(blob, value)
	}
	blob = append(blob, encoded...)
	blob = append(blob, make([]byte, 1+(4-n%4)%4)...)
	parent := newDataset("SIEMENS")
	creator(parent, 0x0029, "SIEMENS CSA HEADER")
	parent.Put(core.NewRawElement(core.NewTag(0x0029, 0x1010), core.VROB, blob))
	return parent
}

func TestReadSiemensEmbeddedIndependentXAAndLegacyDefaults(t *testing.T) {
	if len(embeddedDWIWire()) != 68 || len(embeddedDWIMacro()) != 108 {
		t.Fatal("independent wire dimensions changed")
	}
	for name, wire := range map[string][]byte{
		"top attributes": embeddedDWIWire(), "macro sequence": embeddedDWIMacro(),
		"shared macro":    embeddedDWISequence(0x5200, 0x9229, embeddedDWIItem(embeddedDWIMacro())),
		"per-frame macro": embeddedDWISequence(0x5200, 0x9230, embeddedDWIItem(embeddedDWIMacro())),
	} {
		t.Run(name, func(t *testing.T) {
			parent := embeddedDWIParent(base64.StdEncoding.EncodeToString(wire))
			if _, err := Read(parent, 0); !errors.Is(err, ErrNotFound) {
				t.Fatalf("legacy Read auto-decoded the opt-in payload: %v", err)
			}
			frame, err := ReadSiemensEmbeddedContext(nil, parent, 0, SiemensEmbeddedOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if frame.BValue != 1200 || frame.Direction != [3]float64{0.6, -0.8, 0} || !frame.HasDirection || frame.DirectionFrame != FramePatient || frame.Directionality != "DIRECTIONAL" || frame.Source != SourceSiemensCSAEmbedded || frame.Tag != core.NewTag(0x0029, 0x1010) {
				t.Fatalf("frame = %+v", frame)
			}
			if name == "shared macro" && frame.FunctionalGroup != object.FunctionalGroupShared || name == "per-frame macro" && frame.FunctionalGroup != object.FunctionalGroupPerFrame {
				t.Fatalf("group source = %v", frame.FunctionalGroup)
			}
		})
	}
}

func TestReadSiemensEmbeddedMissingCorruptAndLimits(t *testing.T) {
	if _, err := ReadSiemensEmbeddedContext(nil, nil, 0, SiemensEmbeddedOptions{}); !errors.Is(err, siemens.ErrNoCSAHeader) {
		t.Fatalf("absent CSA = %v", err)
	}
	if _, err := ReadSiemensEmbeddedContext(nil, embeddedDWIParent("%%%"), 0, SiemensEmbeddedOptions{}); !errors.Is(err, siemens.ErrInvalidEmbeddedDataset) || errors.Is(err, ErrNotFound) {
		t.Fatalf("corruption collapsed to absent metadata: %v", err)
	}
	valid := embeddedDWIParent(base64.StdEncoding.EncodeToString(embeddedDWIWire()))
	if _, err := ReadSiemensEmbeddedContext(nil, valid, 0, SiemensEmbeddedOptions{Limits: siemens.EmbeddedDatasetLimits{MaxDecodedBytes: 67}}); !errors.Is(err, siemens.ErrEmbeddedDatasetLimit) {
		t.Fatalf("decode budget = %v", err)
	}
	if _, err := ReadSiemensEmbeddedContext(nil, valid, 0, SiemensEmbeddedOptions{Lookup: siemens.LookupOptions{Limits: siemens.CSALimits{MaxBytes: 16}}}); !errors.Is(err, siemens.ErrCSALimit) {
		t.Fatalf("CSA budget = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ReadSiemensEmbeddedContext(ctx, valid, 0, SiemensEmbeddedOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel = %v", err)
	}
	if _, err := ReadSiemensEmbeddedContext(nil, valid, -1, SiemensEmbeddedOptions{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("negative frame = %v", err)
	}
	// Negative/nonfinite diffusion values retain the established semantic guard.
	for _, b := range []float64{-1, math.NaN(), math.Inf(1)} {
		wire := embeddedDWIWire()
		binary.LittleEndian.PutUint64(wire[28:36], math.Float64bits(b))
		if _, err := ReadSiemensEmbeddedContext(nil, embeddedDWIParent(base64.StdEncoding.EncodeToString(wire)), 0, SiemensEmbeddedOptions{}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid b-value accepted: %v", err)
		}
	}
}

func TestReadSiemensEmbeddedMacroAmbiguityAndMissingBValue(t *testing.T) {
	wire := embeddedDWIWire()
	twoItems := embeddedDWISequence(0x0018, 0x9117, append(embeddedDWIItem(wire), embeddedDWIItem(wire)...))
	shared := embeddedDWISequence(0x5200, 0x9229, embeddedDWIItem(embeddedDWIMacro()))
	perFrame := embeddedDWISequence(0x5200, 0x9230, embeddedDWIItem(embeddedDWIMacro()))
	conflict := append(shared, perFrame...)
	xGradient, yGradient := append([]byte(nil), wire[36:]...), append([]byte(nil), wire[36:]...)
	for i, value := range []float64{1, 0, 0} {
		binary.LittleEndian.PutUint64(xGradient[8+8*i:], math.Float64bits(value))
	}
	for i, value := range []float64{0, 1, 0} {
		binary.LittleEndian.PutUint64(yGradient[8+8*i:], math.Float64bits(value))
	}
	twoGradients := embeddedDWISequence(0x0018, 0x9076, append(embeddedDWIItem(xGradient), embeddedDWIItem(yGradient)...))
	ambiguousGradient := embeddedDWISequence(0x0018, 0x9117, embeddedDWIItem(append(append([]byte(nil), wire[:36]...), twoGradients...)))
	for _, payload := range [][]byte{twoItems, conflict, ambiguousGradient} {
		if _, err := ReadSiemensEmbeddedContext(nil, embeddedDWIParent(base64.StdEncoding.EncodeToString(payload)), 0, SiemensEmbeddedOptions{}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("ambiguous macro = %v", err)
		}
	}
	if _, err := ReadSiemensEmbeddedContext(nil, embeddedDWIParent(base64.StdEncoding.EncodeToString(wire[:20])), 0, SiemensEmbeddedOptions{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("valid dataset without b-value = %v", err)
	}
}

func TestReadSiemensEmbeddedSemanticErrorsExcludeValues(t *testing.T) {
	secret := "sensitive-value " // even-length text, intentionally nonnumeric
	text := []byte{0x18, 0, 0x87, 0x90, 'L', 'O', byte(len(secret)), 0}
	text = append(text, secret...)
	badGradient := []byte{0x18, 0, 0x89, 0x90, 'L', 'O', byte(len(secret)), 0}
	badGradient = append(badGradient, secret...)
	gradient := embeddedDWISequence(0x0018, 0x9076, embeddedDWIItem(badGradient))
	macro := embeddedDWISequence(0x0018, 0x9117, embeddedDWIItem(append(embeddedDWIWire()[:36], gradient...)))
	for _, wire := range [][]byte{text, macro} {
		_, err := ReadSiemensEmbeddedContext(nil, embeddedDWIParent(base64.StdEncoding.EncodeToString(wire)), 0, SiemensEmbeddedOptions{})
		if !errors.Is(err, ErrInvalid) || strings.Contains(err.Error(), "sensitive-value") {
			t.Fatalf("semantic diagnostic leaked value: %v", err)
		}
	}
}
