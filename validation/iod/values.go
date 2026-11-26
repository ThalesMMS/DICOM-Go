package iod

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/iodmodules"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/validation"
)

// MaxConstraintValueBytes bounds each primitive examined by CheckValues. Larger
// or deferred values remain manual; encoding validation keeps its own policy.
const MaxConstraintValueBytes = 1 << 20

// ConstraintSource identifies the normative origin of a value finding. Row is
// 1-based in DocBook and Index is 0-based in the generated table before Includes,
// or -1 for a supplemental value-only source row absent from legacy Rows.
// Edition and SHA256 identify the exact source input, not the current website.
type ConstraintSource struct {
	Table   string   `json:"table"`
	TableID string   `json:"table_id"`
	Row     int      `json:"row"`
	Index   int      `json:"index"`
	Depth   uint8    `json:"depth"`
	Tag     core.Tag `json:"tag"`
	Section string   `json:"section,omitempty"`
	Edition string   `json:"edition"`
	SHA256  string   `json:"sha256"`
}

func constraintSource(s iodmodules.ConstraintSource) ConstraintSource {
	return ConstraintSource{Table: s.Table, TableID: s.TableID, Row: s.Row, Index: s.Index, Depth: s.Depth, Tag: core.NewTag(uint16(s.Tag>>16), uint16(s.Tag)), Section: s.Section, Edition: s.Edition, SHA256: s.SHA256}
}

func (v *validator) addValue(kind Kind, module, path string, n node, c iodmodules.ValueConstraint, reason string) {
	key := string(kind) + " " + path + " " + c.Rule
	if v.seen[key] {
		return
	}
	v.seen[key] = true
	if len(v.report.Findings) >= v.max {
		v.report.Truncated++
		return
	}
	severity := kind.severity()
	if v.inferred && severity == SeverityError {
		severity = SeverityWarning
	}
	source := constraintSource(c.Source)
	v.report.Findings = append(v.report.Findings, Finding{Kind: kind, Severity: severity, Module: module, Path: path, Tag: n.tag, Type: n.typ, Condition: n.condition, Rule: c.Rule, Source: &source, Message: reason})
}

func (v *validator) checkValueConstraints(n node, item *object.Object, module, path string) {
	if len(n.constraints) == 0 {
		return
	}
	values, vr, ok := constraintValues(item, n.tag)
	if ok {
		onlyEmpty := true
		for _, value := range values {
			if strings.TrimSpace(value) != "" {
				onlyEmpty = false
				break
			}
		}
		// An entirely empty Attribute is governed by Type, including Type 2.
		// An empty component of a populated tuple must still meet its own set.
		if onlyEmpty {
			return
		}
	}
	for _, c := range n.constraints {
		if c.Kind == "manual" {
			v.addValue(ManualValueConstraint, module, path, n, c, c.Reason)
			continue
		}
		// Type conditions govern presence. An unqualified literal list still
		// constrains a present value; only its own compiled title predicate
		// qualifies membership. Include/module selection was handled upstream.
		active, known := predicateValue(c.Predicate, item, v.root)
		if !known {
			v.addValue(ManualValueConstraint, module, path, n, c, "constraint predicate could not be evaluated")
			continue
		}
		if !active {
			continue
		}
		if !ok {
			v.addValue(ManualValueConstraint, module, path, n, c, "value was not decoded for bounded constraint validation; encoding and dictionary VM remain separate")
			continue
		}
		if c.Kind == "count" {
			tag := core.NewTag(uint16(c.CountTag>>16), uint16(c.CountTag))
			sequence, exists := v.root.GetSequence(tag)
			if !exists || len(sequence) == 0 {
				v.addValue(ManualValueConstraint, module, path, n, c, "context sequence is absent, empty or not decoded")
				continue
			}
			if len(values) != len(sequence) {
				v.addValue(InvalidContextMultiplicity, module, path, n, c, "value count does not match the root dimension sequence item count")
			}
			continue
		}
		wanted := make(map[string]bool, len(c.Values))
		valid := true
		for _, term := range c.Values {
			normalized, accepted := canonicalValue(vr, term)
			if !accepted {
				valid = false
				break
			}
			wanted[normalized] = true
		}
		if !valid {
			v.addValue(ManualValueConstraint, module, path, n, c, "literal normalization for this VR is not formalized")
			continue
		}
		components := values
		if c.Position > 0 {
			if c.Position > len(values) {
				continue
			}
			components = values[c.Position-1 : c.Position]
		}
		invalid := false
		for _, value := range components {
			if strings.TrimSpace(value) == "" {
				invalid = true
				continue
			}
			normalized, accepted := canonicalValue(vr, value)
			if !accepted {
				valid = false
				break
			}
			if !wanted[normalized] {
				invalid = true
			}
		}
		if !valid {
			v.addValue(ManualValueConstraint, module, path, n, c, "value normalization requires encoding validation")
			continue
		}
		if invalid {
			kind := InvalidEnumeratedValue
			message := "component is outside the scoped enumerated values"
			if c.Kind == "defined" {
				kind = AdditionalDefinedTerm
				message = "component is an additional defined term; the source list is extensible"
			}
			v.addValue(kind, module, path, n, c, message)
		}
	}
}

