package pixeldata

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/derivation"
	"github.com/ThalesMMS/dicom-go/object"
)

type encodedMetadataDelta struct {
	photometric string
	planar      *uint16
}

type encodedRewriteInput struct {
	source     *object.Object
	metadata   Metadata
	fragments  [][]byte
	collection bool
	delta      *encodedMetadataDelta
	limits     TranscodeLimits
}

// shallowDataSetDraft may still alias source values and must never be returned
// from a public API. detachDataSet is its only publication boundary.
type shallowDataSetDraft struct {
	object *object.Object
}

type detachedDataSet struct {
	object *object.Object
}

func rewriteEncodedDataSet(ctx context.Context, input encodedRewriteInput) (shallowDataSetDraft, int64, error) {
	expected := input.metadata.NumberOfFrames
	if input.collection {
		expected = 1
	}
	if len(input.fragments) != expected || len(input.fragments) > input.limits.MaxFrames || len(input.fragments) > input.limits.MaxFragments {
		return shallowDataSetDraft{}, 0, transcodeLimitError("fragments")
	}
	lengths := make([]uint64, len(input.fragments))
	var total uint64
	for i, fragment := range input.fragments {
		if err := ctx.Err(); err != nil {
			return shallowDataSetDraft{}, 0, err
		}
		lengths[i] = uint64(len(fragment))
		if total > math.MaxUint64-lengths[i] {
			return shallowDataSetDraft{}, 0, transcodeLimitError("output_bytes")
		}
		total += lengths[i]
		if total > uint64(input.limits.MaxOutputBytes) {
			return shallowDataSetDraft{}, 0, transcodeLimitError("output_bytes")
		}
	}
	var bot, eot, eotLengths []byte
	if !input.collection {
		var err error
		bot, eot, eotLengths, err = frameOffsetTables(lengths)
		if err != nil {
			return shallowDataSetDraft{}, 0, err
		}
	}
	elements := encodedMetadataElements(input.source, input.delta)
	elements = replaceElement(elements, core.Element{
		Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB, Length: core.UndefinedLength, LengthSet: true},
		Value:  core.FragmentSequence{OffsetTable: bot, Fragments: input.fragments},
	})
	if len(eot) > 0 {
		elements = replaceElement(elements, core.NewRawElement(tagExtendedOffsetTable, core.VROV, eot))
		elements = replaceElement(elements, core.NewRawElement(tagExtendedOffsetTableLengths, core.VROV, eotLengths))
	}
	draft := object.FromElements(elements, nil)
	draft.SetValueByteOrder(binary.LittleEndian)
	return shallowDataSetDraft{object: draft}, int64(total), nil
}

// encodedMetadataElements is source's elements for encapsulated output
// without their Pixel Data payload: the offset tables of the source removed
// (the writer computes new ones) and the encoder's metadata delta applied.
func encodedMetadataElements(source *object.Object, delta *encodedMetadataDelta) []core.Element {
	elements := removeElements(source.Elements(), tagExtendedOffsetTable, tagExtendedOffsetTableLengths, tagEncapsulatedPixelDataValueTotalLength)
	if delta != nil {
		if strings.TrimSpace(delta.photometric) != "" {
			elements = replaceElement(elements, photometricInterpretationElement(delta.photometric))
		}
		if delta.planar != nil {
			elements = replaceElement(elements, uint16RawElement(tagPlanarConfiguration, *delta.planar))
		}
	}
	return elements
}

func detachDataSet(ctx context.Context, draft shallowDataSetDraft, limits TranscodeLimits) (detachedDataSet, error) {
	result, err := cloneDetachedObject(ctx, draft.object, outputCloneLimits(limits))
	if err != nil {
		return detachedDataSet{}, err
	}
	return detachedDataSet{object: result}, nil
}

func finalizeLossyDataSet(data detachedDataSet, nativeBytes, encodedBytes int64, method, root string) error {
	return applyLossyTranscodeMetadataWithUIDRoot(data.object, nativeBytes, encodedBytes, method, root)
}

func frameOffsetTables(lengths []uint64) (bot, eot, eotLengths []byte, err error) {
	if len(lengths) == 0 {
		return nil, nil, nil, nil
	}
	offsets := make([]uint64, len(lengths))
	var next uint64
	useEOT := false
	for i, length := range lengths {
		offsets[i] = next
		if next > math.MaxUint32 {
			useEOT = true
		}
		padded := length
		if padded%2 != 0 {
			padded++
		}
		if next > math.MaxUint64-8-padded {
			return nil, nil, nil, transcodeLimitError("offset_table")
		}
		next += 8 + padded
	}
	if !useEOT {
		bot = make([]byte, len(offsets)*4)
		for i, offset := range offsets {
			binary.LittleEndian.PutUint32(bot[i*4:], uint32(offset))
		}
		return bot, nil, nil, nil
	}
	eot = make([]byte, len(offsets)*8)
	eotLengths = make([]byte, len(lengths)*8)
	for i := range offsets {
		binary.LittleEndian.PutUint64(eot[i*8:], offsets[i])
		binary.LittleEndian.PutUint64(eotLengths[i*8:], lengths[i])
	}
	return nil, eot, eotLengths, nil
}

func applyLossyTranscodeMetadata(dataset *object.Object, nativeBytes, encodedBytes int64, method string) error {
	return applyLossyTranscodeMetadataWithUIDRoot(dataset, nativeBytes, encodedBytes, method, "")
}

