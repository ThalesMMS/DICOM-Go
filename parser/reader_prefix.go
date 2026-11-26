package parser

import (
	"errors"

	"github.com/ThalesMMS/dicom-go/core"
)

// ReadStop describes a deliberate root header stop. Offset uses Reader.Position's
// logical coordinate space. It never contains a value or a source reference.
type ReadStop struct {
	Stopped bool
	Header  core.ElementHeader
	Offset  int64
}

type prefixStop struct{ ReadStop }

func (s *prefixStop) Error() string { return "dicom: deliberate root header stop" }

// ReadDataSetPrefix collects completed elements before a root SelectiveStop.
// Stop is explicit in the returned descriptor; clean EOF has Stopped=false.
// Nested stop, malformed input and validation/sink readers are refused. The
// ordinary ReadDataSet APIs retain their complete-data-set contract.
func (r *Reader) ReadDataSetPrefix() (dataset core.DataSet, stop ReadStop, err error) {
	if r.selective == nil || r.validationLifecycle != nil || r.frameSink != nil || r.encapsulatedSink != nil {
		return core.DataSet{}, ReadStop{}, ErrSelectiveReaderOptions
	}
	elements, readErr := r.collectElementsPrefix(false, true)
	var stopped *prefixStop
	if errors.As(readErr, &stopped) {
		return core.DataSet{Elements: elements}, stopped.ReadStop, nil
	}
	if readErr != nil {
		return core.DataSet{}, ReadStop{}, readErr
	}
	return core.DataSet{Elements: elements}, ReadStop{}, nil
}
