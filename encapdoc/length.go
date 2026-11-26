package encapdoc

import (
	"bytes"
	"context"
	"fmt"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
)

// DocumentLength describes the stored Encapsulated Document Value Field and
// its optional declared unpadded size.
type DocumentLength struct {
	StoredBytes    int64
	DeclaredBytes  int64
	HasDeclaration bool
}

// DocumentLengthError reports an unusable Encapsulated Document Length
// (0042,0015), including a contradiction with the encoded Value Field. A
// negative DeclaredBytes means the attribute does not give a unique UL value.
// The diagnostic contains no document bytes or patient metadata.
type DocumentLengthError struct {
	StoredBytes   int64
	DeclaredBytes int64
}

func (e *DocumentLengthError) Error() string {
	if e.DeclaredBytes < 0 {
		return "encapdoc: Encapsulated Document Length must be a single UL value"
	}
	return fmt.Sprintf("encapdoc: Encapsulated Document Length %d is inconsistent with the %d-byte value or its padding", e.DeclaredBytes, e.StoredBytes)
}

func (e *DocumentLengthError) Unwrap() error { return ErrInvalidDocument }

// InspectDocumentLength checks the optional length against the stored value
// (PS3.3 C.24.2 and PS3.5 6.2). A one-byte difference is valid only for an odd
// declaration followed by a single NUL. The in-memory value of an authored
// dataset can also match an odd declaration exactly before DICOM encoding.
// A deferred value is read only when the potential padding byte needs checking.
// An absent declaration leaves format-specific length inference to callers.
// The object is unchanged, and Object.CopyValueTo can replay the complete
// stored value for analysis even when this function reports a contradiction.
func InspectDocumentLength(ctx context.Context, obj *object.Object) (DocumentLength, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return DocumentLength{}, err
	}
	document, present := obj.Get(tagEncapsulatedDocument)
	if !present {
		return DocumentLength{}, fmt.Errorf("%w: Encapsulated Document is missing", ErrInvalidDocument)
	}
	var info DocumentLength
	raw, materialized := document.RawBytes()
	if materialized {
		info.StoredBytes = int64(len(raw))
	} else if document.Value == nil && document.Header.HasLength() && document.Length().IsDefined() {
		info.StoredBytes = int64(document.Length())
	} else {
		return DocumentLength{}, fmt.Errorf("%w: Encapsulated Document has no defined byte value", ErrInvalidDocument)
	}

	length, present := obj.Get(tagEncapsulatedDocumentLength)
	if !present {
		return info, nil
	}
	info.HasDeclaration = true
	info.DeclaredBytes = -1
	if length.VR() == core.VRUL && length.Length() == 4 && !obj.HasDuplicateParsedTag(tagEncapsulatedDocumentLength) {
		switch value := length.Value.(type) {
		case core.RawValue:
			if len(value) == 4 {
				info.DeclaredBytes = int64(obj.ValueByteOrder().Uint32(value))
			}
		case core.Uint32Value:
			if len(value) == 1 {
				info.DeclaredBytes = int64(value[0])
			}
		}
	}

	if info.DeclaredBytes == info.StoredBytes {
		return info, nil
	}
	if info.DeclaredBytes > 0 && info.DeclaredBytes%2 == 1 && info.StoredBytes-info.DeclaredBytes == 1 {
		if materialized {
			if raw[len(raw)-1] == 0 {
				return info, nil
			}
		} else {
			path := []parser.BulkDataPathStep{{Tag: tagEncapsulatedDocument, ItemIndex: -1}}
			var pad bytes.Buffer
			n, err := obj.CopyBulkDataRangeTo(ctx, path, info.DeclaredBytes, 1, &pad)
			if err != nil {
				return info, fmt.Errorf("encapdoc: read possible padding byte: %w", err)
			}
			if n == 1 && pad.Len() == 1 && pad.Bytes()[0] == 0 {
				return info, nil
			}
		}
	}
	return info, &DocumentLengthError{StoredBytes: info.StoredBytes, DeclaredBytes: info.DeclaredBytes}
}
