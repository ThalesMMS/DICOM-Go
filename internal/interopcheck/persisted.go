// Package interopcheck verifies what an interoperability peer persisted or
// returned against an explicit expectation. A successful DIMSE status, a
// parser that accepts the file or a local roundtrip is never enough: the
// file's syntax, identity, every attribute (VR, VM, values, sequence items)
// and, for native Pixel Data, every frame and sample must match.
//
// Mismatches name a reason and a structural path only. They never carry
// attribute values, so reports built from them expose no dataset content.
package interopcheck

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// Reason classifies a persisted-content mismatch.
type Reason string

const (
	ReasonUnreadable     Reason = "unreadable"
	ReasonTransferSyntax Reason = "transfer-syntax"
	ReasonIdentity       Reason = "identity"
	ReasonMissing        Reason = "attribute-missing"
	ReasonUnexpected     Reason = "attribute-unexpected"
	ReasonAttribute      Reason = "attribute-differs"
	ReasonSequence       Reason = "sequence-differs"
	ReasonFrameCount     Reason = "frame-count"
	ReasonSample         Reason = "sample-differs"
)

// Mismatch is a value-free description of the first difference found.
type Mismatch struct {
	Reason Reason
	Path   string
}

func (m *Mismatch) Error() string {
	if m.Path == "" {
		return "persisted content: " + string(m.Reason)
	}
	return "persisted content: " + string(m.Reason) + " at " + m.Path
}

// ReasonOf returns the mismatch reason carried by err, or "" when err is not
// a Mismatch.
func ReasonOf(err error) Reason {
	var m *Mismatch
	if errors.As(err, &m) {
		return m.Reason
	}
	return ""
}

// Expectation is what a receiver must hold. Dataset is the complete expected
// dataset; TransferSyntaxUID is the syntax the file must declare, whose bytes
// must decode to that dataset.
type Expectation struct {
	TransferSyntaxUID string
	Dataset           *object.Object
}

var (
	tagSOPClassUID    = core.NewTag(0x0008, 0x0016)
	tagSOPInstanceUID = core.NewTag(0x0008, 0x0018)
	tagMediaClassUID  = core.NewTag(0x0002, 0x0002)
	tagMediaInstUID   = core.NewTag(0x0002, 0x0003)
)

// VerifyFile checks a persisted Part 10 file against want.
func VerifyFile(path string, want Expectation) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return &Mismatch{Reason: ReasonUnreadable}
	}
	return VerifyBytes(data, want)
}

// VerifyBytes checks a Part 10 stream against want. The declared syntax is
// checked before the dataset is decoded, so relabelling a stream without
// converting its bytes is reported as such even when it no longer parses.
func VerifyBytes(data []byte, want Expectation) error {
	if want.Dataset == nil || want.TransferSyntaxUID == "" {
		return fmt.Errorf("interopcheck: incomplete expectation")
	}
	header, err := object.ReadPart10Header(bytes.NewReader(data), object.ReadFileOptions{})
	if err != nil {
		return &Mismatch{Reason: ReasonUnreadable, Path: "file-meta"}
	}
	if header.TransferSyntax.UID != want.TransferSyntaxUID {
		return &Mismatch{Reason: ReasonTransferSyntax, Path: "file-meta/(0002,0010)"}
	}
	file, err := object.ReadFile(bytes.NewReader(data))
	if err != nil {
		return &Mismatch{Reason: ReasonUnreadable, Path: "dataset"}
	}
	if err := verifyIdentity(file, want.Dataset); err != nil {
		return err
	}
	return VerifyDataSet(file.Dataset, want.Dataset)
}

