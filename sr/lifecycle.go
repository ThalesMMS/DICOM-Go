package sr

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

// ErrDocumentLifecycle reports a completion, verification or revision the
// document's state does not allow.
var ErrDocumentLifecycle = errors.New("dicom/sr: document lifecycle operation not allowed")

var (
	tagCompletionFlagDescription = core.NewTag(0x0040, 0xA492)
	tagVerifyingObserverSequence = core.NewTag(0x0040, 0xA073)
	tagVerifyingObserverName     = core.NewTag(0x0040, 0xA075)
	tagVerifyingObserverCode     = core.NewTag(0x0040, 0xA088)
	tagVerifyingOrganization     = core.NewTag(0x0040, 0xA027)
	tagVerificationDateTime      = core.NewTag(0x0040, 0xA030)
	tagPredecessorDocuments      = core.NewTag(0x0040, 0xA360)
)

// VerifyingObserver is an item of the Verifying Observer Sequence: the
// person who verified the document, their organization and when.
type VerifyingObserver struct {
	Name         string // Verifying Observer Name (PN, Type 1)
	Organization string // Verifying Organization (LO, Type 1)
	// DateTime is the Verification DateTime (DT, Type 1).
	DateTime string
	// Code is the Verifying Observer Identification Code (Type 2).
	Code CodedEntry
}

// DocumentReference identifies an instance by its study, series, SOP Class
// and SOP Instance (Hierarchical SOP Instance Reference Macro).
type DocumentReference struct {
	StudyInstanceUID  string
	SeriesInstanceUID string
	SOPClassUID       string
	SOPInstanceUID    string
}

// Complete marks the document complete, for now and ever: the Completion
// Flag becomes COMPLETE with an optional description (at most 64
// characters). It fails for Key Object Selection documents, which have no
// Completion Flag, and for documents already complete.
func (d *Document) Complete(description string) error {
	if err := d.checkLifecycle(); err != nil {
		return err
	}
	if strings.TrimSpace(d.CompletionFlag) == completionComplete {
		return fmt.Errorf("%w: the document is already complete", ErrDocumentLifecycle)
	}
	if len(description) > 64 || strings.ContainsAny(description, "\\\r\n") {
		return fmt.Errorf("%w: Completion Flag Description is not an LO value", ErrDocumentLifecycle)
	}
	d.CompletionFlag = completionComplete
	d.CompletionFlagDescription = description
	return nil
}

// Verify records that observer verified the complete document: observer is
// appended to the Verifying Observer Sequence and the Verification Flag
// becomes VERIFIED. Name and Organization are required; an empty DateTime
// selects the current time.
func (d *Document) Verify(observer VerifyingObserver) error {
	if err := d.checkLifecycle(); err != nil {
		return err
	}
	if strings.TrimSpace(d.CompletionFlag) != completionComplete {
		return fmt.Errorf("%w: only a complete document can be verified", ErrDocumentLifecycle)
	}
	if observer.DateTime == "" {
		observer.DateTime = time.Now().Format("20060102150405")
	}
	if err := validateVerifyingObserver(observer); err != nil {
		return fmt.Errorf("%w: %w", ErrDocumentLifecycle, err)
	}
	d.VerifyingObservers = append(d.VerifyingObservers, observer)
	d.VerificationFlag = verificationVerified
	return nil
}

// Revise turns the complete document into a new revision of itself: the
// current document is appended to the Predecessor Documents Sequence, the
// document gets sopInstanceUID (a generated UID when empty), and it becomes
// PARTIAL and UNVERIFIED again, without completion description, verifying
// observers or content date and time, as DCMTK createRevisedVersion does.
// The current document needs its study, series, SOP Class and SOP Instance
// UIDs.
func (d *Document) Revise(sopInstanceUID string) error {
	if err := d.checkLifecycle(); err != nil {
		return err
	}
	if strings.TrimSpace(d.CompletionFlag) != completionComplete {
		return fmt.Errorf("%w: only a complete document can be revised", ErrDocumentLifecycle)
	}
	current := DocumentReference{StudyInstanceUID: d.StudyInstanceUID, SeriesInstanceUID: d.SeriesInstanceUID, SOPClassUID: d.SOPClassUID, SOPInstanceUID: d.SOPInstanceUID}
	if !completeDocumentReference(current) {
		return fmt.Errorf("%w: a revision needs the study, series, SOP Class and SOP Instance UIDs of the document", ErrDocumentLifecycle)
	}
	if sopInstanceUID == "" {
		var err error
		if sopInstanceUID, err = newDocumentUID(); err != nil {
			return err
		}
	}
	if !core.IsValidUID(sopInstanceUID) || sopInstanceUID == d.SOPInstanceUID {
		return fmt.Errorf("%w: the revision needs a new valid SOP Instance UID", ErrDocumentLifecycle)
	}
	d.PredecessorDocuments = append(d.PredecessorDocuments, current)
	d.SOPInstanceUID = sopInstanceUID
	d.CompletionFlag, d.CompletionFlagDescription = completionPartial, ""
	d.VerificationFlag, d.VerifyingObservers = verificationUnverified, nil
	d.ContentDate, d.ContentTime = "", ""
	return nil
}

