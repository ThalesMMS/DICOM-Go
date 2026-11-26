package vendors

import (
	"fmt"
	"strings"

	"github.com/ThalesMMS/dicom-go/object"
)

// VEPROData is decoded (0055,xx20) of "VEPRO VIF 3.0 DATA" or "VEPRO VIM
// 5.0 DATA", written by VEPRO archives: fixed-width text fields, trimmed of
// trailing spaces and NULs. The data fields are site-configured; they often
// hold patient demographics (identifier, name, birth date, sex, size,
// weight).
type VEPROData struct {
	ID, Version      string
	UserName         string
	UserAddress      [5]string
	RecordingDate    string
	RecordingTime    string
	RecordingPlace   string
	RecordingSource  string
	DataFields       [20]string
	StudyInstanceUID string
	SeriesUID        string
	Modality         string
}

// veproField is the offset and width of a field of the VIM data layout.
type veproField struct {
	offset, width int
	value         func(*VEPROData) *string
}

var veproFields = func() []veproField {
	fields := []veproField{
		{0, 3, func(d *VEPROData) *string { return &d.ID }},
		{3, 3, func(d *VEPROData) *string { return &d.Version }},
		{6, 32, func(d *VEPROData) *string { return &d.UserName }},
		{198, 8, func(d *VEPROData) *string { return &d.RecordingDate }},
		{206, 6, func(d *VEPROData) *string { return &d.RecordingTime }},
		{212, 64, func(d *VEPROData) *string { return &d.RecordingPlace }},
		{276, 64, func(d *VEPROData) *string { return &d.RecordingSource }},
		{1642, 64, func(d *VEPROData) *string { return &d.StudyInstanceUID }},
		{1706, 64, func(d *VEPROData) *string { return &d.SeriesUID }},
		{1770, 16, func(d *VEPROData) *string { return &d.Modality }},
	}
	for i := range 5 {
		fields = append(fields, veproField{38 + 32*i, 32, func(d *VEPROData) *string { return &d.UserAddress[i] }})
	}
	for i := range 20 {
		fields = append(fields, veproField{340 + 64*i, 64, func(d *VEPROData) *string { return &d.DataFields[i] }})
	}
	return fields
}()

// veproSize is the end of the last field (Modality).
const veproSize = 1786

// VEPRODataOf decodes the VEPRO data of dataset (VIF 3.0 before VIM 5.0).
func VEPRODataOf(dataset *object.Object, limits Limits) (*VEPROData, error) {
	limits = limits.normalized()
	raw, _, err := privateValue(dataset, 0x0055, []string{"VEPRO VIF 3.0 DATA", "VEPRO VIM 5.0 DATA"}, 0x20, limits)
	if err != nil {
		return nil, err
	}
	return ParseVEPROData(raw, limits)
}

// ParseVEPROData decodes a VEPRO data value.
func ParseVEPROData(data []byte, limits Limits) (*VEPROData, error) {
	limits = limits.normalized()
	if len(data) > limits.MaxBytes {
		return nil, fmt.Errorf("%w: %d bytes", ErrLimit, len(data))
	}
	if len(data) < veproSize {
		return nil, fmt.Errorf("%w: VEPRO data has %d bytes, %d needed", ErrInvalid, len(data), veproSize)
	}
	out := &VEPROData{}
	for _, field := range veproFields {
		*field.value(out) = strings.TrimRight(string(data[field.offset:field.offset+field.width]), " \x00")
	}
	return out, nil
}