// VerifyDataSet compares a received dataset (for example one delivered by a
// C-GET suboperation) with the expected one: native frames and samples first,
// then every attribute.
func VerifyDataSet(got, want *object.Object) error {
	if got == nil || want == nil {
		return &Mismatch{Reason: ReasonMissing, Path: "dataset"}
	}
	if err := verifyFrames(got, want); err != nil {
		return err
	}
	a, err := dicomtest.SemanticDataSet(got.ToDataSet(), binary.LittleEndian)
	if err != nil {
		return &Mismatch{Reason: ReasonUnreadable, Path: "dataset"}
	}
	b, err := dicomtest.SemanticDataSet(want.ToDataSet(), binary.LittleEndian)
	if err != nil {
		return fmt.Errorf("interopcheck: expected dataset: %w", err)
	}
	diff := dicomtest.DiffSemantic(a, b)
	if diff == "" {
		return nil
	}
	path, detail, _ := strings.Cut(diff, ": ")
	switch {
	case strings.Contains(path, "/item["):
		return &Mismatch{Reason: ReasonSequence, Path: path}
	case strings.Contains(detail, "missing"):
		return &Mismatch{Reason: ReasonMissing, Path: path}
	case strings.Contains(detail, "unexpected") || strings.Contains(detail, "sequence item count"):
		if strings.Contains(detail, "sequence") {
			return &Mismatch{Reason: ReasonSequence, Path: path}
		}
		return &Mismatch{Reason: ReasonUnexpected, Path: path}
	default:
		return &Mismatch{Reason: ReasonAttribute, Path: path}
	}
}

func verifyIdentity(file *object.File, want *object.Object) error {
	wantClass, _ := want.GetString(tagSOPClassUID)
	wantInstance, _ := want.GetString(tagSOPInstanceUID)
	gotClass, _ := file.Dataset.GetString(tagSOPClassUID)
	gotInstance, _ := file.Dataset.GetString(tagSOPInstanceUID)
	if gotClass != wantClass {
		return &Mismatch{Reason: ReasonIdentity, Path: "dataset/(0008,0016)"}
	}
	if gotInstance != wantInstance {
		return &Mismatch{Reason: ReasonIdentity, Path: "dataset/(0008,0018)"}
	}
	if file.Meta != nil {
		if v, ok := file.Meta.GetString(tagMediaClassUID); !ok || v != wantClass {
			return &Mismatch{Reason: ReasonIdentity, Path: "file-meta/(0002,0002)"}
		}
		if v, ok := file.Meta.GetString(tagMediaInstUID); !ok || v != wantInstance {
			return &Mismatch{Reason: ReasonIdentity, Path: "file-meta/(0002,0003)"}
		}
	}
	return nil
}

// verifyFrames compares native frames one by one. Encapsulated Pixel Data is
// compared fragment by fragment by the attribute comparison instead.
func verifyFrames(got, want *object.Object) error {
	wantFrames, err := pixeldata.ExtractNativeFramesView(want)
	if err != nil {
		return nil
	}
	gotFrames, err := pixeldata.ExtractNativeFramesView(got)
	if err != nil {
		if _, ok := got.Get(core.TagPixelData); !ok {
			return &Mismatch{Reason: ReasonMissing, Path: "dataset/(7FE0,0010)"}
		}
		return &Mismatch{Reason: ReasonFrameCount, Path: "dataset/(7FE0,0010)"}
	}
	if len(gotFrames.Data) != len(wantFrames.Data) {
		return &Mismatch{Reason: ReasonFrameCount, Path: "dataset/(7FE0,0010)"}
	}
	for i := range wantFrames.Data {
		g, w := gotFrames.Data[i], wantFrames.Data[i]
		if len(g) != len(w) {
			return &Mismatch{Reason: ReasonSample, Path: fmt.Sprintf("dataset/(7FE0,0010)/frame[%d]", i)}
		}
		for j := range w {
			if g[j] != w[j] {
				return &Mismatch{Reason: ReasonSample, Path: fmt.Sprintf("dataset/(7FE0,0010)/frame[%d]/byte[%d]", i, j)}
			}
		}
	}
	return nil
}

// Mutation is a negative control: an alteration a receiver could make while
// still answering success. Every mutation must be rejected by VerifyBytes.
type Mutation string

const (
	MutationDeleteAttribute Mutation = "delete-attribute"
	MutationRelabelSyntax   Mutation = "relabel-syntax"
	MutationDistantSample   Mutation = "distant-sample"
	MutationRemoveFrame     Mutation = "remove-frame"
	MutationCorruptSequence Mutation = "corrupt-sequence"
)

// Mutations lists every negative control in a stable order.
func Mutations() []Mutation {
	return []Mutation{MutationDeleteAttribute, MutationRelabelSyntax, MutationDistantSample, MutationRemoveFrame, MutationCorruptSequence}
}

