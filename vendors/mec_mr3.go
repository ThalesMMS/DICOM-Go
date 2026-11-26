package vendors

import (
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/vendors/headers"
)

// MECMR3Options select limits and an explicit Unicode/STR40 text encoding.
type MECMR3Options struct {
	Limits       Limits
	TextEncoding string
}

// MECMR3Record exposes a numerical group/key/type and owned values.
type MECMR3Record = headers.MECMR3Record

// DecodeMECMR3 decodes Original Data using strict UTF-8.
func DecodeMECMR3(raw []byte, l Limits) ([]MECMR3Record, error) {
	return headers.DecodeMECMR3(raw, headers.Limits(l))
}

// DecodeMECMR3WithOptions permits explicitly selected Shift-JIS/EUC-JP.
func DecodeMECMR3WithOptions(raw []byte, opts MECMR3Options) ([]MECMR3Record, error) {
	return headers.DecodeMECMR3WithOptions(raw, headers.MECMR3Options{Limits: headers.Limits(opts.Limits), TextEncoding: opts.TextEncoding})
}

// ToshibaMECMR3Of locates Original Data by creator.
func ToshibaMECMR3Of(o *object.Object, l Limits) ([]MECMR3Record, error) {
	return headers.ToshibaMECMR3Of(o, headers.Limits(l))
}