// Frame Content and Frame Usefulness are Functional Group Macros, not Include
// rows of the generic Multi-frame Functional Groups table. Examine only these
// present macros' value rules in Shared/Per-Frame Items; no new Type requirements
// are inferred. This path is entered only for a checked Multi-frame Functional
// Groups module.
func (v *validator) checkFunctionalGroupValues(module string) {
	var check func([]node, *object.Object, string)
	check = func(nodes []node, item *object.Object, prefix string) {
		for _, n := range nodes {
			if n.group {
				if n.condition != "" {
					condition, ok := parseCondition(n.condition)
					if !ok || !condition.eval(item, v.root) {
						continue
					}
				}
				check(n.children, item, prefix)
				continue
			}
			if n.opaque {
				continue
			}
			element, present := item.Get(n.tag)
			if !present {
				continue
			}
			path := prefix + n.tag.String()
			if !emptyElement(item, element) {
				v.checkValueConstraints(n, item, module, path)
			}
			if len(n.children) > 0 {
				items, _ := item.GetSequence(n.tag)
				for index, child := range items {
					check(n.children, child, fmt.Sprintf("%s[%d]/", path, index+1))
				}
			}
		}
	}
	macros := [][]node{valueOnlyTree("Frame Content Macro"), valueOnlyTree("Frame Usefulness Functional Group Macro")}
	for _, tag := range []core.Tag{core.NewTag(0x5200, 0x9229), core.NewTag(0x5200, 0x9230)} {
		items, _ := v.root.GetSequence(tag)
		for index, item := range items {
			for _, macro := range macros {
				check(macro, item, fmt.Sprintf("%s[%d]/", tag, index+1))
			}
		}
	}
}

// valueOnlyTree adds source-proven supplemental bindings to a detached tree.
// The legacy projection and its cached Type tree stay immutable, so CheckValues
// cannot introduce requirements into later default validations.
func valueOnlyTree(table string) []node {
	var clone func([]node) []node
	clone = func(nodes []node) []node {
		out := append([]node(nil), nodes...)
		for i := range out {
			out[i].children = clone(out[i].children)
			out[i].constraints = append([]iodmodules.ValueConstraint(nil), out[i].constraints...)
		}
		return out
	}
	nodes := clone(tree(table))
	for _, c := range iodmodules.Constraints(table, -1) {
		// Unproved paths remain coverage records, never executable bindings.
		if len(c.ParentPath) != int(c.Source.Depth) {
			continue
		}
		level := &nodes
		found := true
		for _, parent := range c.ParentPath {
			tag := core.NewTag(uint16(parent>>16), uint16(parent))
			matched := false
			for i := range *level {
				if !(*level)[i].group && !(*level)[i].opaque && (*level)[i].tag == tag {
					level = &(*level)[i].children
					matched = true
					break
				}
			}
			if !matched {
				found = false
				break
			}
		}
		if !found {
			continue
		}
		tag := core.NewTag(uint16(c.Source.Tag>>16), uint16(c.Source.Tag))
		matched := false
		for i := range *level {
			if !(*level)[i].group && (*level)[i].tag == tag {
				(*level)[i].constraints = append((*level)[i].constraints, c)
				matched = true
				break
			}
		}
		if !matched {
			*level = append(*level, node{tag: tag, typ: c.AttributeType, constraints: []iodmodules.ValueConstraint{c}})
		}
	}
	return nodes
}

// constraintValues reuses encoding validation as a gate, avoiding a second
// error for malformed representations or global VM. Only primitive components
// are counted: SQ Items never become VM. Raw values use the facade's byte order
// and inherited character set; typed values keep their declared VR.
func constraintValues(obj *object.Object, tag core.Tag) ([]string, core.VR, bool) {
	element, present := obj.Get(tag)
	if !present {
		return nil, "", false
	}
	if element.VR() == core.VRSQ || element.VR() == core.VRUN {
		return nil, element.VR(), false
	}
	length, known := element.CalculatedLength()
	if !known || !length.IsDefined() || uint64(length) > MaxConstraintValueBytes {
		return nil, element.VR(), false
	}
	ds := core.DataSet{Elements: []core.Element{element}}
	if element.VR().UsesSpecificCharacterSet() {
		charset, err := obj.CharacterSet()
		if err != nil {
			return nil, element.VR(), false
		}
		ds.Elements = append([]core.Element{{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0005), VR: core.VRCS}, Value: core.StringValue(charset.Names())}}, ds.Elements...)
	}
	report, err := validation.ValidateDataSet(context.Background(), ds, validation.Options{Dictionary: std.Dictionary, ByteOrder: obj.ValueByteOrder(), MaxFindings: 32})
	if err != nil || report.Report.HasErrors() {
		return nil, element.VR(), false
	}
	vr := element.VR()
	if vr.IsStringLike() {
		values, ok := obj.GetStrings(tag)
		return values, vr, ok
	}
	var values []string
	switch vr {
	case core.VRUS, core.VRSS, core.VRUL, core.VRSL, core.VRUV, core.VRSV:
		numbers, err := obj.GetInts(tag)
		if err != nil {
			return nil, vr, false
		}
		for _, n := range numbers {
			values = append(values, strconv.FormatInt(n, 10))
		}
	case core.VRFL, core.VRFD:
		numbers, err := obj.GetFloats(tag)
		if err != nil {
			return nil, vr, false
		}
		for _, n := range numbers {
			values = append(values, strconv.FormatFloat(n, 'g', -1, 64))
		}
	case core.VRAT:
		tags, ok := obj.GetTags(tag)
		if !ok {
			return nil, vr, false
		}
		for _, value := range tags {
			values = append(values, fmt.Sprintf("%04X%04XH", value.Group, value.Element))
		}
	default:
		return nil, vr, false
	}
	return values, vr, len(values) > 0
}

