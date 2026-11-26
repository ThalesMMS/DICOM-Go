package validation

import (
	"context"
	"errors"
	"unsafe"

	"github.com/ThalesMMS/dicom-go/core"
	dicomenc "github.com/ThalesMMS/dicom-go/encoding"
)

// WriteOperation is a bounded, report-consuming validation operation for a
// serializer. Unlike public detached validation results, its fast built-in view
// borrows immutable value backing until EndWrite. It is not concurrency safe.
// Dictionaries, resolvers, rules, hooks and opaque values retain their public
// contracts; caller code must not mutate borrowed input concurrently.
type WriteOperation struct {
	*Operation
	limits        WriteLimits
	budget        *writeBudget
	configWork    int64
	locationWork  int64
	locationNodes int
	epoch         uint64
}

func NewWriteOperation(ctx context.Context, opts Options) (*WriteOperation, error) {
	limits, err := normalizeWriteLimits(opts.WriteLimits)
	if err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if len(opts.DataSetRules) > limits.MaxRuleRegistrations {
		return nil, ErrValidationLimit
	}
	depth, elements := opts.MaxDepth, opts.MaxElements
	if depth == 0 {
		depth = DefaultMaxDepth
	}
	if elements == 0 {
		elements = DefaultMaxElements
	}
	if depth < 0 || elements < 0 || depth > int(^uint(0)>>1)/128 {
		return nil, ErrInvalidPolicy
	}
	b := &writeBudget{ctx: ctx, limits: limits, maxDepth: depth, maxElements: elements}
	// Freeze caller slice policy before validation's rule-name map allocation.
	ruleBytes, err := writeProduct(len(opts.DataSetRules), unsafe.Sizeof(DataSetRuleRegistration{}))
	if err != nil {
		return nil, err
	}
	uidBytes, err := writeProduct(len(opts.RequiredUIDs), unsafe.Sizeof(core.Tag{}))
	if err != nil {
		return nil, err
	}
	mapBytes, err := writeProduct(len(opts.DataSetRules), 64)
	if err != nil {
		return nil, err
	}
	configBytes, err := writeAdd(ruleBytes, uidBytes)
	if err == nil {
		configBytes, err = writeAdd(configBytes, mapBytes)
	}
	if err != nil {
		return nil, err
	}
	if err = b.reserve(0, configBytes, len(opts.RequiredUIDs)); err != nil {
		return nil, err
	}
	opts.DataSetRules = cloneExact(opts.DataSetRules)
	opts.RequiredUIDs = cloneExact(opts.RequiredUIDs)
	hooks, err := opts.Hooks.snapshotForWrite(limits.MaxHookRegistrations, b)
	if err != nil {
		return nil, err
	}
	opts.Hooks = hooks
	if err = ValidateOptions(opts); err != nil {
		return nil, err
	}
	operation := &Operation{ctx: ctx, opts: opts, hooks: hooks, locations: make(map[string][]int64)}
	return &WriteOperation{Operation: operation, limits: limits, configWork: b.work}, nil
}
func (w *WriteOperation) BeginWrite() error {
	if w == nil || w.budget != nil {
		return ErrInvalidPolicy
	}
	depth, elements := w.opts.MaxDepth, w.opts.MaxElements
	if depth == 0 {
		depth = DefaultMaxDepth
	}
	if elements == 0 {
		elements = DefaultMaxElements
	}
	b := &writeBudget{ctx: w.ctx, limits: w.limits, maxDepth: depth, maxElements: elements}
	if err := b.reserve(0, w.configWork+w.locationWork, w.locationNodes); err != nil {
		return err
	}
	w.epoch++
	w.budget = b
	return nil
}

