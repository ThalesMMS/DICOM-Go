package sr

import (
	"github.com/ThalesMMS/dicom-go/codes/dcm"
)

var (
	KOSTitleOfInterest = FromCode(dcm.OfInterest)
	// KOSTitleManifest is (113030, DCM, "Manifest") of CID 7010, the title of
	// a KOS that lists the instances of a study for exchange (DCMTK
	// CODE_DCM_Manifest; IHE XDS-I.b and MADO manifests).
	KOSTitleManifest  = FromCode(dcm.Manifest)
	KOSFindingConcept = FromCode(dcm.Finding)
)

// NewKeyObjectSelection builds a KOS document that references the provided
// images and attaches optional textual findings by SOP Instance UID. A
// finding is a TEXT acquisition context of its IMAGE: the only per-image TEXT
// the Key Object Selection IOD allows (PS3.3 Table A.35.4-2, since CP-2595).
// The document belongs to the study of the images when they share one, and
// to a new series whose UID derives from sopInstanceUID; callers may set
// both. A reference's RetrieveURL is written in its evidence series. The
// title is Of Interest; callers may set another of CID 7010, such as
// KOSTitleManifest.
func NewKeyObjectSelection(sopInstanceUID string, refs []ImageReference, findings map[string]string) *Document {
	contentDate, contentTime := currentContentDateTime()
	doc := &Document{
		SOPClassUID:       KeyObjectSelectionDocumentStorage,
		SOPInstanceUID:    sopInstanceUID,
		SeriesInstanceUID: documentSeriesUID(KeyObjectSelectionDocumentStorage, sopInstanceUID, ""),
		Modality:          "KO",
		Title:             KOSTitleOfInterest,
		ContentDate:       contentDate,
		ContentTime:       contentTime,
	}
	for i, ref := range refs {
		if i == 0 {
			doc.StudyInstanceUID = ref.StudyInstanceUID
		} else if ref.StudyInstanceUID != doc.StudyInstanceUID {
			doc.StudyInstanceUID = ""
			break
		}
	}
	for _, ref := range refs {
		item := ContentItem{
			ValueType:        ValueImage,
			RelationshipType: RelationshipContains,
			Image: ImageReference{
				StudyInstanceUID:  ref.StudyInstanceUID,
				SeriesInstanceUID: ref.SeriesInstanceUID,
				SOPClassUID:       ref.SOPClassUID,
				SOPInstanceUID:    ref.SOPInstanceUID,
				Frames:            append([]int(nil), ref.Frames...),
				RetrieveURL:       ref.RetrieveURL,
			},
		}
		if findings != nil {
			if finding := findings[ref.SOPInstanceUID]; finding != "" {
				item.Children = []ContentItem{{
					ValueType:        ValueText,
					RelationshipType: RelationshipHasAcqContext,
					ConceptName:      KOSFindingConcept,
					Text:             finding,
				}}
			}
		}
		doc.Content = append(doc.Content, item)
	}
	return doc
}

// KeyObjectSelectionImages extracts image references from a KOS document.
func KeyObjectSelectionImages(doc *Document) []ImageReference {
	if doc == nil {
		return nil
	}
	refs := make([]ImageReference, 0, len(doc.Content))
	for _, item := range doc.Content {
		if item.ValueType != ValueImage {
			continue
		}
		ref := item.Image
		ref.Frames = append([]int(nil), ref.Frames...)
		refs = append(refs, ref)
	}
	return refs
}

// KeyObjectSelectionFindings extracts textual findings by SOP Instance UID.
func KeyObjectSelectionFindings(doc *Document) map[string]string {
	out := map[string]string{}
	if doc == nil {
		return out
	}
	for _, item := range doc.Content {
		if item.ValueType != ValueImage || item.Image.SOPInstanceUID == "" {
			continue
		}
		for _, child := range item.Children {
			if child.ValueType == ValueText && child.Text != "" {
				out[item.Image.SOPInstanceUID] = child.Text
				break
			}
		}
	}
	return out
}
