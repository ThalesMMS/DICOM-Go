// Package iod checks a data set against its Information Object Definition:
// the Modules of the IOD (Mandatory, Conditional and User Option) and, in
// each, the Attribute Types 1, 1C, 2 and 2C at every Sequence Item depth.
//
// The rules are generated from the PS3.3 DocBook by cmd/iodmodulegen
// (internal/iodmodules, StandardVersion). Conditions of 1C and 2C
// Attributes and of C Modules are evaluated when their sentence has a
// formal reading ("Required if X (gggg,eeee) is present", "... has a Value
// of A or B", "... is not A", "... is greater than N", joined by "and" or
// "or"); the others are reported for manual review. A Conditional or User
// Option Module whose condition cannot be read is checked when one of its
// own Attributes is present, and its findings are warnings: a stray
// Attribute is weaker evidence than a Mandatory Module or a condition that
// holds. Options.CheckValues additionally checks structured enumerations,
// defined terms and proven contextual VM rules. Encoding and dictionary VR/VM
// remain the responsibility of package validation.
package iod

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/uid"
	"github.com/ThalesMMS/dicom-go/internal/iodmodules"
	"github.com/ThalesMMS/dicom-go/object"
)

// StandardVersion is the PS3.3 edition the rules come from.
const StandardVersion = iodmodules.StandardVersion

// ErrUnknownIOD reports a data set whose IOD cannot be identified.
var ErrUnknownIOD = errors.New("dicom/validation/iod: unknown IOD")

// Kind classifies a finding.
type Kind string

const (
	MissingType1 Kind = "missing-type-1"
	EmptyType1   Kind = "empty-type-1"
	MissingType2 Kind = "missing-type-2"
	// MissingType1C, EmptyType1C and MissingType2C are reported when the
	// condition was evaluated true.
	MissingType1C Kind = "missing-type-1c"
	EmptyType1C   Kind = "empty-type-1c"
	MissingType2C Kind = "missing-type-2c"
	// UnexpectedConditional is a 1C or 2C Attribute present although its
	// condition is false and PS3.3 says it shall not be present otherwise.
	UnexpectedConditional Kind = "unexpected-conditional"
	// ManualCondition is an absent 1C or 2C Attribute whose condition could
	// not be evaluated.
	ManualCondition            Kind = "manual-condition"
	InvalidEnumeratedValue     Kind = "invalid-enumerated-value"
	AdditionalDefinedTerm      Kind = "additional-defined-term"
	InvalidContextMultiplicity Kind = "invalid-context-multiplicity"
	ManualValueConstraint      Kind = "manual-value-constraint"
)

// Severity of a finding.
type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
	SeverityInfo    Severity = "info"
)

func (k Kind) severity() Severity {
	switch k {
	case UnexpectedConditional, AdditionalDefinedTerm:
		return SeverityWarning
	case ManualCondition, ManualValueConstraint:
		return SeverityInfo
	}
	return SeverityError
}

// Finding is one requirement an Attribute does not meet.
type Finding struct {
	Kind     Kind
	Severity Severity
	// Module is the Module (or Macro) the requirement comes from.
	Module string
	// Path locates the Attribute: "(0040,A730)[1]/(0040,A010)" for the
	// Concept Name Code Sequence of the first Content Sequence Item.
	Path string
	Tag  core.Tag
	Type string
	// Condition is the PS3.3 condition text of a 1C or 2C Attribute.
	Condition string
	// Rule and Source identify a value restriction; they never contain the
	// examined value. They are empty for legacy Type findings.
	Rule   string
	Source *ConstraintSource
	// Message explains a value finding using fixed, PHI-free text.
	Message string
}

func (f Finding) String() string {
	return fmt.Sprintf("%s %s: %s (Type %s, %s)", f.Severity, f.Kind, f.Path, f.Type, f.Module)
}

// ModuleCheck records whether and why a Module of the IOD was checked.
type ModuleCheck struct {
	Module  string
	Usage   string
	Checked bool
	Reason  string
}

// Report is the result of Validate.
type Report struct {
	IOD             string
	StandardVersion string
	Modules         []ModuleCheck
	Findings        []Finding
	// Truncated counts the findings dropped beyond Options.MaxFindings.
	Truncated int
}

