package object

import (
	"fmt"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// EncodeFileMetaHeader returns the start of a Part 10 file whose data set is
// written separately, byte for byte in syntax: a zero 128-byte preamble,
// "DICM" and the File Meta Information group for the SOP Class and Instance
// UIDs, the transfer syntax, this implementation's identity and the AE
// titles. Bit-preserving C-STORE reception (#1050) writes it before the
// received P-DATA bytes, as DCMTK's storescp --bit-preserving does.
func EncodeFileMetaHeader(sopClassUID, sopInstanceUID string, syntax transfer.Syntax, aeTitles FileMetaAETitles) ([]byte, error) {
	sopClassUID, sopInstanceUID = transfer.NormalizeUID(sopClassUID), transfer.NormalizeUID(sopInstanceUID)
	if !core.IsValidUID(sopClassUID) {
		return nil, fmt.Errorf("%w: %q", ErrMissingSOPClassUID, sopClassUID)
	}
	if !core.IsValidUID(sopInstanceUID) {
		return nil, fmt.Errorf("%w: %q", ErrMissingSOPInstanceUID, sopInstanceUID)
	}
	if !core.IsValidUID(syntax.UID) {
		return nil, fmt.Errorf("dicom: invalid transfer syntax UID %q", syntax.UID)
	}
	meta := New(nil)
	meta.Put(newStringElement(tagMediaStorageSOPClassUID, core.VRUI, sopClassUID))
	meta.Put(newStringElement(tagMediaStorageSOPInstanceUID, core.VRUI, sopInstanceUID))
	prepared, err := prepareFileMeta(&File{Meta: meta}, syntax, fileMetaOptions{mode: FileMetaCreate, aeTitles: aeTitles})
	if err != nil {
		return nil, err
	}
	header := make([]byte, part10PreambleLength, part10PreambleLength+len(part10Magic)+len(prepared.encoded))
	header = append(header, part10Magic...)
	return append(header, prepared.encoded...), nil
}
