package dimse

import (
	"github.com/ThalesMMS/dicom-go/dictionary/uid"
	"github.com/ThalesMMS/dicom-go/encapdoc"
	"github.com/ThalesMMS/dicom-go/gsps"
	"github.com/ThalesMMS/dicom-go/microscopy"
	"github.com/ThalesMMS/dicom-go/parametricmap"
	"github.com/ThalesMMS/dicom-go/rtdose"
	"github.com/ThalesMMS/dicom-go/rtstruct"
	"github.com/ThalesMMS/dicom-go/seg"
	"github.com/ThalesMMS/dicom-go/spatialreg"
	"github.com/ThalesMMS/dicom-go/sr"
	"github.com/ThalesMMS/dicom-go/vps"
	"github.com/ThalesMMS/dicom-go/waveform"
)

// libraryStorageSOPClassUIDs are the Storage SOP Classes this library reads
// or writes; a C-GET requestor proposes them before the rest of the catalog.
var libraryStorageSOPClassUIDs = append([]string{
	"1.2.840.10008.5.1.4.1.1.1",
	"1.2.840.10008.5.1.4.1.1.2",
	"1.2.840.10008.5.1.4.1.1.2.1",
	"1.2.840.10008.5.1.4.1.1.4",
	"1.2.840.10008.5.1.4.1.1.4.1",
	"1.2.840.10008.5.1.4.1.1.6.1",
	"1.2.840.10008.5.1.4.1.1.7",
	"1.2.840.10008.5.1.4.1.1.7.1",
	"1.2.840.10008.5.1.4.1.1.7.2",
	"1.2.840.10008.5.1.4.1.1.7.3",
	"1.2.840.10008.5.1.4.1.1.7.4",
	"1.2.840.10008.5.1.4.1.1.12.1",
	"1.2.840.10008.5.1.4.1.1.12.2",
	"1.2.840.10008.5.1.4.1.1.20",
	"1.2.840.10008.5.1.4.1.1.128",
	"1.2.840.10008.5.1.4.1.1.1.1",
	"1.2.840.10008.5.1.4.1.1.1.1.1",
	seg.SegmentationStorage,
	seg.LabelMapSegmentationStorage,
	sr.EnhancedSRStorage,
	sr.ComprehensiveSRStorage,
	sr.Comprehensive3DSRStorage,
	sr.KeyObjectSelectionDocumentStorage,
	rtdose.RTDoseStorage,
	parametricmap.ParametricMapStorage,
	encapdoc.EncapsulatedPDFStorage,
	encapdoc.EncapsulatedCDAStorage,
	encapdoc.EncapsulatedSTLStorage,
	encapdoc.EncapsulatedOBJStorage,
	encapdoc.EncapsulatedMTLStorage,
	microscopy.VLWholeSlideMicroscopyImageStorage,
	microscopy.ConfocalMicroscopyTiledPyramidalStorage,
	microscopy.MicroscopyBulkSimpleAnnotationsStorage,
	gsps.GrayscaleSoftcopyPresentationStateStorage,
	rtstruct.RTStructureSetStorage,
	spatialreg.SpatialRegistrationStorage,
	spatialreg.DeformableSpatialRegistrationStorage,
	vps.GrayscalePlanarMPRVolumetricPresentationStateStorage,
	vps.CompositingPlanarMPRVolumetricPresentationStateStorage,
	vps.VolumeRenderingVolumetricPresentationStateStorage,
	vps.SegmentedVolumeRenderingVolumetricPresentationStateStorage,
	vps.MultipleVolumeRenderingVolumetricPresentationStateStorage,
}, waveform.SupportedStorageSOPClassUIDs()...)

// defaultStorageSOPClassUIDs is the catalog of every standard Storage SOP
// Class (dictionary/uid, generated from PS3.4 Tables B.5-1, B.6-1 and GG.3-1
// and PS3.6 Table A-1), followed by any class the library reads that the
// catalog lacks.
var defaultStorageSOPClassUIDs = func() []string {
	out := uid.StorageSOPClassUIDs()
	for _, sopClassUID := range libraryStorageSOPClassUIDs {
		if !uid.IsStorage(sopClassUID) {
			out = append(out, sopClassUID)
		}
	}
	return out
}()

// DefaultStorageSOPClassUIDs returns the Storage SOP Class UIDs accepted by
// the library's default Storage SCP profile: every standard Storage SOP
// Class, current and retired (#1046).
func DefaultStorageSOPClassUIDs() []string {
	return append([]string(nil), defaultStorageSOPClassUIDs...)
}

// DefaultCGetStorageSOPClassUIDs returns the Storage SOP Classes a C-GET
// requestor proposes (with the SCP role) by default. An association carries
// at most 128 presentation contexts, so this is uid.StorageSCUSubset: the
// classes this library reads first, then the clinical catalog by priority,
// at most uid.MaxSCUStorageContexts. Callers that know the classes of the
// study to retrieve should propose those instead.
func DefaultCGetStorageSOPClassUIDs() []string {
	return uid.StorageSCUSubset(libraryStorageSOPClassUIDs...)
}
