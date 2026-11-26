package iod

import (
	"errors"
	"strings"

	"github.com/ThalesMMS/dicom-go/internal/iodmodules"
	"github.com/ThalesMMS/dicom-go/object"
)

// ConditionState distinguishes a proved requirement from unresolved context.
type ConditionState uint8

const (
	ConditionUnknown ConditionState = iota
	ConditionFalse
	ConditionTrue
)

// ExternalFamily selects an audited external fact, not an arbitrary predicate.
type ExternalFamily string

const (
	PerformedProcedureStepParticipation ExternalFamily = "performed-procedure-step-participation"
	ContributingSOPInstanceConsistency  ExternalFamily = "contributing-sop-instance-consistency"
)

// ExternalFact is a caller's resolution of one normative condition at Path.
// True consistency means the attribute is present and consistent across ALL
// contributing SOP Instances of this item; false means that conjunction is
// proved false. It does not verify the attribute's actual clinical value.
// Rule is the stable source-row identifier reported by a prior contextual
// validation. EvidenceSHA256 identifies caller-retained evidence, not PHI.
type ExternalFact struct {
	Family         ExternalFamily
	Rule, Path     string
	State          ConditionState
	EvidenceSHA256 string
}

// ContradictoryExternalContext is an error, never a silently false condition.
const ContradictoryExternalContext Kind = "contradictory-external-context"

// RuleSource preserves a condition's normative origin and cross references.
type RuleSource struct {
	Rule            string
	Source          ConstraintSource
	CrossReferences []string
	ExternalFamily  ExternalFamily
}

func publicRuleSource(source iodmodules.RuleSource) RuleSource {
	return RuleSource{Rule: source.Rule, Source: constraintSource(source.Source), CrossReferences: append([]string(nil), source.CrossReferences...), ExternalFamily: ExternalFamily(source.ExternalFamily)}
}

var ErrExternalContext = errors.New("dicom/validation/iod: invalid external context")

// ExternalContext is an immutable, finite snapshot. It retains no objects,
// resolvers, files or callbacks and performs no external I/O.
type ExternalContext struct{ facts map[string][]ExternalFact }

func factKey(family ExternalFamily, rule, path string) string {
	return string(family) + "\x00" + rule + "\x00" + path
}

// NewExternalContext copies at most256 facts, with at most64KiB of text.
// Conflicting facts are preserved for an explicit error at the affected rule;
// they are not resolved using insertion order or last-wins behavior.
func NewExternalContext(facts []ExternalFact) (*ExternalContext, error) {
	if len(facts) > 256 {
		return nil, ErrExternalContext
	}
	c := &ExternalContext{facts: map[string][]ExternalFact{}}
	total := 0
	for _, f := range facts {
		if f.Family != PerformedProcedureStepParticipation && f.Family != ContributingSOPInstanceConsistency {
			return nil, ErrExternalContext
		}
		if f.State > ConditionTrue || len(f.Rule) == 0 || len(f.Rule) > 512 || len(f.Path) == 0 || len(f.Path) > 1024 || len(f.EvidenceSHA256) != 64 {
			return nil, ErrExternalContext
		}
		for _, text := range []string{f.Rule, f.Path} {
			for _, char := range text {
				if char < 32 || char > 126 {
					return nil, ErrExternalContext
				}
			}
		}
		for _, char := range f.EvidenceSHA256 {
			if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f') {
				return nil, ErrExternalContext
			}
		}
		total += len(f.Rule) + len(f.Path) + len(f.EvidenceSHA256) + len(f.Family)
		if total > 65536 {
			return nil, ErrExternalContext
		}
		// Clone short substrings so an admitted fact cannot retain a large
		// caller-owned backing buffer. Family is canonicalized to constants.
		f.Rule = strings.Clone(f.Rule)
		f.Path = strings.Clone(f.Path)
		f.EvidenceSHA256 = strings.Clone(f.EvidenceSHA256)
		if f.Family == PerformedProcedureStepParticipation {
			f.Family = PerformedProcedureStepParticipation
		} else {
			f.Family = ContributingSOPInstanceConsistency
		}
		key := factKey(f.Family, f.Rule, f.Path)
		c.facts[key] = append(c.facts[key], f)
	}
	return c, nil
}

// ConditionCheck records a checked or pending condition without clinical values.
type ConditionCheck struct {
	Rule, Path, Module string
	Source             RuleSource
	State              ConditionState
	Mode               string // local, external, pending
	Reason             string // fixed diagnostic, never caller text or values
	EvidenceSHA256     []string
}