// Errors returns the findings of error severity.
func (r Report) Errors() []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Severity == SeverityError {
			out = append(out, f)
		}
	}
	return out
}

// Options tunes Validate.
type Options struct {
	// IOD names the IOD as in PS3.3 without "IOD" (for example "CT
	// Image"); empty derives it from the SOP Class UID.
	IOD string
	// MaxFindings bounds the findings; 0 means 1000.
	MaxFindings int
	// CheckValues opts into scoped PS3.3 value restrictions. The zero value
	// retains the legacy Type/condition checks and findings.
	CheckValues bool
}

// Validate checks obj against its IOD.
func Validate(obj *object.Object, opts Options) (Report, error) {
	return validateWithExternal(obj, opts, nil)
}

func validateWithExternal(obj *object.Object, opts Options, external *externalValidation) (Report, error) {
	if obj == nil {
		return Report{}, fmt.Errorf("%w: data set is nil", ErrUnknownIOD)
	}
	name := opts.IOD
	if name == "" {
		sopClass, _ := obj.GetUID(core.NewTag(0x0008, 0x0016))
		var ok bool
		if name, ok = IODForSOPClass(sopClass); !ok {
			return Report{StandardVersion: StandardVersion}, fmt.Errorf("%w: SOP Class %q", ErrUnknownIOD, sopClass)
		}
	}
	modules, ok := iodmodules.Modules(name)
	if !ok {
		return Report{StandardVersion: StandardVersion}, fmt.Errorf("%w: %q", ErrUnknownIOD, name)
	}
	max := opts.MaxFindings
	if max <= 0 {
		max = 1000
	}
	v := &validator{root: obj, report: Report{IOD: name, StandardVersion: StandardVersion}, max: max, seen: map[string]bool{}, checkValues: opts.CheckValues, external: external}
	mandatory := map[core.Tag]bool{}
	for _, use := range modules {
		if use.Usage == "M" {
			topTags(tree(moduleTable(use.Module)), func(tag core.Tag) { mandatory[tag] = true })
		}
	}
	for useIndex, use := range modules {
		check := ModuleCheck{Module: use.Module, Usage: use.Usage}
		switch use.Usage {
		case "M":
			check.Checked, check.Reason = true, "mandatory"
		default:
			if use.Usage == "C" {
				source, _ := iodmodules.ModuleSource(name, useIndex)
				var state ConditionState
				if external != nil {
					required, known, _, _ := external.evaluate(use.Condition, source, "module/"+use.Module, use.Module, obj, obj)
					if known {
						state = conditionState(required)
					}
				} else {
					state, _ = datasetCondition(use.Condition, source, obj, obj)
				}
				if state == ConditionTrue {
					check.Checked, check.Reason = true, "condition true"
					break
				}
				switch state {
				case ConditionFalse:
					check.Reason = "condition false"
				case ConditionUnknown:
					if priorityConditions[source.Rule] != nil {
						check.Reason = "condition unknown"
					}
				}
				// A Frame of Reference may be present in SEG even
				// when Derivation Image makes the Module optional.
				if state == ConditionFalse && source.Rule != segFrameOfReferenceRule {
					break
				}
			}
			// Present when one of its own top-level Attributes is.
			topTags(tree(moduleTable(use.Module)), func(tag core.Tag) {
				if !check.Checked && !mandatory[tag] && obj.Has(tag) {
					check.Checked, check.Reason = true, fmt.Sprintf("present (%s)", tag)
				}
			})
			if !check.Checked && check.Reason == "" {
				check.Reason = "absent"
			}
		}
		v.report.Modules = append(v.report.Modules, check)
		if check.Checked {
			v.inferred = strings.HasPrefix(check.Reason, "present")
			v.check(tree(moduleTable(use.Module)), obj, "", use.Module)
			if opts.CheckValues && moduleTable(use.Module) == "Multi-frame Functional Groups Module" {
				v.checkFunctionalGroupValues(use.Module)
			}
		}
	}
	return v.report, nil
}

// moduleTable is the name of a Module's table in the rules.
func moduleTable(module string) string {
	if _, ok := iodmodules.Rows(module); ok {
		if _, exact := iodmodules.Rows(module + " Module"); exact {
			return module + " Module"
		}
		return module
	}
	return module + " Module"
}

