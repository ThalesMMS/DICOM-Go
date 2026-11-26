package pixeldata

import (
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// DiagnosticNativePixelDataUnderEncapsulated: the transfer syntax declared
// compressed Pixel Data but it held a defined-length native value of exactly
// the expected size (PS3.5 A.4 requires the encapsulated format);
// DecodeRecovery.NativePixelDataUnderEncapsulated read it as native.
const DiagnosticNativePixelDataUnderEncapsulated = "native-pixel-data-under-encapsulated-syntax"

// nativeUnderEncapsulated returns the syntax to decode dataset with: Explicit
// VR Little Endian, the value encoding of every encapsulated syntax, when
// source declares an encapsulated syntax but Pixel Data is a defined-length
// value of exactly the native size (one pad byte allowed for an odd size),
// as GDCM reads it; otherwise source.
func nativeUnderEncapsulated(dataset *object.Object, source transfer.Syntax) (transfer.Syntax, bool) {
	if !source.Encapsulated || source.UID == transfer.EncapsulatedUncompressedExplicitVRLittleEndian.UID {
		return source, false
	}
	pixel, err := ExtractView(dataset)
	if err != nil || pixel.Encapsulated {
		return source, false
	}
	metadata, err := ExtractMetadata(dataset)
	if err != nil {
		return source, false
	}
	total, size := metadata.TotalSize(), int64(len(pixel.Raw))
	if total <= 0 || size != total && !(total%2 == 1 && size == total+1) {
		return source, false
	}
	return transfer.ExplicitVRLittleEndian, true
}
