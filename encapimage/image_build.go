package encapimage

import (
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
	"slices"
	"strings"
)

type imageLayout struct {
	rows, columns, samples, bits, signed int
	allocated, frameCount                int
	method                               string
	process, photometric                 string
	lossless                             bool
	lossyUnknown                         bool
	payloadBytes                         int
}

// buildImage is shared by the existing JPEG constructor and the bounded importer.
// It retains the JPEG constructor's common modules and identity behavior.
func buildImage(opts JPEGOptions, layout imageLayout, value core.Value, syntax transfer.Syntax, report Report, icc []byte) (*object.File, Report, error) {
	sopClass := strings.TrimSpace(opts.SOPClassUID)
	if sopClass == "" {
		sopClass = SecondaryCaptureImageStorage
	}
	burned := strings.ToUpper(strings.TrimSpace(opts.BurnedInAnnotation))
	laterality := strings.ToUpper(strings.TrimSpace(opts.Laterality))
	photometric := layout.photometric
	uid := func(value, keyword string) (string, error) {
		if value = strings.TrimSpace(value); value != "" {
			return value, nil
		}
		generated, err := newUID()
		if err != nil {
			return "", err
		}
		report.GeneratedUIDs = append(report.GeneratedUIDs, keyword)
		return generated, nil
	}
	study, err := uid(opts.StudyInstanceUID, "StudyInstanceUID")
	if err != nil {
		return nil, report, err
	}
	series, err := uid(opts.SeriesInstanceUID, "SeriesInstanceUID")
	if err != nil {
		return nil, report, err
	}
	instance, err := uid(opts.SOPInstanceUID, "SOPInstanceUID")
	if err != nil {
		return nil, report, err
	}

	samples := layout.samples
	allocated := layout.allocated
	if allocated == 0 {
		allocated = 8
		if layout.bits > 8 {
			allocated = 16
		}
	}
	vlp := sopClass == VLPhotographicImageStorage || sopClass == VideoPhotographicImageStorage
	modality := opts.Modality
	imageType := opts.ImageType
	if vlp {
		modality = defaultString(modality, "XC")
		imageType = defaultString(imageType, `DERIVED\SECONDARY`)
	} else {
		modality = defaultString(modality, "OT")
	}

	text := []string{opts.PatientName, opts.PatientID, opts.StudyID, opts.StudyDescription,
		opts.AccessionNumber, opts.ReferringPhysicianName, opts.SeriesDescription, opts.Manufacturer}
	elements := []core.Element{
		stringElement(tagSOPClassUID, core.VRUI, sopClass),
		stringElement(tagSOPInstanceUID, core.VRUI, instance),
		stringElement(tagStudyDate, core.VRDA, opts.StudyDate),
		stringElement(tagStudyTime, core.VRTM, opts.StudyTime),
		stringElement(tagAccessionNumber, core.VRSH, opts.AccessionNumber),
		stringElement(tagModality, core.VRCS, modality),
		stringElement(tagReferringPhysicianName, core.VRPN, opts.ReferringPhysicianName),
		stringElement(tagPatientName, core.VRPN, opts.PatientName),
		stringElement(tagPatientID, core.VRLO, opts.PatientID),
		stringElement(tagPatientBirthDate, core.VRDA, opts.PatientBirthDate),
		stringElement(tagPatientSex, core.VRCS, opts.PatientSex),
		stringElement(tagStudyInstanceUID, core.VRUI, study),
		stringElement(tagSeriesInstanceUID, core.VRUI, series),
		stringElement(tagStudyID, core.VRSH, opts.StudyID),
		stringElement(tagSeriesNumber, core.VRIS, defaultString(opts.SeriesNumber, "1")),
		stringElement(tagLaterality, core.VRCS, laterality),
		stringElement(tagInstanceNumber, core.VRIS, defaultString(opts.InstanceNumber, "1")),
		stringElement(tagPatientOrientation, core.VRCS, ""),
		usElement(tagSamplesPerPixel, samples),
		stringElement(tagPhotometric, core.VRCS, photometric),
		usElement(tagRows, layout.rows),
		usElement(tagColumns, layout.columns),
		usElement(tagBitsAllocated, allocated),
		usElement(tagBitsStored, layout.bits),
		usElement(tagHighBit, layout.bits-1),
		usElement(tagPixelRepresentation, layout.signed),
		{
			Header: core.ElementHeader{Tag: tagPixelData, VR: core.VROB, Length: core.UndefinedLength},
			Value:  value,
		},
	}
	optional := func(tag core.Tag, vr core.VR, value string) {
		if value = strings.TrimSpace(value); value != "" {
			elements = append(elements, stringElement(tag, vr, value))
		}
	}
	if imageType = strings.TrimSpace(imageType); imageType != "" {
		elements = append(elements, core.Element{
			Header: core.ElementHeader{Tag: tagImageType, VR: core.VRCS},
			Value:  core.StringValue(strings.Split(imageType, `\`)),
		})
	}
	optional(tagContentDate, core.VRDA, opts.ContentDate)
	optional(tagContentTime, core.VRTM, opts.ContentTime)
	optional(tagStudyDescription, core.VRLO, opts.StudyDescription)
	optional(tagSeriesDescription, core.VRLO, opts.SeriesDescription)
	optional(tagBurnedInAnnotation, core.VRCS, burned)
	if vlp {
		elements = append(elements,
			stringElement(tagManufacturer, core.VRLO, opts.Manufacturer),
			core.Element{
				Header: core.ElementHeader{Tag: tagAcquisitionContextSeq, VR: core.VRSQ, Length: core.UndefinedLength, LengthSet: true},
				Value:  core.SequenceValue{},
			})
	} else {
		elements = append(elements, stringElement(tagConversionType, core.VRCS, defaultString(opts.ConversionType, "WSD")))
		optional(tagManufacturer, core.VRLO, opts.Manufacturer)
	}
	if samples > 1 {
		elements = append(elements, usElement(tagPlanarConfiguration, 0))
	}
	if layout.lossyUnknown {
		elements = append(elements, stringElement(tagLossyImageCompression, core.VRCS, ""))
	} else if layout.lossless {
		elements = append(elements, stringElement(tagLossyImageCompression, core.VRCS, "00"))
	} else {
		uncompressed := float64(layout.rows) * float64(layout.columns) * float64(samples) * float64(allocated/8) * float64(max(1, layout.frameCount))
		elements = append(elements,
			stringElement(tagLossyImageCompression, core.VRCS, "01"),
			stringElement(tagLossyCompressionRatio, core.VRDS, formatRatio(uncompressed/float64(layout.payloadBytes))),
			stringElement(tagLossyCompressionMethod, core.VRCS, defaultString(layout.method, "ISO_10918_1")))
	}
	if icc != nil && !opts.DiscardICCProfile {
		if len(icc)%2 != 0 {
			icc = append(icc, 0)
		}
		elements = append(elements, core.NewRawElement(tagICCProfile, core.VROB, icc))
		report.ICCProfileCopied = true
	}
	for _, value := range text {
		if !isASCII(value) {
			elements = append(elements, stringElement(tagSpecificCharacterSet, core.VRCS, "ISO_IR 192"))
			break
		}
	}
	if _, native := value.(core.RawValue); native {
		for i := range elements {
			if elements[i].Header.Tag == tagPixelData {
				elements[i].Header.Length = 0
				if allocated > 8 {
					elements[i].Header.VR = core.VROW
				}
			}
		}
	}
	dataset := object.FromElements(elements, std.Dictionary)
	if opts.Common.Source != nil {
		// Unset patient and study fields, and a generated Study Instance
		// UID, give way to the source's.
		unset := map[core.Tag]string{
			tagPatientName: opts.PatientName, tagPatientID: opts.PatientID, tagPatientBirthDate: opts.PatientBirthDate,
			tagPatientSex: opts.PatientSex, tagStudyDate: opts.StudyDate, tagStudyTime: opts.StudyTime,
			tagAccessionNumber: opts.AccessionNumber, tagReferringPhysicianName: opts.ReferringPhysicianName,
			tagStudyID: opts.StudyID, tagStudyInstanceUID: opts.StudyInstanceUID,
		}
		for tag, value := range unset {
			if strings.TrimSpace(value) == "" {
				dataset.Remove(tag)
			}
		}
		if sourceUID, _ := opts.Common.Source.GetString(tagStudyInstanceUID); strings.Trim(sourceUID, " \x00") != "" {
			report.GeneratedUIDs = slices.DeleteFunc(report.GeneratedUIDs, func(keyword string) bool {
				return keyword == "StudyInstanceUID"
			})
		}
	}
	if err := opts.Common.Apply(dataset, object.InheritWithoutFrameOfReference); err != nil {
		return nil, Report{}, err
	}
	return &object.File{
		Dataset:        dataset,
		TransferSyntax: syntax,
	}, report, nil
}