// Want returns the reason VerifyBytes must report for m.
func (m Mutation) Want() Reason {
	switch m {
	case MutationDeleteAttribute:
		return ReasonMissing
	case MutationRelabelSyntax:
		return ReasonTransferSyntax
	case MutationDistantSample:
		return ReasonSample
	case MutationRemoveFrame:
		return ReasonFrameCount
	case MutationCorruptSequence:
		return ReasonSequence
	}
	return ""
}

// Apply returns a Part 10 stream derived from the Explicit VR Little Endian
// file of ds with m applied, written by this library. Peers apply the same
// mutations independently; see the pynetdicom tampering peer.
func (m Mutation) Apply(ds *object.Object) ([]byte, error) {
	var source bytes.Buffer
	if err := object.WriteFile(&source, &object.File{Dataset: ds, TransferSyntax: transfer.ExplicitVRLittleEndian}); err != nil {
		return nil, err
	}
	copied, err := object.ReadFile(&source)
	if err != nil {
		return nil, err
	}
	obj := copied.Dataset
	switch m {
	case MutationDeleteAttribute:
		if !obj.Remove(core.NewTag(0x0008, 0x1030)) {
			return nil, fmt.Errorf("interopcheck: %s needs Study Description", m)
		}
	case MutationRelabelSyntax:
	case MutationDistantSample, MutationRemoveFrame:
		pixel, ok := obj.Get(core.TagPixelData)
		raw, isRaw := pixel.Value.(core.RawValue)
		if !ok || !isRaw {
			return nil, fmt.Errorf("interopcheck: %s needs native Pixel Data", m)
		}
		raw = append(core.RawValue(nil), raw...)
		if m == MutationDistantSample {
			raw[len(raw)-2] ^= 0x5a
		} else {
			frames, err := pixeldata.ExtractNativeFramesView(ds)
			if err != nil || len(frames.Data) < 2 {
				return nil, fmt.Errorf("interopcheck: %s needs a multi-frame image", m)
			}
			raw = raw[:len(raw)-len(frames.Data[len(frames.Data)-1])]
			obj.Put(dicomtest.NewStringElement(core.NewTag(0x0028, 0x0008), core.VRIS, fmt.Sprint(len(frames.Data)-1)))
		}
		pixel.Value = raw
		obj.Put(pixel)
	case MutationCorruptSequence:
		if err := corruptFirstSequence(obj); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("interopcheck: unknown mutation %q", m)
	}
	var out bytes.Buffer
	if err := object.WriteFile(&out, &object.File{Dataset: obj, TransferSyntax: transfer.ExplicitVRLittleEndian}); err != nil {
		return nil, err
	}
	data := out.Bytes()
	if m == MutationRelabelSyntax {
		// Declare Explicit VR Big Endian while keeping every little-endian byte:
		// the UIDs have equal length, so nothing else in the stream moves.
		from, to := []byte(transfer.ExplicitVRLittleEndian.UID+"\x00"), []byte(transfer.ExplicitVRBigEndian.UID+"\x00")
		if bytes.Count(data, from) != 1 {
			return nil, fmt.Errorf("interopcheck: syntax UID not unique in file meta")
		}
		data = bytes.Replace(data, from, to, 1)
	}
	return data, nil
}

// corruptFirstSequence changes one text value inside the first sequence item
// that has one.
func corruptFirstSequence(obj *object.Object) error {
	for _, element := range obj.Elements() {
		seq, ok := element.Value.(core.SequenceValue)
		if !ok {
			continue
		}
		items := make([]core.DataSet, len(seq.Items))
		copy(items, seq.Items)
		for i, item := range items {
			elements := append([]core.Element(nil), item.Elements...)
			for j, nested := range elements {
				if !nested.Header.VR.IsStringLike() {
					continue
				}
				switch value := nested.Value.(type) {
				case core.StringValue:
					if len(value) == 0 || value[0] == "" {
						continue
					}
					changed := append(core.StringValue(nil), value...)
					changed[0] = string(changed[0][0]^0x01) + changed[0][1:]
					elements[j].Value = changed
				case core.RawValue:
					if len(value) == 0 {
						continue
					}
					changed := append(core.RawValue(nil), value...)
					changed[0] ^= 0x01
					elements[j].Value = changed
				default:
					continue
				}
				items[i] = core.DataSet{Elements: elements}
				element.Value = core.SequenceValue{Items: items}
				obj.Put(element)
				return nil
			}
		}
	}
	return fmt.Errorf("interopcheck: no sequence item with a text value")
}
