package parametricmap

import (
	"errors"
	"fmt"

	"github.com/ThalesMMS/dicom-go/pixeldata/display"
)

// WindowDomain samples at most 4096 resolved values without filling the frame
// cache. It is a useful interval, not a claim to contain every voxel. Display
// normalization still uses the exact decoded frame range at render time.
func (m *Map) WindowDomain(frame int) (display.WindowDomain, error) {
	if m == nil || !m.payloadRetained || frame < 0 || frame >= len(m.Frames) || m.Rows <= 0 || m.Columns <= 0 {
		return display.WindowDomain{}, fmt.Errorf("%w: window domain frame", ErrInvalidObject)
	}
	if m.Rows > int(^uint(0)>>1)/m.Columns {
		return display.WindowDomain{}, ErrInvalidObject
	}
	count := m.Rows * m.Columns
	n := min(count, 4096)
	values := make([]float64, 0, n)
	for sample := 0; sample < n; sample++ {
		pixel := 0
		if n > 1 {
			pixel = sample*((count-1)/(n-1)) + sample*((count-1)%(n-1))/(n-1)
		}
		stored, e := m.storedValue(frame, pixel)
		if e != nil {
			return display.WindowDomain{}, e
		}
		if !Measured(stored) || (m.Padding != nil && m.Padding.Contains(stored)) {
			continue
		}
		value, e := m.Frames[frame].applyMapping(stored)
		if e != nil {
			if errors.Is(e, ErrOutsideMapping) || errors.Is(e, errMappingOverflow) {
				continue
			}
			return display.WindowDomain{}, e
		}
		values = append(values, value)
	}
	return display.MeasuredWindowDomain(values, m.Frames[frame].Mapping.Units.String()), nil
}