func (d *Document) checkLifecycle() error {
	if d == nil {
		return fmt.Errorf("%w: document is nil", ErrDocumentLifecycle)
	}
	if d.SOPClassUID == KeyObjectSelectionDocumentStorage {
		return fmt.Errorf("%w: a Key Object Selection document has no completion or verification", ErrDocumentLifecycle)
	}
	return nil
}

func newDocumentUID() (string, error) {
	return core.NewUID()
}

func validateVerifyingObserver(observer VerifyingObserver) error {
	if strings.TrimSpace(observer.Name) == "" || strings.TrimSpace(observer.Organization) == "" || strings.TrimSpace(observer.DateTime) == "" {
		return errors.New("a verifying observer needs a name, an organization and a verification date and time")
	}
	return nil
}

func completeDocumentReference(ref DocumentReference) bool {
	return clean(ref.StudyInstanceUID) != "" && clean(ref.SeriesInstanceUID) != "" && clean(ref.SOPClassUID) != "" && clean(ref.SOPInstanceUID) != ""
}

// lifecycleElements encodes the completion description, verifying observers
// and predecessor documents of the SR Document General module.
func lifecycleElements(description string, observers []VerifyingObserver, predecessors []DocumentReference) ([]core.Element, error) {
	var elements []core.Element
	if description != "" {
		elements = append(elements, strElem(tagCompletionFlagDescription, core.VRLO, description))
	}
	if len(observers) > 0 {
		items := make([]core.DataSet, 0, len(observers))
		for i, observer := range observers {
			if err := validateVerifyingObserver(observer); err != nil {
				return nil, fmt.Errorf("%w: verifying observer %d: %w", ErrInvalidDocument, i, err)
			}
			code := seqElement(tagVerifyingObserverCode)
			if !observer.Code.Empty() {
				code = codeSequence(tagVerifyingObserverCode, observer.Code)
			}
			items = append(items, core.DataSet{Elements: []core.Element{
				strElem(tagVerifyingOrganization, core.VRLO, observer.Organization),
				strElem(tagVerificationDateTime, core.VRDT, observer.DateTime),
				strElem(tagVerifyingObserverName, core.VRPN, observer.Name),
				code,
			}})
		}
		elements = append(elements, seqElement(tagVerifyingObserverSequence, items...))
	}
	if len(predecessors) > 0 {
		refs := make([]ImageReference, 0, len(predecessors))
		for i, ref := range predecessors {
			if !completeDocumentReference(ref) {
				return nil, fmt.Errorf("%w: predecessor document %d needs its study, series, SOP Class and SOP Instance UIDs", ErrInvalidDocument, i)
			}
			refs = append(refs, ImageReference{StudyInstanceUID: ref.StudyInstanceUID, SeriesInstanceUID: ref.SeriesInstanceUID, SOPClassUID: ref.SOPClassUID, SOPInstanceUID: ref.SOPInstanceUID})
		}
		elements = append(elements, hierarchicalReferenceSequence(tagPredecessorDocuments, refs))
	}
	return elements, nil
}

// readLifecycle decodes the completion description, verifying observers and
// predecessor documents.
func readLifecycle(dec *decoder, obj *object.Object) (string, []VerifyingObserver, []DocumentReference) {
	description := dec.string(obj, tagCompletionFlagDescription)
	var observers []VerifyingObserver
	items, _ := dec.sequence(obj, tagVerifyingObserverSequence)
	for _, item := range items {
		observer := VerifyingObserver{
			Name:         dec.string(item, tagVerifyingObserverName),
			Organization: dec.string(item, tagVerifyingOrganization),
			DateTime:     dec.string(item, tagVerificationDateTime),
		}
		observer.Code, _ = decodeCode(dec, item, tagVerifyingObserverCode)
		observers = append(observers, observer)
	}
	var predecessors []DocumentReference
	studies, _ := dec.sequence(obj, tagPredecessorDocuments)
	for _, study := range studies {
		studyUID := dec.string(study, tagStudyInstanceUID)
		seriesItems, _ := dec.sequence(study, tagReferencedSeriesSequence)
		for _, series := range seriesItems {
			seriesUID := dec.string(series, tagSeriesInstanceUID)
			sops, _ := dec.sequence(series, tagRefSOPSequence)
			for _, sop := range sops {
				predecessors = append(predecessors, DocumentReference{
					StudyInstanceUID: studyUID, SeriesInstanceUID: seriesUID,
					SOPClassUID: dec.string(sop, tagRefSOPClassUID), SOPInstanceUID: dec.string(sop, tagRefSOPInstanceUID),
				})
			}
		}
	}
	return description, observers, predecessors
}