// EndWrite releases scope references/counters. The bounded report and observed
// offsets remain owned by the operation; no payload/cache/source is retained.
func (w *WriteOperation) EndWrite() {
	if w != nil {
		w.budget = nil
	}
}
func (w *WriteOperation) Active() bool { return w != nil && w.budget != nil }
func (w *WriteOperation) HandleWrite(event HookEvent) (HookResult, error) {
	if w == nil || w.budget == nil {
		return HookResult{}, ErrInvalidPolicy
	}
	// There is no consumer of a detached result at an unregistered writer point.
	// Public Operation.Handle and HookChain.Run retain their detached contract.
	if !w.HasHooks(event.Point) {
		event.Element = nil
		event.DataSet = nil
	}
	bytes, err := writeProduct(len(event.Path), unsafe.Sizeof(PathStep{}))
	if err != nil {
		return HookResult{}, err
	}
	// Includes Path.String's components/key and conservative map/offset backing.
	location, err := writeScale(bytes, 16)
	if err == nil {
		location, err = writeAdd(location, 128)
	}
	if err != nil {
		return HookResult{}, err
	}
	if len(w.opts.DataSetRules) > int(^uint(0)>>1)/128 {
		return HookResult{}, ErrValidationLimit
	}
	overhead, err := writeAdd(location, int64(len(w.hooksSlice()))*8+int64(len(w.opts.DataSetRules))*128+256)
	if err != nil {
		return HookResult{}, err
	}
	if err = w.budget.reserve(0, overhead, 1); err != nil {
		return HookResult{}, err
	}
	if event.OffsetSet && len(event.Path) > 0 {
		w.locationWork += location
		w.locationNodes++
	}
	if err = w.budget.admitReport(w.report); err != nil {
		return HookResult{}, err
	}
	return w.Operation.handle(event, w.budget)
}
func (w *WriteOperation) hooksSlice() []*registeredHook {
	if w.hooks == nil {
		return nil
	}
	return w.hooks.hooks
}
func (w *WriteOperation) addReport(report Report) error {
	if err := w.budget.reserve(0, int64(len(w.opts.DataSetRules))*64+256, 0); err != nil {
		return err
	}
	if err := w.budget.admitReport(report); err != nil {
		return err
	}
	if err := w.budget.admitReport(w.report); err != nil {
		return err
	}
	w.AddReport(report)
	return nil
}

// ValidateElement consumes a report only. RequiredUIDs remain root-dataset
// policy, exactly as in the validated writer's element path.
func (w *WriteOperation) ValidateElement(element core.Element) error {
	return w.validateElementInCharacterSet(element, dicomenc.DefaultCharacterSet)
}
func (w *WriteOperation) ValidateChangedElement(element core.Element, characterSet dicomenc.SpecificCharacterSet) error {
	return w.validateElementInCharacterSet(element, characterSet)
}
func (w *WriteOperation) validateElementInCharacterSet(element core.Element, characterSet dicomenc.SpecificCharacterSet) error {
	if w == nil || w.budget == nil {
		return ErrInvalidPolicy
	}
	opts := w.opts
	opts.Hooks = nil
	opts.RequiredUIDs = nil
	result, err := validateWriteView(w.ctx, core.DataSet{Elements: []core.Element{element}}, opts, w.budget, characterSet)
	if mergeErr := w.addReport(result.Report); mergeErr != nil {
		return mergeErr
	}
	if errors.Is(err, ErrValidationFailed) {
		return &ValidationError{Report: w.Report()}
	}
	return err
}

// ValidateDataSet preserves lifecycle and caller rule order. Its returned
// dataset is a borrowed immutable view only when no contextual caller function
// or opaque value requires the owned fallback; callers must not mutate it.
func (w *WriteOperation) ValidateDataSet(dataset core.DataSet) (Result, error) {
	if w == nil || w.budget == nil {
		return Result{}, ErrInvalidPolicy
	}
	if w.stopped {
		return w.stoppedResult(dataset)
	}
	if _, err := w.HandleWrite(HookEvent{Point: HookPreValidation, DataSet: &dataset}); err != nil {
		return Result{DataSet: dataset, Report: w.Report()}, err
	}
	if w.stopped {
		return w.stoppedResult(dataset)
	}
	opts := w.opts
	opts.Hooks = nil
	result, ruleErr := validateWriteView(w.ctx, dataset, opts, w.budget, dicomenc.DefaultCharacterSet)
	if err := w.addReport(result.Report); err != nil {
		return Result{}, err
	}
	if ruleErr != nil && !errors.Is(ruleErr, ErrValidationFailed) {
		return Result{DataSet: result.DataSet, Report: w.Report()}, ruleErr
	}
	if w.stopped {
		return w.stoppedResult(result.DataSet)
	}
	if _, err := w.HandleWrite(HookEvent{Point: HookDataSetComplete, DataSet: &result.DataSet}); err != nil {
		return Result{DataSet: result.DataSet, Report: w.Report()}, err
	}
	if _, err := w.HandleWrite(HookEvent{Point: HookPostValidation, DataSet: &result.DataSet}); err != nil {
		return Result{DataSet: result.DataSet, Report: w.Report()}, err
	}
	result.Report = w.Report()
	if w.opts.Mode == ModeStrict && result.Report.HasErrors() {
		return result, &ValidationError{Report: result.Report.Clone()}
	}
	return result, nil
}
func unsafeElementSize() uintptr { return unsafe.Sizeof(core.Element{}) }