func applyLossyTranscodeMetadataWithUIDRoot(dataset *object.Object, nativeBytes, encodedBytes int64, method, root string) error {
	if dataset == nil || nativeBytes <= 0 || encodedBytes <= 0 || strings.TrimSpace(method) == "" {
		return fmt.Errorf("%w: invalid lossy encoder result", ErrEncoderOutputInvalid)
	}
	ratios, err := lossyHistoryStrings(dataset, tagLossyImageCompressionRatio, core.VRDS)
	if err != nil {
		return err
	}
	methods, err := lossyHistoryStrings(dataset, tagLossyImageCompressionMethod, core.VRCS)
	if err != nil {
		return err
	}
	if len(ratios) != len(methods) {
		return fmt.Errorf("%w: lossy history multiplicity", ErrEncoderOutputInvalid)
	}
	if !validLossyCompressionMethod(method) {
		return fmt.Errorf("%w: lossy method", ErrEncoderOutputInvalid)
	}
	for _, existing := range methods {
		if !validLossyCompressionMethod(existing) {
			return fmt.Errorf("%w: lossy history method", ErrEncoderOutputInvalid)
		}
	}
	for _, existing := range ratios {
		if !validLossyCompressionRatio(existing) {
			return fmt.Errorf("%w: lossy history ratio", ErrEncoderOutputInvalid)
		}
	}
	dataset.Put(core.Element{Header: core.ElementHeader{Tag: tagLossyImageCompression, VR: core.VRCS}, Value: core.StringValue{"01"}})
	ratios = append(ratios, strconv.FormatFloat(float64(nativeBytes)/float64(encodedBytes), 'g', 8, 64))
	dataset.Put(core.Element{Header: core.ElementHeader{Tag: tagLossyImageCompressionRatio, VR: core.VRDS}, Value: core.StringValue(ratios)})
	methods = append(methods, strings.TrimSpace(method))
	dataset.Put(core.Element{Header: core.ElementHeader{Tag: tagLossyImageCompressionMethod, VR: core.VRCS}, Value: core.StringValue(methods)})
	imageTypes := elementStrings(dataset, tagImageType)
	if len(imageTypes) == 0 {
		imageTypes = []string{"DERIVED", "PRIMARY"}
	} else {
		imageTypes[0] = "DERIVED"
	}
	dataset.Put(core.Element{Header: core.ElementHeader{Tag: tagImageType, VR: core.VRCS}, Value: core.StringValue(imageTypes)})
	// Record the step as DCMTK and GDCM do (PS3.3 C.7.6.1.1.5): Derivation
	// Code 113040 and a Derivation Description, and a Source Image reference
	// to the predecessor (121320) when it has SOP Class and Instance UIDs.
	step := derivation.Derivation{
		Description: derivation.LossyDescription(strings.TrimSpace(method), float64(nativeBytes)/float64(encodedBytes)),
		Codes:       []derivation.Code{derivation.LossyCompression},
	}
	if source, err := derivation.SourceOf(dataset, &derivation.UncompressedPredecessor); err == nil {
		step.Sources = []derivation.Source{source}
	}
	if err := derivation.ApplyTo(dataset, step); err != nil {
		return fmt.Errorf("%w: derivation: %v", ErrEncoderOutputInvalid, err)
	}
	uid, err := newTranscodeUID(root)
	if err != nil {
		return &TranscodeError{Stage: "uid", Err: ErrTranscodeTransaction}
	}
	dataset.Put(core.Element{Header: core.ElementHeader{Tag: tagSOPInstanceUID, VR: core.VRUI}, Value: core.StringValue{uid}})
	return nil
}

func lossyHistoryStrings(dataset *object.Object, tag core.Tag, vr core.VR) ([]string, error) {
	element, ok := dataset.Get(tag)
	if !ok {
		return nil, nil
	}
	if element.VR() != vr {
		return nil, fmt.Errorf("%w: lossy history VR", ErrEncoderOutputInvalid)
	}
	return append([]string(nil), element.StringValues()...), nil
}

func validLossyCompressionRatio(value string) bool {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || len(value) > 16 || strings.Contains(value, "\\") {
		return false
	}
	index := 0
	if trimmed[index] == '+' || trimmed[index] == '-' {
		index++
		if index == len(trimmed) {
			return false
		}
	}
	digits := 0
	for index < len(trimmed) && trimmed[index] >= '0' && trimmed[index] <= '9' {
		index++
		digits++
	}
	if index < len(trimmed) && trimmed[index] == '.' {
		index++
		for index < len(trimmed) && trimmed[index] >= '0' && trimmed[index] <= '9' {
			index++
			digits++
		}
	}
	if digits == 0 {
		return false
	}
	if index < len(trimmed) && (trimmed[index] == 'E' || trimmed[index] == 'e') {
		index++
		if index < len(trimmed) && (trimmed[index] == '+' || trimmed[index] == '-') {
			index++
		}
		exponentDigits := 0
		for index < len(trimmed) && trimmed[index] >= '0' && trimmed[index] <= '9' {
			index++
			exponentDigits++
		}
		if exponentDigits == 0 {
			return false
		}
	}
	if index != len(trimmed) {
		return false
	}
	ratio, err := strconv.ParseFloat(trimmed, 64)
	return err == nil && ratio > 0 && !math.IsInf(ratio, 0) && !math.IsNaN(ratio)
}

func elementStrings(dataset *object.Object, tag core.Tag) []string {
	element, ok := dataset.Get(tag)
	if !ok {
		return nil
	}
	return append([]string(nil), element.StringValues()...)
}

func cloneFrames(frames [][]byte) [][]byte {
	out := make([][]byte, len(frames))
	for i := range frames {
		out[i] = core.CloneBytes(frames[i])
	}
	return out
}
