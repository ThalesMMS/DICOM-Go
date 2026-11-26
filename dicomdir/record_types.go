package dicomdir

import "github.com/ThalesMMS/dicom-go/dictionary/uid"

// RecordType is a Directory Record Type value from DICOM PS3.3 Annex F.
type RecordType string

const (
	RecordTypePatient              RecordType = "PATIENT"
	RecordTypeStudy                RecordType = "STUDY"
	RecordTypeSeries               RecordType = "SERIES"
	RecordTypeImage                RecordType = "IMAGE"
	RecordTypeRTDose               RecordType = "RT DOSE"
	RecordTypeRTStructureSet       RecordType = "RT STRUCTURE SET"
	RecordTypeRTPlan               RecordType = "RT PLAN"
	RecordTypeRTTreatmentRecord    RecordType = "RT TREAT RECORD"
	RecordTypePresentation         RecordType = "PRESENTATION"
	RecordTypeWaveform             RecordType = "WAVEFORM"
	RecordTypeSRDocument           RecordType = "SR DOCUMENT"
	RecordTypeKeyObjectDocument    RecordType = "KEY OBJECT DOC"
	RecordTypeSpectroscopy         RecordType = "SPECTROSCOPY"
	RecordTypeRawData              RecordType = "RAW DATA"
	RecordTypeRegistration         RecordType = "REGISTRATION"
	RecordTypeFiducial             RecordType = "FIDUCIAL"
	RecordTypeEncapsulatedDocument RecordType = "ENCAP DOC"
	RecordTypeValueMap             RecordType = "VALUE MAP"
	RecordTypeStereometric         RecordType = "STEREOMETRIC"
	RecordTypeSurface              RecordType = "SURFACE"
	RecordTypeSurfaceScan          RecordType = "SURFACE SCAN"
	RecordTypeTract                RecordType = "TRACT"
	RecordTypeAnnotation           RecordType = "ANNOTATION"
	RecordTypeWaveformPresentation RecordType = "WF PRESENTATION"
	RecordTypeHangingProtocol      RecordType = "HANGING PROTOCOL"
	RecordTypePalette              RecordType = "PALETTE"
	RecordTypeImplant              RecordType = "IMPLANT"
	RecordTypeImplantAssembly      RecordType = "IMPLANT ASSY"
	RecordTypeImplantGroup         RecordType = "IMPLANT GROUP"
	RecordTypePlan                 RecordType = "PLAN"
	RecordTypeMeasurement          RecordType = "MEASUREMENT"
	RecordTypeAssessment           RecordType = "ASSESSMENT"
	RecordTypeRadiotherapy         RecordType = "RADIOTHERAPY"
	RecordTypeInventory            RecordType = "INVENTORY"
)