func canonicalValue(vr core.VR, value string) (string, bool) {
	value = strings.TrimSpace(value)
	switch vr {
	case core.VRDS, core.VRFL, core.VRFD:
		n, err := strconv.ParseFloat(value, 64)
		return strconv.FormatFloat(n, 'g', -1, 64), err == nil
	case core.VRIS, core.VRUS, core.VRSS, core.VRUL, core.VRSL, core.VRUV, core.VRSV:
		if (vr == core.VRUS || vr == core.VRUL) && strings.HasSuffix(value, "H") {
			n, err := strconv.ParseUint(strings.TrimSuffix(value, "H"), 16, 32)
			return strconv.FormatUint(n, 10), err == nil
		}
		n, err := strconv.ParseInt(value, 10, 64)
		return strconv.FormatInt(n, 10), err == nil
	case core.VRAT:
		hex := strings.TrimSuffix(value, "H")
		if len(hex) != 8 {
			return "", false
		}
		n, err := strconv.ParseUint(hex, 16, 32)
		return fmt.Sprintf("%08XH", n), err == nil
	default:
		return value, vr.IsStringLike()
	}
}

// predicateValue returns (active, known). Missing equality inputs, wrong VR or
// failed decoding remain manual; absence is meaningful only for presence tests.
func predicateValue(p iodmodules.ValuePredicate, item, root *object.Object) (bool, bool) {
	switch p.Operator {
	case "":
		return true, true
	case "and", "or":
		unknown := false
		for _, child := range p.Children {
			value, known := predicateValue(child, item, root)
			if !known {
				unknown = true
				continue
			}
			if p.Operator == "and" && !value {
				return false, true
			}
			if p.Operator == "or" && value {
				return true, true
			}
		}
		if unknown {
			return false, false
		}
		return p.Operator == "and", true
	}
	tag := core.NewTag(uint16(p.Tag>>16), uint16(p.Tag))
	holder := item
	if !item.Has(tag) && root != item && root.Has(tag) {
		holder = root
	}
	if p.Operator == "present" {
		return holder.Has(tag), true
	}
	if p.Operator != "one-of" || !holder.Has(tag) {
		return false, false
	}
	values, vr, ok := constraintValues(holder, tag)
	if !ok {
		return false, false
	}
	if p.Position > 0 {
		if p.Position > len(values) {
			return false, false
		}
		values = values[p.Position-1 : p.Position]
	}
	for _, want := range p.Values {
		normalized, known := canonicalValue(vr, want)
		if !known {
			return false, false
		}
		for _, value := range values {
			got, known := canonicalValue(vr, value)
			if !known {
				return false, false
			}
			if got == normalized {
				return true, true
			}
		}
	}
	return false, true
}

// ValueCoverage distinguishes compiled source rules from manual qualifications.
// It counts source restrictions, not clinical interoperability or IOD completeness.
type ValueCoverage struct {
	Compiled int
	Manual   []ValueRule
}
type ValueRule struct {
	Rule, Reason string
	Source       ConstraintSource
}

func ValueConstraintCoverage() ValueCoverage {
	var result ValueCoverage
	tables := iodmodules.ConstraintTables()
	names := make([]string, 0, len(tables))
	for name := range tables {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, table := range names {
		indexes := make([]int, 0, len(tables[table]))
		for index := range tables[table] {
			indexes = append(indexes, index)
		}
		sort.Ints(indexes)
		for _, index := range indexes {
			for _, c := range tables[table][index] {
				if c.Kind != "manual" {
					result.Compiled++
				} else {
					result.Manual = append(result.Manual, ValueRule{Rule: c.Rule, Reason: c.Reason, Source: constraintSource(c.Source)})
				}
			}
		}
	}
	return result
}
