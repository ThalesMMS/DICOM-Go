package validation

import (
	"context"
	"errors"
	"unsafe"

	"github.com/ThalesMMS/dicom-go/core"
	dicomenc "github.com/ThalesMMS/dicom-go/encoding"
)

// validateWriteView shares every built-in check and traversal with the public
// validator. Only its unconsumed detached dataset result is omitted. Contextual
// caller functions use the owned fallback, without purity assumptions.
func validateWriteView(ctx context.Context, ds core.DataSet, opts Options, budget *writeBudget, inherited dicomenc.SpecificCharacterSet) (Result, error) {
	if err := budget.reserve(0, int64(len(opts.DataSetRules))*64+256, 0); err != nil {
		return Result{}, err
	}
	builder, err := newReportBuilder(opts)
	if err != nil {
		return Result{}, err
	}
	builder.writeBudget = budget
	f, err := budget.admitTraversal(ds)
	if err != nil {
		return Result{}, err
	}
	borrowed := opts.ResolveVR == nil && len(opts.DataSetRules) == 0 && opts.Hooks == nil && f.known
	working := ds
	if !borrowed {
		if err = budget.admitCopy(ds, 1); err != nil {
			return Result{}, err
		}
		working, err = cloneDataSetWithLimits(ctx, ds, budget.maxDepth, budget.maxElements)
		if err != nil {
			return Result{}, err
		}
	}
	session := validationSession{ctx: ctx, opts: opts, builder: builder, maxDepth: budget.maxDepth, maxElements: budget.maxElements, reportOnly: borrowed, writeBudget: budget}
	elements, err := session.processElements(working, nil, 0, inherited, true)
	if !borrowed {
		working.Elements = elements
	}
	if err == nil && !builder.shouldStop(opts) {
		err = session.validateRequiredUIDs(working)
	}
	if builder.limitErr != nil {
		err = builder.limitErr
	}
	if copyErr := budget.admitReport(builder.report); copyErr != nil {
		return Result{}, copyErr
	}
	result := Result{DataSet: working, Report: builder.report.Clone()}
	if err != nil {
		return result, err
	}
	if opts.Mode == ModeStrict && result.Report.HasErrors() {
		return result, &ValidationError{Report: result.Report.Clone()}
	}
	return result, nil
}

// ErrWriteValidationScope means a serialization change invalidated a previous
// context proof that cannot be re-established without replaying caller code or
// changing validation phases. No IOD/purity/cache claim is implied.
var ErrWriteValidationScope = errors.New("dicom serialization change invalidates an opaque or character-set validation scope")

// WriteContext is ordinal metadata for one dataset during one write. Its value
// backing is borrowed and must remain immutable. It is not a cross-call cache.
type WriteContext struct {
	elements  []core.Element
	filtered  []bool
	root      bool
	processed int
	owner     *WriteOperation
	epoch     uint64
}

func (w *WriteOperation) NewContext(ds core.DataSet, root bool) (*WriteContext, error) {
	if w.budget == nil {
		return nil, ErrInvalidPolicy
	}
	bytes, e := writeProduct(len(ds.Elements), uintptr(unsafeElementSize()+1))
	if e != nil {
		return nil, e
	}
	bytes, e = writeAdd(bytes, int64(unsafe.Sizeof(WriteContext{})))
	if e != nil {
		return nil, e
	}
	if e = w.budget.reserve(0, bytes, len(ds.Elements)+1); e != nil {
		return nil, e
	}
	elements := make([]core.Element, len(ds.Elements))
	copy(elements, ds.Elements)
	return &WriteContext{elements: elements, filtered: make([]bool, len(elements)), root: root, owner: w, epoch: w.epoch}, nil
}

// ApplyChange invalidates by ordinal, even for same-length replacement. It
// checks newly introduced presence/ordering defects without replaying rules or
// duplicating all unchanged findings. Element content is checked separately.
func (w *WriteOperation) ApplyChange(c *WriteContext, index int, original, current core.Element, filtered, priorProof bool, path Path) error {
	if w == nil || w.budget == nil {
		return ErrInvalidPolicy
	}
	if c != nil && (c.owner != w || c.epoch != w.epoch) {
		return ErrWriteValidationScope
	}
	footprint, err := w.budget.scanWriteDataSet(core.DataSet{Elements: []core.Element{current}})
	if err != nil {
		return err
	}
	if err = w.budget.reserve(0, footprint.scratch, 0); err != nil {
		return err
	}
	if priorProof && (w.opts.ResolveVR != nil || len(w.opts.DataSetRules) != 0) {
		return ErrWriteValidationScope
	}
	if c == nil {
		return nil
	}
	if index < 0 || index >= len(c.elements) {
		return ErrWriteValidationScope
	}
	if (original.Tag() == tagSpecificCharacterSet || current.Tag() == tagSpecificCharacterSet) && (priorProof || c.processed > 0) {
		return ErrWriteValidationScope
	}
	c.elements[index] = current
	c.filtered[index] = filtered
	report := Report{}
	if !filtered && original.Tag() != current.Tag() {
		duplicate := false
		for i, el := range c.elements {
			if i != index && !c.filtered[i] && el.Tag() == current.Tag() {
				duplicate = true
				break
			}
		}
		if duplicate {
			report.Findings = append(report.Findings, Finding{Path: path, Tag: current.Tag(), VR: current.VR(), Severity: SeverityError, Code: CodeDuplicateElement, Message: "duplicate element in the same data set"})
		}
		previous := -1
		next := -1
		for i := index - 1; i >= 0; i-- {
			if !c.filtered[i] {
				previous = i
				break
			}
		}
		for i := index + 1; i < len(c.elements); i++ {
			if !c.filtered[i] {
				next = i
				break
			}
		}
		if previous >= 0 && current.Tag().Less(c.elements[previous].Tag()) || next >= 0 && c.elements[next].Tag().Less(current.Tag()) {
			report.Findings = append(report.Findings, Finding{Path: path, Tag: current.Tag(), VR: current.VR(), Severity: SeverityWarning, Code: CodeElementOrder, Message: "element is encoded out of tag order"})
		}
	}
	if c.root {
		affected := false
		for _, tag := range w.opts.RequiredUIDs {
			if original.Tag() == tag || current.Tag() == tag {
				affected = true
				break
			}
		}
		if affected {
			bytes, e := writeProduct(len(c.elements), unsafeElementSize())
			if e != nil {
				return e
			}
			if e = w.budget.reserve(0, bytes+256, 0); e != nil {
				return e
			}
			elements := make([]core.Element, 0, len(c.elements))
			for i, el := range c.elements {
				if !c.filtered[i] {
					elements = append(elements, el)
				}
			}
			opts := w.opts
			opts.DataSetRules = nil
			builder, e := newReportBuilder(opts)
			if e != nil {
				return e
			}
			builder.writeBudget = w.budget
			session := validationSession{ctx: w.ctx, opts: opts, builder: builder}
			session.validateRequiredUIDs(core.DataSet{Elements: elements})
			if builder.limitErr != nil {
				return builder.limitErr
			}
			requiredReport := builder.report
			requiredReport.Findings = append(report.Findings, requiredReport.Findings...)
			report = requiredReport
		}
	}
	if err := w.addReport(report); err != nil {
		return err
	}
	if w.opts.Mode == ModeStrict && report.HasErrors() {
		return &ValidationError{Report: w.Report()}
	}
	return nil
}
func (c *WriteContext) Mark(index int) {
	if c != nil && index >= c.processed {
		c.processed = index + 1
	}
}