// ContextReport leaves the legacy Report/API unchanged. Pending and Truncated
// are not conformant passes, even when the legacy Report.Errors() is empty.
type ContextReport struct {
	Report                              Report
	Validated                           bool
	Conditions                          []ConditionCheck
	Local, External, Pending, Truncated int
}

// Complete reports whether every encountered condition was resolved without
// an error or dropped finding/check. It is not full IOD/clinical conformance.
func (r ContextReport) Complete() bool {
	return r.Validated && r.Pending == 0 && r.Truncated == 0 && r.Report.Truncated == 0 && len(r.Report.Errors()) == 0
}

type externalValidation struct {
	context                           *ExternalContext
	checks                            []ConditionCheck
	seen                              map[string]bool
	local, external, pending, dropped int
	max                               int
}

// ValidateWithExternalContext opts into typed, caller-resolved external facts.
// A nil context records missing context explicitly. Validate remains unchanged.
// maxChecks0 means10000; positive values above65536 are rejected.
func ValidateWithExternalContext(obj *object.Object, opts Options, context *ExternalContext, maxChecks int) (ContextReport, error) {
	if maxChecks < 0 || maxChecks > 65536 {
		return ContextReport{}, ErrExternalContext
	}
	if maxChecks == 0 {
		maxChecks = 10000
	}
	v := &externalValidation{context: context, seen: map[string]bool{}, max: maxChecks}
	report, err := validateWithExternal(obj, opts, v)
	return ContextReport{Report: report, Validated: err == nil, Conditions: v.checks, Local: v.local, External: v.external, Pending: v.pending, Truncated: v.dropped}, err
}

func (v *externalValidation) record(check ConditionCheck) {
	key := check.Rule + "\x00" + check.Path + "\x00" + check.Module
	if v.seen[key] {
		return
	}
	if len(v.checks) >= v.max {
		v.dropped++
		return
	}
	v.seen[key] = true
	switch check.Mode {
	case "local":
		v.local++
	case "external":
		v.external++
	default:
		v.pending++
	}
	v.checks = append(v.checks, check)
}

// evaluate first preserves the existing dataset-decided grammar; external
// evidence only resolves a generated audited family, bound to exact row/path.
func (v *externalValidation) evaluate(text string, source iodmodules.RuleSource, path, module string, item, root *object.Object) (bool, bool, bool, bool) {
	check := ConditionCheck{Rule: source.Rule, Path: path, Module: module, Source: publicRuleSource(source), State: ConditionUnknown, Mode: "pending", Reason: "unsupported-context-family"}
	if check.Rule == "" {
		check.Rule = "unbound-condition"
		check.Reason = "missing-normative-source"
	}
	state, forbidden := datasetCondition(text, source, item, root)
	if state != ConditionUnknown {
		check.State = state
		check.Mode = "local"
		check.Reason = "dataset-condition"
		v.record(check)
		return state == ConditionTrue, true, forbidden, false
	}
	if priorityConditions[source.Rule] != nil {
		check.Reason = "indeterminate-dataset-condition"
		v.record(check)
		return false, false, false, false
	}
	family := ExternalFamily(source.ExternalFamily)
	if family == PerformedProcedureStepParticipation || family == ContributingSOPInstanceConsistency {
		check.Reason = "missing-external-context"
		if v.context != nil {
			facts := v.context.facts[factKey(family, source.Rule, path)]
			known := ConditionUnknown
			conflict := false
			for _, f := range facts {
				check.EvidenceSHA256 = append(check.EvidenceSHA256, f.EvidenceSHA256)
				if f.State != ConditionUnknown {
					if known != ConditionUnknown && known != f.State {
						conflict = true
					}
					known = f.State
				}
			}
			if conflict {
				check.Reason = "contradictory-external-context"
				v.record(check)
				return false, false, false, true
			}
			if known != ConditionUnknown {
				check.State = known
				check.Mode = "external"
				check.Reason = "caller-resolved-condition"
				v.record(check)
				return known == ConditionTrue, true, false, false
			}
		}
	}
	v.record(check)
	return false, false, false, false
}

// ExternalConditionCoverage separates external-capable rows from dataset-only
// coverage. Unsupported families remain in ConditionCoverage().Unevaluated.
func ExternalConditionCoverage() map[ExternalFamily]int {
	counts := map[ExternalFamily]int{}
	for _, table := range iodmodules.RuleTables() {
		rows, _ := iodmodules.Rows(table)
		for index := range rows {
			source, ok := iodmodules.RowSource(table, index)
			if ok && strings.TrimSpace(source.ExternalFamily) != "" {
				counts[ExternalFamily(source.ExternalFamily)]++
			}
		}
	}
	return counts
}