// recordTypeForSOPClass maps only SOP Classes with an explicitly supported
// PS3.3 Annex F Directory Record Type. In particular, an unrecognized Storage
// SOP Class must not silently become an IMAGE record.
func recordTypeForSOPClass(sopClassUID string) (RecordType, bool) {
	entry, ok := uid.Dictionary.ByUID(sopClassUID)
	if !ok || entry.Type != uid.SOPClass {
		return "", false
	}

	switch entry.Keyword {
	case "HangingProtocolStorage":
		return RecordTypeHangingProtocol, true
	case "ColorPaletteStorage":
		return RecordTypePalette, true
	case "GenericImplantTemplateStorage":
		return RecordTypeImplant, true
	case "ImplantAssemblyTemplateStorage":
		return RecordTypeImplantAssembly, true
	case "ImplantTemplateGroupStorage":
		return RecordTypeImplantGroup, true
	case "InventoryStorage":
		return RecordTypeInventory, true
	case "ContentAssessmentResultsStorage":
		return RecordTypeAssessment, true
	case "RTBeamsDeliveryInstructionStorage", "RTBrachyApplicationSetupDeliveryInstructionStorage":
		return RecordTypePlan, true
	case "LensometryMeasurementsStorage", "AutorefractionMeasurementsStorage", "KeratometryMeasurementsStorage",
		"SubjectiveRefractionMeasurementsStorage", "VisualAcuityMeasurementsStorage", "OphthalmicAxialMeasurementsStorage",
		"IntraocularLensCalculationsStorage", "OphthalmicVisualFieldStaticPerimetryMeasurementsStorage":
		return RecordTypeMeasurement, true
	case "RTPhysicianIntentStorage", "RTSegmentAnnotationStorage", "RTRadiationSetStorage",
		"CArmPhotonElectronRadiationStorage", "TomotherapeuticRadiationStorage", "RoboticArmRadiationStorage",
		"RTRadiationRecordSetStorage", "RTRadiationSalvageRecordStorage", "TomotherapeuticRadiationRecordStorage",
		"CArmPhotonElectronRadiationRecordStorage", "RoboticRadiationRecordStorage", "RTRadiationSetDeliveryInstructionStorage",
		"RTTreatmentPreparationStorage", "RTPatientPositionAcquisitionInstructionStorage":
		return RecordTypeRadiotherapy, true
	case // Image Directory Records.
		"HardcopyGrayscaleImageStorage",
		"HardcopyColorImageStorage",
		"ComputedRadiographyImageStorage",
		"DigitalXRayImageStorageForPresentation",
		"DigitalXRayImageStorageForProcessing",
		"DigitalMammographyXRayImageStorageForPresentation",
		"DigitalMammographyXRayImageStorageForProcessing",
		"DigitalIntraOralXRayImageStorageForPresentation",
		"DigitalIntraOralXRayImageStorageForProcessing",
		"XRayAngiographicImageStorage",
		"EnhancedXAImageStorage",
		"XRayRadiofluoroscopicImageStorage",
		"EnhancedXRFImageStorage",
		"XRayAngiographicBiPlaneImageStorage",
		"PositronEmissionTomographyImageStorage",
		"LegacyConvertedEnhancedPETImageStorage",
		"XRay3DAngiographicImageStorage",
		"XRay3DCraniofacialImageStorage",
		"BreastTomosynthesisImageStorage",
		"BreastProjectionXRayImageStorageForPresentation",
		"BreastProjectionXRayImageStorageForProcessing",
		"EnhancedPETImageStorage",
		"IntravascularOpticalCoherenceTomographyImageStorageForPresentation",
		"IntravascularOpticalCoherenceTomographyImageStorageForProcessing",
		"CTImageStorage",
		"EnhancedCTImageStorage",
		"LegacyConvertedEnhancedCTImageStorage",
		"CTImageStorageForProcessing",
		"EnhancedCTImageStorageForProcessing",
		"LegacyConvertedEnhancedCTImageStorageForProcessing",
		"NuclearMedicineImageStorage",
		"UltrasoundMultiFrameImageStorageRetired",
		"UltrasoundMultiFrameImageStorage",
		"MRImageStorage",
		"EnhancedMRImageStorage",
		"EnhancedMRColorImageStorage",
		"LegacyConvertedEnhancedMRImageStorage",
		"RTImageStorage",
		"EnhancedRTImageStorage",
		"EnhancedContinuousRTImageStorage",
		"NuclearMedicineImageStorageRetired",
		"DICOSCTImageStorage",
		"DICOSDigitalXRayImageStorageForPresentation",
		"DICOSDigitalXRayImageStorageForProcessing",
		"UltrasoundImageStorageRetired",
		"UltrasoundImageStorage",
		"PhotoacousticImageStorage",
		"EddyCurrentImageStorage",
		"EddyCurrentMultiFrameImageStorage",
		"ThermographyImageStorage",
		"ThermographyMultiFrameImageStorage",
		"SecondaryCaptureImageStorage",
		"MultiFrameSingleBitSecondaryCaptureImageStorage",
		"MultiFrameGrayscaleByteSecondaryCaptureImageStorage",
		"MultiFrameGrayscaleWordSecondaryCaptureImageStorage",
		"MultiFrameTrueColorSecondaryCaptureImageStorage",
		"VLImageStorageTrial",
		"VLEndoscopicImageStorage",
		"VideoEndoscopicImageStorage",
		"VLMicroscopicImageStorage",
		"VideoMicroscopicImageStorage",
		"VLSlideCoordinatesMicroscopicImageStorage",
		"VLPhotographicImageStorage",
		"VideoPhotographicImageStorage",
		"OphthalmicPhotography8BitImageStorage",
		"OphthalmicPhotography16BitImageStorage",
		"OphthalmicTomographyImageStorage",
		"WideFieldOphthalmicPhotographyStereographicProjectionImageStorage",
		"WideFieldOphthalmicPhotography3DCoordinatesImageStorage",
		"OphthalmicOpticalCoherenceTomographyEnFaceImageStorage",
		"OphthalmicOpticalCoherenceTomographyBscanVolumeAnalysisStorage",
		"VLWholeSlideMicroscopyImageStorage",
		"DermoscopicPhotographyImageStorage",
		"ConfocalMicroscopyImageStorage",
		"ConfocalMicroscopyTiledPyramidalImageStorage",
		"VLMultiFrameImageStorageTrial",
		"SegmentationStorage",
		"LabelMapSegmentationStorage",
		"HeightMapSegmentationStorage",
		"ParametricMapStorage",
		"EnhancedUSVolumeStorage",
		"OphthalmicThicknessMapStorage",
		"CornealTopographyMapStorage":
		return RecordTypeImage, true

	case "RTDoseStorage":
		return RecordTypeRTDose, true

	case "RTStructureSetStorage":
		return RecordTypeRTStructureSet, true

	case "RTPlanStorage", "RTIonPlanStorage":
		return RecordTypeRTPlan, true

	case "RTBeamsTreatmentRecordStorage",
		"RTBrachyTreatmentRecordStorage",
		"RTTreatmentSummaryRecordStorage":
		return RecordTypeRTTreatmentRecord, true

	case "GrayscaleSoftcopyPresentationStateStorage",
		"ColorSoftcopyPresentationStateStorage",
		"PseudoColorSoftcopyPresentationStateStorage",
		"BlendingSoftcopyPresentationStateStorage",
		"XAXRFGrayscaleSoftcopyPresentationStateStorage",
		"GrayscalePlanarMPRVolumetricPresentationStateStorage",
		"CompositingPlanarMPRVolumetricPresentationStateStorage",
		"AdvancedBlendingPresentationStateStorage",
		"VolumeRenderingVolumetricPresentationStateStorage",
		"SegmentedVolumeRenderingVolumetricPresentationStateStorage",
		"MultipleVolumeRenderingVolumetricPresentationStateStorage",
		"VariableModalityLUTSoftcopyPresentationStateStorage",
		"BasicStructuredDisplayStorage":
		return RecordTypePresentation, true

	case "WaveformStorageTrial",
		"TwelveLeadECGWaveformStorage",
		"GeneralECGWaveformStorage",
		"AmbulatoryECGWaveformStorage",
		"General32bitECGWaveformStorage",
		"HemodynamicWaveformStorage",
		"CardiacElectrophysiologyWaveformStorage",
		"BasicVoiceAudioWaveformStorage",
		"GeneralAudioWaveformStorage",
		"ArterialPulseWaveformStorage",
		"RespiratoryWaveformStorage",
		"MultichannelRespiratoryWaveformStorage",
		"RoutineScalpElectroencephalogramWaveformStorage",
		"ElectromyogramWaveformStorage",
		"ElectrooculogramWaveformStorage",
		"SleepElectroencephalogramWaveformStorage",
		"BodyPositionWaveformStorage",
		"UltrasoundWaveformStorage":
		return RecordTypeWaveform, true

	case "TextSRStorageTrial",
		"AudioSRStorageTrial",
		"DetailSRStorageTrial",
		"ComprehensiveSRStorageTrial",
		"BasicTextSRStorage",
		"EnhancedSRStorage",
		"ComprehensiveSRStorage",
		"Comprehensive3DSRStorage",
		"ExtensibleSRStorage",
		"ProcedureLogStorage",
		"MammographyCADSRStorage",
		"ChestCADSRStorage",
		"XRayRadiationDoseSRStorage",
		"RadiopharmaceuticalRadiationDoseSRStorage",
		"ColonCADSRStorage",
		"ImplantationPlanSRStorage",
		"AcquisitionContextSRStorage",
		"SimplifiedAdultEchoSRStorage",
		"PatientRadiationDoseSRStorage",
		"PlannedImagingAgentAdministrationSRStorage",
		"PerformedImagingAgentAdministrationSRStorage",
		"EnhancedXRayRadiationDoseSRStorage",
		"WaveformAnnotationSRStorage":
		return RecordTypeSRDocument, true

	case "KeyObjectSelectionDocumentStorage":
		return RecordTypeKeyObjectDocument, true

	case "MRSpectroscopyStorage":
		return RecordTypeSpectroscopy, true

	case "RawDataStorage":
		return RecordTypeRawData, true

	case "SpatialRegistrationStorage", "DeformableSpatialRegistrationStorage":
		return RecordTypeRegistration, true

	case "SpatialFiducialsStorage":
		return RecordTypeFiducial, true

	case "EncapsulatedPDFStorage",
		"EncapsulatedCDAStorage",
		"EncapsulatedSTLStorage",
		"EncapsulatedOBJStorage",
		"EncapsulatedMTLStorage":
		return RecordTypeEncapsulatedDocument, true

	case "RealWorldValueMappingStorage":
		return RecordTypeValueMap, true

	case "StereometricRelationshipStorage":
		return RecordTypeStereometric, true

	case "SurfaceSegmentationStorage":
		return RecordTypeSurface, true

	case "SurfaceScanMeshStorage", "SurfaceScanPointCloudStorage":
		return RecordTypeSurfaceScan, true

	case "TractographyResultsStorage":
		return RecordTypeTract, true

	case "MicroscopyBulkSimpleAnnotationsStorage":
		return RecordTypeAnnotation, true

	case "WaveformPresentationStateStorage", "WaveformAcquisitionPresentationStateStorage":
		return RecordTypeWaveformPresentation, true
	}

	return "", false
}

func rootFileRecordType(typ RecordType) bool {
	switch typ {
	case RecordTypeHangingProtocol, RecordTypePalette, RecordTypeImplant, RecordTypeImplantAssembly, RecordTypeImplantGroup, RecordTypeInventory:
		return true
	}
	return false
}

func standardFileRecordType(typ RecordType) bool {
	switch typ {
	case RecordTypeImage, RecordTypeRTDose, RecordTypeRTStructureSet, RecordTypeRTPlan, RecordTypeRTTreatmentRecord,
		RecordTypePresentation, RecordTypeWaveform, RecordTypeSRDocument, RecordTypeKeyObjectDocument,
		RecordTypeSpectroscopy, RecordTypeRawData, RecordTypeRegistration, RecordTypeFiducial, RecordTypeEncapsulatedDocument,
		RecordTypeValueMap, RecordTypeStereometric, RecordTypeSurface, RecordTypeSurfaceScan, RecordTypeTract,
		RecordTypeAnnotation, RecordTypeWaveformPresentation, RecordTypePlan, RecordTypeMeasurement, RecordTypeAssessment,
		RecordTypeRadiotherapy:
		return true
	}
	return rootFileRecordType(typ)
}
