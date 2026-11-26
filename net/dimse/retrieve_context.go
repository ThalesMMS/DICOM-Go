package dimse

import (
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/uid"
	"github.com/ThalesMMS/dicom-go/net/ul"
)

// StudyRootMoveSOPClassUID is the Query/Retrieve Information Model - MOVE SOP
// Class UID for the Study Root information model.
const StudyRootMoveSOPClassUID = "1.2.840.10008.5.1.4.1.2.2.2"

// PatientRootMoveSOPClassUID is the Query/Retrieve Information Model - MOVE SOP
// Class UID for the Patient Root information model.
const PatientRootMoveSOPClassUID = "1.2.840.10008.5.1.4.1.2.1.2"

// StudyRootGetSOPClassUID is the Query/Retrieve Information Model - GET SOP
// Class UID for the Study Root information model.
const StudyRootGetSOPClassUID = "1.2.840.10008.5.1.4.1.2.2.3"

// PatientRootGetSOPClassUID is the Query/Retrieve Information Model - GET SOP
// Class UID for the Patient Root information model.
const PatientRootGetSOPClassUID = "1.2.840.10008.5.1.4.1.2.1.3"

// DefaultMoveTransferSyntaxes are the transfer syntaxes proposed for C-MOVE
// identifier datasets.
var DefaultMoveTransferSyntaxes = []string{ul.ImplicitVRLittleEndian, ul.ExplicitVRLittleEndian}

// DefaultGetTransferSyntaxes are the transfer syntaxes proposed for C-GET
// identifier datasets.
var DefaultGetTransferSyntaxes = []string{ul.ImplicitVRLittleEndian, ul.ExplicitVRLittleEndian}

// StudyRootMovePresentationContext returns a presentation context proposal for
// Study Root Query/Retrieve Information Model - MOVE.
func StudyRootMovePresentationContext() ul.PresentationContext {
	return presentationContextFor(StudyRootMoveSOPClassUID, DefaultMoveTransferSyntaxes)
}

// PatientRootMovePresentationContext returns a presentation context proposal
// for Patient Root Query/Retrieve Information Model - MOVE.
func PatientRootMovePresentationContext() ul.PresentationContext {
	return presentationContextFor(PatientRootMoveSOPClassUID, DefaultMoveTransferSyntaxes)
}

// StudyRootGetPresentationContext returns a presentation context proposal for
// Study Root Query/Retrieve Information Model - GET.
func StudyRootGetPresentationContext() ul.PresentationContext {
	return presentationContextFor(StudyRootGetSOPClassUID, DefaultGetTransferSyntaxes)
}

// PatientRootGetPresentationContext returns a presentation context proposal
// for Patient Root Query/Retrieve Information Model - GET.
func PatientRootGetPresentationContext() ul.PresentationContext {
	return presentationContextFor(PatientRootGetSOPClassUID, DefaultGetTransferSyntaxes)
}

// BuildCGetPresentationContexts proposes the model's C-GET context followed by
// one context per Storage SOP Class. Each storage class has the requestor's SCP
// role and receives a copy of the transfer syntax preferences in caller order.
// The caller selects which classes and syntaxes its Storage SCP can handle.
// Canonical private UIDs are allowed for storage classes and transfer syntaxes.
func BuildCGetPresentationContexts(model QueryRetrieveModel, storageSOPClassUIDs, storageTransferSyntaxUIDs []string) ([]ul.PresentationContext, []ul.RoleSelectionItem, error) {
	switch model {
	case QueryRetrieveModelStudyRoot:
		return BuildStudyRootCGetPresentationContexts(storageSOPClassUIDs, storageTransferSyntaxUIDs)
	case QueryRetrieveModelPatientRoot:
		return buildCGetContexts(PatientRootGetPresentationContext(), storageSOPClassUIDs, storageTransferSyntaxUIDs)
	default:
		return nil, nil, fmt.Errorf("dicom dimse: unsupported C-GET model %q", model)
	}
}

// BuildStudyRootCGetPresentationContexts proposes only the Study Root C-GET
// information model, with the requested reverse Storage contexts and roles.
func BuildStudyRootCGetPresentationContexts(storageSOPClassUIDs, storageTransferSyntaxUIDs []string) ([]ul.PresentationContext, []ul.RoleSelectionItem, error) {
	return buildCGetContexts(StudyRootGetPresentationContext(), storageSOPClassUIDs, storageTransferSyntaxUIDs)
}

