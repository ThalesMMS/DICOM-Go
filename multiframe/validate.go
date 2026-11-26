package multiframe

import (
	"encoding/binary"
	"fmt"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

func validateClassicDataset(dataset *object.Object, targetClassUID, modality string) error {
	if got, err := requiredUID(dataset, tagSOPClassUID); err != nil || got != targetClassUID {
		return fmt.Errorf("%w: target SOP Class UID", ErrNonconformant)
	}
	for _, tag := range []core.Tag{tagSOPInstanceUID, tagStudyInstanceUID, tagSeriesInstanceUID, tagFrameOfReferenceUID} {
		if _, err := requiredUID(dataset, tag); err != nil {
			return err
		}
		if element, _ := dataset.Get(tag); element.VR() != core.VRUI {
			return fmt.Errorf("%w: UID attribute %s must use UI", ErrNonconformant, tag)
		}
	}
	if got, ok := dataset.GetString(tagModality); !ok || strings.TrimSpace(got) != modality || elementVR(dataset, tagModality) != core.VRCS {
		return fmt.Errorf("%w: Modality must be %s", ErrNonconformant, modality)
	}
	if values, ok := dataset.GetStrings(tagImageType); !ok || len(values) < 2 || strings.TrimSpace(values[0]) == "" || elementVR(dataset, tagImageType) != core.VRCS {
		return fmt.Errorf("%w: ImageType must have at least two Values", ErrNonconformant)
	}
	for _, tag := range []core.Tag{tagRows, tagColumns, tagSamplesPerPixel, tagBitsAllocated, tagBitsStored, tagHighBit, tagPixelRepresentation} {
		if _, ok := uint16Value(dataset, tag); !ok || elementVR(dataset, tag) != core.VRUS {
			return fmt.Errorf("%w: required pixel attribute %s", ErrNonconformant, tag)
		}
	}
	rows, _ := uint16Value(dataset, tagRows)
	columns, _ := uint16Value(dataset, tagColumns)
	bitsAllocated, _ := uint16Value(dataset, tagBitsAllocated)
	bitsStored, _ := uint16Value(dataset, tagBitsStored)
	highBit, _ := uint16Value(dataset, tagHighBit)
	samples, _ := uint16Value(dataset, tagSamplesPerPixel)
	pixelRepresentation, _ := uint16Value(dataset, tagPixelRepresentation)
	if rows == 0 || columns == 0 || bitsAllocated != 16 || bitsStored < 12 || bitsStored > 16 || highBit != bitsStored-1 || samples != 1 || pixelRepresentation > 1 {
		return fmt.Errorf("%w: classic CT/MR integer pixel characteristics", ErrNonconformant)
	}
	photometric, ok := dataset.GetString(tagPhotometricInterpretation)
	photometric = strings.TrimSpace(photometric)
	if !ok || elementVR(dataset, tagPhotometricInterpretation) != core.VRCS || (photometric != "MONOCHROME1" && photometric != "MONOCHROME2") {
		return fmt.Errorf("%w: classic CT/MR PhotometricInterpretation", ErrNonconformant)
	}
	for _, geometry := range []struct {
		tag core.Tag
		vm  int
	}{{tagPixelSpacing, 2}, {tagImagePositionPatient, 3}, {tagImageOrientationPatient, 6}} {
		values, err := dataset.GetFloats(geometry.tag)
		if err != nil || len(values) != geometry.vm || elementVR(dataset, geometry.tag) != core.VRDS {
			return fmt.Errorf("%w: required frame geometry %s", ErrNonconformant, geometry.tag)
		}
		if geometry.tag == tagImageOrientationPatient {
			if deviation, ok := object.ImageOrientationDeviation(values); !ok || deviation > object.ImageOrientationTolerance {
				return fmt.Errorf("%w: Image Orientation (Patient) is not orthonormal", ErrNonconformant)
			}
		}
	}
	if dataset.Has(tagNumberOfFrames) || dataset.Has(tagSharedFunctionalGroupsSequence) || dataset.Has(tagPerFrameFunctionalGroupsSequence) {
		return fmt.Errorf("%w: enhanced multi-frame attributes remain", ErrNonconformant)
	}
	if !dataset.Has(tagConversionSourceAttributesSequence) || !dataset.Has(tagContributingEquipmentSequence) {
		return fmt.Errorf("%w: conversion provenance is missing", ErrNonconformant)
	}
	switch targetClassUID {
	case CTImageStorageUID:
		for _, tag := range []core.Tag{tagRescaleIntercept, tagRescaleSlope} {
			values, err := dataset.GetFloats(tag)
			if err != nil || len(values) != 1 || elementVR(dataset, tag) != core.VRDS {
				return fmt.Errorf("%w: required CT attribute %s", ErrNonconformant, tag)
			}
		}
		for _, attribute := range []struct {
			tag core.Tag
			vr  core.VR
		}{{tagKVP, core.VRDS}, {tagAcquisitionNumber, core.VRIS}} {
			if !dataset.Has(attribute.tag) || elementVR(dataset, attribute.tag) != attribute.vr {
				return fmt.Errorf("%w: required CT Type 2 attribute %s", ErrNonconformant, attribute.tag)
			}
		}
	case PETImageStorageUID:
		for _, tag := range []core.Tag{tagRescaleIntercept, tagRescaleSlope, tagFrameReferenceTime} {
			values, err := dataset.GetFloats(tag)
			if err != nil || len(values) != 1 {
				return fmt.Errorf("%w: required PET attribute %s", ErrNonconformant, tag)
			}
		}
		for _, tag := range []core.Tag{tagUnits, tagCountsSource, tagSeriesType, tagDecayCorrection, tagSeriesDate, tagSeriesTime} {
			if values, ok := dataset.GetStrings(tag); !ok || len(values) == 0 || cleanText(values[0]) == "" {
				return fmt.Errorf("%w: required PET attribute %s", ErrNonconformant, tag)
			}
		}
		for _, tag := range []core.Tag{tagNumberOfSlices, tagImageIndex} {
			if _, ok := uint16Value(dataset, tag); !ok {
				return fmt.Errorf("%w: required PET attribute %s", ErrNonconformant, tag)
			}
		}
	case MRImageStorageUID:
		for _, tag := range []core.Tag{tagScanningSequence, tagSequenceVariant} {
			values, ok := dataset.GetStrings(tag)
			if !ok || len(values) == 0 || strings.TrimSpace(values[0]) == "" || elementVR(dataset, tag) != core.VRCS {
				return fmt.Errorf("%w: required MR attribute %s", ErrNonconformant, tag)
			}
		}
		for _, attribute := range []struct {
			tag core.Tag
			vr  core.VR
		}{
			{tagScanOptions, core.VRCS},
			{tagMRAcquisitionType, core.VRCS},
			{tagEchoTime, core.VRDS},
			{tagEchoTrainLength, core.VRIS},
		} {
			if !dataset.Has(attribute.tag) || elementVR(dataset, attribute.tag) != attribute.vr {
				return fmt.Errorf("%w: required MR Type 2 attribute %s", ErrNonconformant, attribute.tag)
			}
		}
	}
	return nil
}

func elementVR(dataset *object.Object, tag core.Tag) core.VR {
	element, _ := dataset.Get(tag)
	return element.VR()
}

func uint16Value(dataset *object.Object, tag core.Tag) (uint16, bool) {
	element, ok := dataset.Get(tag)
	if !ok {
		return 0, false
	}
	switch value := element.Value.(type) {
	case core.Uint16Value:
		if len(value) != 1 {
			return 0, false
		}
		return value[0], true
	case core.RawValue:
		if len(value) != 2 {
			return 0, false
		}
		order := dataset.ValueByteOrder()
		if order == nil {
			order = binary.LittleEndian
		}
		return order.Uint16(value), true
	default:
		return 0, false
	}
}
