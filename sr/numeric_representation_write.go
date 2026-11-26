package sr

import (
	"github.com/ThalesMMS/dicom-go/core"
	"sort"
)

// Restore after code contexts so that a rebuilt units sequence cannot discard
// preserved numeric attributes. The snapshot guard has already checked paths,
// units, scalars, payloads and backing-node identities before output allocation.
func restoreNumericRepresentations(elements *[]core.Element, items []ContentItem, parent ContentItemIdentifier, numeric map[string]NumericRepresentation) error {
	if len(numeric) == 0 || len(items) == 0 {
		return nil
	}
	sequenceIndex := elementIndex(*elements, tagContentSequence)
	if sequenceIndex < 0 {
		return &NumericRepresentationError{Code: NumericCodeAssociation}
	}
	sequence, ok := (*elements)[sequenceIndex].Value.(core.SequenceValue)
	if !ok || len(sequence.Items) != len(items) {
		return &NumericRepresentationError{Code: NumericCodeAssociation}
	}
	for i := range items {
		path := appendPath(parent, uint32(i+1))
		dataSet := &sequence.Items[i]
		if representation, present := numeric[path.String()]; present {
			index := elementIndex(dataSet.Elements, tagMeasuredValueSeq)
			if index < 0 {
				return &NumericRepresentationError{Code: NumericCodeAssociation, Path: path.String()}
			}
			measured, ok := dataSet.Elements[index].Value.(core.SequenceValue)
			if !ok || len(measured.Items) != 1 {
				return &NumericRepresentationError{Code: NumericCodeAssociation, Path: path.String()}
			}
			target := &measured.Items[0].Elements
			// Raw DS bypasses any text formatter or pad normalization. Part10 writers
			// may append the required space for an odd-length payload; no byte is lost.
			replaceOrAppendElement(target, core.NewRawElement(tagNumericValue, core.VRDS, []byte(representation.DecimalString)))
			if representation.HasFloatingPoint {
				replaceOrAppendElement(target, core.Element{Header: core.ElementHeader{Tag: tagFloatingPointValue, VR: core.VRFD}, Value: core.Float64Value{representation.FloatingPoint}})
			}
			if representation.HasRational {
				replaceOrAppendElement(target, core.Element{Header: core.ElementHeader{Tag: tagRationalNumeratorValue, VR: core.VRSL}, Value: core.Int32Value{representation.RationalNumerator}})
				replaceOrAppendElement(target, core.Element{Header: core.ElementHeader{Tag: tagRationalDenominatorValue, VR: core.VRUL}, Value: core.Uint32Value{representation.RationalDenominator}})
			}
			sort.SliceStable(*target, func(i, j int) bool { return (*target)[i].Tag().Less((*target)[j].Tag()) })
			dataSet.Elements[index].Value = measured
		}
		if err := restoreNumericRepresentations(&dataSet.Elements, items[i].Children, path, numeric); err != nil {
			return err
		}
	}
	(*elements)[sequenceIndex].Value = sequence
	return nil
}
