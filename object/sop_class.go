package object

import (
	"errors"
	"fmt"

	"github.com/ThalesMMS/dicom-go/core"
)

// SOPClassPolicy controls resolution when the data set's SOP Class UID
// (0008,0016) and File Meta's Media Storage SOP Class UID (0002,0002) differ.
// It does not alter the file, the reader, or the writer's reconciliation policy.
type SOPClassPolicy uint8

const (
	// SOPClassRejectMismatch, the default, refuses conflicting declarations.
	SOPClassRejectMismatch SOPClassPolicy = iota
	// SOPClassPreferDataset resolves a conflict using the data set declaration.
	SOPClassPreferDataset
	// SOPClassPreferFileMeta resolves a conflict using the File Meta declaration.
	SOPClassPreferFileMeta
)

// SOPClassSource identifies the declaration used for a resolved UID.
type SOPClassSource string

const (
	SOPClassSourceDataset  SOPClassSource = "dataset"
	SOPClassSourceFileMeta SOPClassSource = "file-meta"
	SOPClassSourceAgreed   SOPClassSource = "agreed"
)

var (
	ErrSOPClassMismatch      = errors.New("dicom: File Meta and data set SOP Class UIDs differ")
	ErrInvalidSOPClassUID    = errors.New("dicom: invalid or unavailable SOP Class UID value")
	ErrInvalidSOPClassPolicy = errors.New("dicom: invalid SOP Class resolution policy")
)

// SOPClassResolution reports the normalized declarations and the selected UID.
// Mismatch remains true when a preference resolves the conflict successfully;
// consumers can diagnose inconsistent input without silently repairing it.
// UID and Source are empty on error. A nonempty declaration is reported only
// after its syntax has been checked; no SOP Class registry or IOD is validated.
type SOPClassResolution struct {
	UID         string
	Source      SOPClassSource
	DatasetUID  string
	FileMetaUID string
	Mismatch    bool
}

// ResolveSOPClass resolves the top-level SOP Class declarations without
// mutation, recursive traversal, value loading, or pixel decoding. All policies
// accept a single available declaration (including meta-only DICOMDIRs), and
// return ErrMissingSOPClassUID if both are absent or empty. Malformed,
// multivalued, non-UI, or unloaded values return ErrInvalidSOPClassUID even when
// the other declaration is valid. Trailing UI padding is normalized.
//
// A preference is only an explicit interpretation of conflicting metadata; it
// does not make the file Part 10 conformant. ValidatePart10Identity and
// ValidateFile continue reporting the original mismatch.
func (f *File) ResolveSOPClass(policy SOPClassPolicy) (SOPClassResolution, error) {
	var result SOPClassResolution
	if f == nil {
		return result, ErrNilFile
	}
	if policy > SOPClassPreferFileMeta {
		return result, ErrInvalidSOPClassPolicy
	}
	var dataErr, metaErr error
	result.DatasetUID, dataErr = declaredSOPClassUID(f.Dataset, tagSOPClassUID)
	result.FileMetaUID, metaErr = declaredSOPClassUID(f.Meta, tagMediaStorageSOPClassUID)
	if err := errors.Join(dataErr, metaErr); err != nil {
		return result, err
	}
	switch {
	case result.DatasetUID == "" && result.FileMetaUID == "":
		return result, ErrMissingSOPClassUID
	case result.DatasetUID == "":
		result.UID, result.Source = result.FileMetaUID, SOPClassSourceFileMeta
	case result.FileMetaUID == "":
		result.UID, result.Source = result.DatasetUID, SOPClassSourceDataset
	case result.DatasetUID == result.FileMetaUID:
		result.UID, result.Source = result.DatasetUID, SOPClassSourceAgreed
	default:
		result.Mismatch = true
		switch policy {
		case SOPClassRejectMismatch:
			return result, ErrSOPClassMismatch
		case SOPClassPreferDataset:
			result.UID, result.Source = result.DatasetUID, SOPClassSourceDataset
		case SOPClassPreferFileMeta:
			result.UID, result.Source = result.FileMetaUID, SOPClassSourceFileMeta
		}
	}
	return result, nil
}

func declaredSOPClassUID(o *Object, tag core.Tag) (string, error) {
	element, present := o.Get(tag)
	if !present {
		return "", nil
	}
	invalid := func() (string, error) {
		return "", fmt.Errorf("%w: %s", ErrInvalidSOPClassUID, tag)
	}
	if element.VR() != core.VRUI {
		return invalid()
	}
	var value string
	switch v := element.Value.(type) {
	case core.RawValue:
		if len(v) > 64 {
			return invalid()
		}
		value = string(v)
	case core.StringValue:
		if len(v) == 0 {
			return "", nil
		}
		if len(v) != 1 || len(v[0]) > 64 {
			return invalid()
		}
		value = v[0]
	default:
		return invalid()
	}
	value = core.NormalizeUID(value)
	if value != "" && !core.IsValidUID(value) {
		return invalid()
	}
	return value, nil
}
