package dicomdir

import (
	"errors"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

// MRDR was retired. Its use is an explicit compatibility choice, not current
// standard authoring. New file-sets never create MRDR groups.
var ErrLegacyMRDRRequiresOptIn = errors.New("dicomdir: legacy MRDR requires explicit compatibility mode")

const recordTypeMRDR RecordType = "MRDR"

var (
	tagMRDRDirectoryRecordOffset = core.NewTag(0x0004, 0x1504)
	tagNumberOfReferences        = core.NewTag(0x0004, 0x1600)
)

func inspectLegacyMRDR(items map[uint32]*object.Object, legacy bool) (map[uint32]*object.Object, error) {
	out := make(map[uint32]*object.Object)
	for offset, item := range items {
		typ, _ := item.GetString(tagDirectoryRecordType)
		if typ != "MRDR" {
			if item.Has(tagMRDRDirectoryRecordOffset) && !legacy {
				return nil, ErrLegacyMRDRRequiresOptIn
			}
			continue
		}
		if !legacy {
			return nil, ErrLegacyMRDRRequiresOptIn
		}
		next, nok := uint32Value(item, tagOffsetNextDirectoryRecord)
		lower, lok := uint32Value(item, tagOffsetLowerLevelRecord)
		flag, fok := uint16Value(item, tagRecordInUseFlag)
		count, cok := uint32Value(item, tagNumberOfReferences)
		if !nok || !lok || next != 0 || lower != 0 || !fok || !cok || (flag == 0) != (count == 0) {
			return nil, ErrInvalidRecord
		}
		ids, ok := stringsWithVR(item, tagReferencedFileID, core.VRCS)
		if flag != 0 && !ok {
			return nil, ErrInvalidRecord
		}
		if _, err := NewFileID(ids...); flag != 0 && err != nil {
			return nil, err
		}
		for _, tag := range []core.Tag{tagReferencedSOPClassUIDInFile, tagReferencedSOPInstanceUIDInFile,
			tagReferencedTransferSyntaxUIDInFile, tagReferencedRelatedGeneralSOPClassUIDInFile, tagMRDRDirectoryRecordOffset} {
			if item.Has(tag) {
				return nil, ErrInvalidRecord
			}
		}
		out[offset] = item
	}
	return out, nil
}

func logicalMRDRTarget(item *object.Object, mrdrs map[uint32]*object.Object, legacy bool) (uint32, bool, error) {
	if !item.Has(tagMRDRDirectoryRecordOffset) {
		return 0, false, nil
	}
	if !legacy {
		return 0, false, ErrLegacyMRDRRequiresOptIn
	}
	offset, ok := uint32Value(item, tagMRDRDirectoryRecordOffset)
	if !ok || offset == 0 || mrdrs[offset] == nil || item.Has(tagReferencedFileID) {
		return 0, false, ErrInvalidRecord
	}
	flag, ok := uint16Value(mrdrs[offset], tagRecordInUseFlag)
	if !ok || flag == 0 {
		return 0, false, ErrInvalidRecord
	}
	return offset, true, nil
}