// IODForSOPClass returns the IOD of a Storage SOP Class: its name in the
// UID registry without " Storage" and the "- For Presentation/Processing"
// suffix, matched against the PS3.3 IOD names regardless of case.
func IODForSOPClass(sopClassUID string) (string, bool) {
	// PS3.4 maps the Label Map Segmentation Storage SOP Class to A.51.
	if sopClassUID == "1.2.840.10008.5.1.4.1.1.66.7" {
		return "Segmentation", true
	}
	entry, ok := uid.Dictionary.ByUID(sopClassUID)
	if !ok {
		return "", false
	}
	name := entry.Name
	for _, suffix := range []string{" - For Presentation", " - For Processing"} {
		name = strings.TrimSuffix(name, suffix)
	}
	name = strings.TrimSuffix(name, " Storage")
	candidates := []string{name, strings.TrimSuffix(name, " Instance"), strings.Replace(name, "Secondary Capture", "SC", 1)}
	iodIndexOnce.Do(buildIODIndex)
	for _, candidate := range candidates {
		if iod, ok := iodIndex[strings.ToLower(candidate)]; ok {
			return iod, true
		}
	}
	// Several SOP Classes share one IOD named without their leading
	// qualifier ("Grayscale Planar MPR Volumetric Presentation State" and
	// "Compositing Planar MPR ..." use "Planar MPR ...").
	words := strings.Fields(name)
	for i := 1; i < len(words)-1; i++ {
		if iod, ok := iodIndex[strings.ToLower(strings.Join(words[i:], " "))]; ok {
			return iod, true
		}
	}
	return "", false
}

var (
	iodIndexOnce sync.Once
	iodIndex     map[string]string
)

func buildIODIndex() {
	iodIndex = map[string]string{}
	for _, name := range iodmodules.IODs() {
		iodIndex[strings.ToLower(name)] = name
	}
}

// node is an Attribute of a Module with the Attributes of its Sequence
// Items, or (group) a Macro included under a condition.
type node struct {
	tag         core.Tag
	opaque      bool
	group       bool
	typ         string
	condition   string
	children    []node
	constraints []iodmodules.ValueConstraint
	source      iodmodules.RuleSource
}

var trees sync.Map // table name -> []node

// tree resolves a table's rows to a tree; each Include becomes a group of
// the included table's nodes, with the Include's condition.
func tree(table string) []node {
	if cached, ok := trees.Load(table); ok {
		return cached.([]node)
	}
	nodes := buildTree(table, map[string]bool{})
	trees.Store(table, nodes)
	return nodes
}

func buildTree(table string, open map[string]bool) []node {
	rows, ok := iodmodules.Rows(table)
	if !ok || open[table] {
		return nil
	}
	open[table] = true
	defer delete(open, table)
	var build func(i, depth int) ([]node, int)
	build = func(i, depth int) ([]node, int) {
		var out []node
		for i < len(rows) && int(rows[i].Depth) >= depth {
			r := rows[i]
			rowIndex := i
			if int(r.Depth) > depth {
				// Rows deeper than the level above them (a malformed or
				// opaque table) are attached to the last node.
				var children []node
				children, i = build(i, int(r.Depth))
				if len(out) > 0 {
					out[len(out)-1].children = append(out[len(out)-1].children, children...)
				}
				continue
			}
			i++
			if r.Include != "" {
				source, _ := iodmodules.RowSource(table, rowIndex)
				out = append(out, node{group: true, condition: r.Condition, children: buildTree(r.Include, open), source: source})
				continue
			}
			n := node{
				tag: core.NewTag(uint16(r.Tag>>16), uint16(r.Tag)), opaque: r.Tag == 0,
				typ: r.Type, condition: r.Condition,
				constraints: iodmodules.Constraints(table, rowIndex),
			}
			n.source, _ = iodmodules.RowSource(table, rowIndex)
			if i < len(rows) && int(rows[i].Depth) > depth {
				n.children, i = build(i, depth+1)
			}
			out = append(out, n)
		}
		return out, i
	}
	nodes, _ := build(0, 0)
	return nodes
}

// topTags calls f with the top-level Attributes of nodes, looking into the
// unconditional groups.
func topTags(nodes []node, f func(core.Tag)) {
	for _, n := range nodes {
		switch {
		case n.group && n.condition == "":
			topTags(n.children, f)
		case !n.group && !n.opaque:
			f(n.tag)
		}
	}
}