func buildCGetContexts(get ul.PresentationContext, storageSOPClassUIDs, storageTransferSyntaxUIDs []string) ([]ul.PresentationContext, []ul.RoleSelectionItem, error) {
	if len(storageSOPClassUIDs) == 0 || len(storageSOPClassUIDs) >= ul.MaxPresentationContexts {
		return nil, nil, fmt.Errorf("dicom dimse: C-GET needs 1 to %d Storage SOP Classes", ul.MaxPresentationContexts-1)
	}

	longestClassUID := 0
	classes := make(map[string]bool, len(storageSOPClassUIDs))
	for i, classUID := range storageSOPClassUIDs {
		_, known := uid.Dictionary.ByUID(classUID)
		if !core.IsValidUID(classUID) || known && !uid.IsStorage(classUID) {
			return nil, nil, fmt.Errorf("dicom dimse: invalid C-GET Storage SOP Class UID at index %d", i)
		}
		if classes[classUID] {
			return nil, nil, fmt.Errorf("dicom dimse: duplicate C-GET Storage SOP Class UID at index %d", i)
		}
		classes[classUID] = true
		longestClassUID = max(longestClassUID, len(classUID))
	}
	if err := validateCGetTransferSyntaxUIDs(get.TransferSyntaxUIDs, len(get.AbstractSyntaxUID)); err != nil {
		return nil, nil, err
	}
	if err := validateCGetTransferSyntaxUIDs(storageTransferSyntaxUIDs, longestClassUID); err != nil {
		return nil, nil, err
	}

	contexts := make([]ul.PresentationContext, 0, 1+len(storageSOPClassUIDs))
	roles := make([]ul.RoleSelectionItem, 0, len(storageSOPClassUIDs))
	contexts = append(contexts, get)
	for _, classUID := range storageSOPClassUIDs {
		contexts = append(contexts, presentationContextFor(classUID, storageTransferSyntaxUIDs))
		roles = append(roles, ul.RoleSelectionItem{SopClassUID: classUID, SCPRole: true})
	}
	return contexts, roles, nil
}

// BuildCGetPresentationContextsWithAlternatives also proposes separate
// single-syntax contexts for each alternative syntax of every Storage SOP
// Class. Unlike several syntaxes in a single context, this lets a peer
// accept alternatives simultaneously to send different representations of
// the same SOP Class. The expanded list leaves one context slot free for other proposals.
func BuildCGetPresentationContextsWithAlternatives(model QueryRetrieveModel, storageSOPClassUIDs, storageTransferSyntaxUIDs, alternativeSyntaxUIDs []string) ([]ul.PresentationContext, []ul.RoleSelectionItem, error) {
	contexts, roles, err := BuildCGetPresentationContexts(model, storageSOPClassUIDs, storageTransferSyntaxUIDs)
	if err != nil {
		return nil, nil, err
	}
	return appendCGetAlternativeContexts(contexts, roles, storageTransferSyntaxUIDs, alternativeSyntaxUIDs)
}

// BuildStudyRootCGetAlternativePresentationContexts adds separate Storage
// transfer-syntax offers to a Study Root C-GET proposal.
func BuildStudyRootCGetAlternativePresentationContexts(storageSOPClassUIDs, storageTransferSyntaxUIDs, alternativeSyntaxUIDs []string) ([]ul.PresentationContext, []ul.RoleSelectionItem, error) {
	contexts, roles, err := BuildStudyRootCGetPresentationContexts(storageSOPClassUIDs, storageTransferSyntaxUIDs)
	if err != nil {
		return nil, nil, err
	}
	return appendCGetAlternativeContexts(contexts, roles, storageTransferSyntaxUIDs, alternativeSyntaxUIDs)
}

func appendCGetAlternativeContexts(contexts []ul.PresentationContext, roles []ul.RoleSelectionItem, storageTransferSyntaxUIDs, alternativeSyntaxUIDs []string) ([]ul.PresentationContext, []ul.RoleSelectionItem, error) {
	if len(alternativeSyntaxUIDs) == 0 {
		return contexts, roles, nil
	}
	if 1+len(alternativeSyntaxUIDs) > (ul.MaxPresentationContexts-2)/len(roles) {
		return nil, nil, fmt.Errorf("dicom dimse: C-GET Storage SOP Classes and transfer syntax alternatives exceed presentation context capacity")
	}
	allSyntaxUIDs := append(append([]string(nil), storageTransferSyntaxUIDs...), alternativeSyntaxUIDs...)
	if err := validateCGetTransferSyntaxUIDs(allSyntaxUIDs, 64); err != nil {
		return nil, nil, err
	}
	proposals := make([]ul.PresentationContext, 0, 1+(1+len(alternativeSyntaxUIDs))*len(roles))
	proposals = append(proposals, contexts[0])
	for _, storage := range contexts[1:] {
		for _, syntaxUID := range alternativeSyntaxUIDs {
			proposals = append(proposals, presentationContextFor(storage.AbstractSyntaxUID, []string{syntaxUID}))
		}
		proposals = append(proposals, storage)
	}
	return proposals, roles, nil
}

func validateCGetTransferSyntaxUIDs(syntaxUIDs []string, classUIDLength int) error {
	if len(syntaxUIDs) == 0 {
		return fmt.Errorf("dicom dimse: C-GET requires transfer syntaxes")
	}
	seen := make(map[string]bool)
	// Presentation Context item body: four fixed bytes, the Abstract
	// Syntax sub-item, then the Transfer Syntax sub-items.
	wireBytes := 8 + classUIDLength
	for i, syntaxUID := range syntaxUIDs {
		entry, known := uid.Dictionary.ByUID(syntaxUID)
		if !core.IsValidUID(syntaxUID) || known && entry.Type != uid.TransferSyntax {
			return fmt.Errorf("dicom dimse: invalid C-GET transfer syntax UID at index %d", i)
		}
		if seen[syntaxUID] {
			return fmt.Errorf("dicom dimse: duplicate C-GET transfer syntax UID at index %d", i)
		}
		seen[syntaxUID] = true
		wireBytes += 4 + len(syntaxUID)
		if wireBytes > math.MaxUint16 {
			return fmt.Errorf("dicom dimse: C-GET transfer syntax offers exceed presentation context item capacity")
		}
	}
	return nil
}