type validator struct {
	root   *object.Object
	report Report
	max    int
	seen   map[string]bool
	// inferred marks a Module checked because one of its Attributes is
	// present; its errors are reported as warnings.
	inferred    bool
	checkValues bool
	external    *externalValidation
}

func (v *validator) add(kind Kind, module, path string, n node) {
	key := string(kind) + " " + path
	if v.seen[key] {
		return
	}
	v.seen[key] = true
	if len(v.report.Findings) >= v.max {
		v.report.Truncated++
		return
	}
	severity := kind.severity()
	if v.inferred && severity == SeverityError && kind != ContradictoryExternalContext {
		severity = SeverityWarning
	}
	finding := Finding{Kind: kind, Severity: severity, Module: module, Path: path, Tag: n.tag, Type: n.typ, Condition: n.condition}
	if v.external != nil {
		finding.Rule = n.source.Rule
		source := constraintSource(n.source.Source)
		finding.Source = &source
		if kind == ContradictoryExternalContext {
			finding.Message = "Caller evidence contradicts the condition resolution"
		}
	}
	v.report.Findings = append(v.report.Findings, finding)
}

// check applies the nodes' Types to a data set or Sequence Item.
func (v *validator) check(nodes []node, item *object.Object, prefix, module string) {
	for _, n := range nodes {
		if n.group {
			// A conditional Include applies when its condition is true; one
			// that cannot be read is not checked.
			if n.condition != "" {
				if v.external != nil {
					required, known, _, conflict := v.external.evaluate(n.condition, n.source, prefix, module, item, v.root)
					if conflict {
						v.add(ContradictoryExternalContext, module, prefix, n)
					}
					if !known || !required {
						continue
					}
				} else {
					c, ok := parseCondition(n.condition)
					if !ok || !c.eval(item, v.root) {
						continue
					}
				}
			}
			v.check(n.children, item, prefix, module)
			continue
		}
		if n.opaque {
			continue
		}
		path := prefix + n.tag.String()
		element, present := item.Get(n.tag)
		empty := present && emptyElement(item, element)
		switch n.typ {
		case "1":
			if !present {
				v.add(MissingType1, module, path, n)
			} else if empty {
				v.add(EmptyType1, module, path, n)
			}
		case "2":
			if !present {
				v.add(MissingType2, module, path, n)
			}
		case "1C", "2C":
			required, ok, forbidden, conflict := false, false, false, false
			if v.external != nil {
				required, ok, forbidden, conflict = v.external.evaluate(n.condition, n.source, path, module, item, v.root)
			} else {
				state, disallowed := datasetCondition(n.condition, n.source, item, v.root)
				required, ok, forbidden = state == ConditionTrue, state != ConditionUnknown, disallowed
			}
			if conflict {
				v.add(ContradictoryExternalContext, module, path, n)
			}
			switch {
			case !ok:
				if !present || v.external != nil || priorityConditions[n.source.Rule] != nil {
					v.add(ManualCondition, module, path, n)
				}
			case required:
				if !present {
					v.add(map[string]Kind{"1C": MissingType1C, "2C": MissingType2C}[n.typ], module, path, n)
				} else if empty && n.typ == "1C" {
					v.add(EmptyType1C, module, path, n)
				}
			case present && forbidden:
				v.add(UnexpectedConditional, module, path, n)
			}
		}
		if v.checkValues && present && !empty {
			v.checkValueConstraints(n, item, module, path)
		}
		if present && len(n.children) > 0 {
			items, _ := item.GetSequence(n.tag)
			for i, child := range items {
				v.check(n.children, child, fmt.Sprintf("%s[%d]/", path, i+1), module)
			}
		}
	}
}

// emptyElement reports an Attribute without a Value: zero length, or a
// Sequence without Items.
func emptyElement(obj *object.Object, element core.Element) bool {
	if element.VR() == core.VRSQ {
		items, _ := obj.GetSequence(element.Tag())
		return len(items) == 0
	}
	length := element.Length()
	return length == 0
}

// SortFindings orders findings by path, then kind.
func SortFindings(findings []Finding) {
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Path != findings[j].Path {
			return findings[i].Path < findings[j].Path
		}
		return findings[i].Kind < findings[j].Kind
	})
}
