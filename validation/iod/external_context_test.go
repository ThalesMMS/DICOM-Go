package iod

import (
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/iodmodules"
	"github.com/ThalesMMS/dicom-go/object"
	"reflect"
	"strings"
	"testing"
)

func contextNode(t *testing.T, family ExternalFamily, tag uint32) node {
	t.Helper()
	for _, table := range iodmodules.RuleTables() {
		rows, _ := iodmodules.Rows(table)
		for index, row := range rows {
			source, ok := iodmodules.RowSource(table, index)
			if ok && source.ExternalFamily == string(family) && row.Tag == tag {
				return node{tag: core.NewTag(uint16(tag>>16), uint16(tag)), typ: row.Type, condition: row.Condition, source: source}
			}
		}
	}
	t.Fatal("expected generated external family source is absent")
	return node{}
}

func contextCheck(n node, obj *object.Object, prefix string, context *ExternalContext, inferred bool) (*validator, *externalValidation) {
	x := &externalValidation{context: context, seen: map[string]bool{}, max: 100}
	v := &validator{root: obj, max: 100, seen: map[string]bool{}, external: x, inferred: inferred}
	v.check([]node{n}, obj, prefix, "synthetic scoped caller")
	return v, x
}

func TestExternalContextFamilies(t *testing.T) {
	for _, family := range []struct {
		family ExternalFamily
		tag    uint32
	}{{PerformedProcedureStepParticipation, 0x00081111}, {ContributingSOPInstanceConsistency, 0x00081090}} {
		t.Run(string(family.family), func(t *testing.T) {
			n := contextNode(t, family.family, family.tag)
			for _, present := range []bool{false, true} {
				for _, state := range []string{"true", "false", "missing", "unknown", "contradictory"} {
					t.Run(state+map[bool]string{false: "/absent", true: "/present"}[present], func(t *testing.T) {
						obj := object.New(nil)
						if present {
							if family.family == PerformedProcedureStepParticipation {
								obj.Put(core.Element{Header: core.ElementHeader{Tag: n.tag, VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{
									{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x1150), VR: core.VRUI}, Value: core.StringValue{"1.2.840.10008.3.1.2.3.3"}},
									{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x1155), VR: core.VRUI}, Value: core.StringValue{"1.2.3.4.1478"}},
								}}}}})
							} else {
								obj.Put(core.Element{Header: core.ElementHeader{Tag: n.tag, VR: core.VRLO}, Value: core.StringValue{"SYNTHETIC"}})
							}
						}
						path := "(0018,9506)[1]/" + n.tag.String()
						var facts []ExternalFact
						f := ExternalFact{Family: family.family, Rule: n.source.Rule, Path: path, State: ConditionUnknown, EvidenceSHA256: strings.Repeat("a", 64)}
						switch state {
						case "true":
							f.State = ConditionTrue
							facts = []ExternalFact{f}
						case "false":
							f.State = ConditionFalse
							facts = []ExternalFact{f}
						case "unknown":
							facts = []ExternalFact{f}
						case "contradictory":
							f.State = ConditionTrue
							facts = []ExternalFact{f}
							f.State = ConditionFalse
							facts = append(facts, f)
						}
						context, err := NewExternalContext(facts)
						if err != nil {
							t.Fatal(err)
						}
						v, x := contextCheck(n, obj, "(0018,9506)[1]/", context, true)
						missing, manual, contradictory := 0, 0, 0
						for _, finding := range v.report.Findings {
							if finding.Path != path || finding.Rule != n.source.Rule || finding.Source == nil || finding.Source.SHA256 != iodmodules.SourceSHA256 {
								t.Fatal("unbound or wrong scoped finding")
							}
							switch finding.Kind {
							case MissingType1C:
								missing++
							case ManualCondition:
								manual++
							case ContradictoryExternalContext:
								contradictory++
								if finding.Severity != SeverityError {
									t.Fatal("contradiction downgraded by module inference")
								}
							}
						}
						wantMissing := 0
						if state == "true" && !present {
							wantMissing = 1
						}
						wantManual := 0
						if state == "missing" || state == "unknown" || state == "contradictory" {
							wantManual = 1
						}
						wantConflict := 0
						if state == "contradictory" {
							wantConflict = 1
						}
						if missing != wantMissing || manual != wantManual || contradictory != wantConflict || len(x.checks) != 1 {
							t.Fatal("wrong tri-state/contradiction outcome")
						}
						if x.checks[0].Path != path {
							t.Fatal("wrong condition path")
						}
						if wantManual > 0 && (x.pending != 1 || x.external != 0) {
							t.Fatal("unknown context was counted as verified")
						}
					})
				}
			}
		})
	}
}

func TestExternalContextScopeAndImmutableSnapshot(t *testing.T) {
	n := contextNode(t, ContributingSOPInstanceConsistency, 0x00181020)
	obj := object.New(nil)
	facts := []ExternalFact{{Family: ContributingSOPInstanceConsistency, Rule: n.source.Rule, Path: "(0018,9506)[1]/" + n.tag.String(), State: ConditionTrue, EvidenceSHA256: strings.Repeat("b", 64)}}
	context, err := NewExternalContext(facts)
	if err != nil {
		t.Fatal(err)
	}
	facts[0].State = ConditionFalse
	facts[0].Path = "unrelated"
	first, x := contextCheck(n, obj, "(0018,9506)[1]/", context, false)
	second, y := contextCheck(n, obj, "(0018,9506)[2]/", context, false)
	if len(first.report.Findings) != 1 || first.report.Findings[0].Kind != MissingType1C || x.external != 1 {
		t.Fatal("caller mutation altered snapshot")
	}
	if len(second.report.Findings) != 1 || second.report.Findings[0].Kind != ManualCondition || y.pending != 1 {
		t.Fatal("context leaked between sequence items")
	}
	if len(x.checks[0].Source.CrossReferences) == 0 {
		t.Fatal("non-vacuous normative xref control absent")
	}
	if len(x.checks[0].Source.CrossReferences) > 0 {
		before := n.source.CrossReferences[0]
		x.checks[0].Source.CrossReferences[0] = "changed"
		if n.source.CrossReferences[0] != before {
			t.Fatal("report aliases normative references")
		}
	}
}

func TestExternalContextAdmissionAndBounds(t *testing.T) {
	f := ExternalFact{Family: PerformedProcedureStepParticipation, Rule: "table/row-1/index-0", Path: "(0008,1111)", State: ConditionTrue, EvidenceSHA256: strings.Repeat("a", 64)}
	for _, mutate := range []func(*ExternalFact){func(f *ExternalFact) { f.Family = "arbitrary" }, func(f *ExternalFact) { f.State = 9 }, func(f *ExternalFact) { f.Rule = "" }, func(f *ExternalFact) { f.Path = "bad\x00path" }, func(f *ExternalFact) { f.EvidenceSHA256 = "not a digest" }, func(f *ExternalFact) { f.Path = strings.Repeat("x", 1025) }} {
		bad := f
		mutate(&bad)
		if _, err := NewExternalContext([]ExternalFact{bad}); err != ErrExternalContext {
			t.Fatal("invalid context accepted")
		}
	}
	if _, err := NewExternalContext(make([]ExternalFact, 257)); err != ErrExternalContext {
		t.Fatal("fact count cap absent")
	}
	big := f
	big.Path = strings.Repeat("x", 1024)
	big.Rule = strings.Repeat("r", 512)
	facts := make([]ExternalFact, 50)
	for i := range facts {
		facts[i] = big
	}
	if _, err := NewExternalContext(facts); err != ErrExternalContext {
		t.Fatal("text cap absent")
	}
	if _, err := ValidateWithExternalContext(object.New(nil), Options{IOD: "CT Image"}, nil, 65537); err != ErrExternalContext {
		t.Fatal("check cap absent")
	}
	x := &externalValidation{seen: map[string]bool{}, max: 1}
	a := ConditionCheck{Rule: "one", Mode: "pending"}
	x.record(a)
	x.record(a)
	if len(x.checks) != 1 || len(x.seen) != 1 || x.dropped != 0 || x.pending != 1 {
		t.Fatal("retained duplicate consumes diagnostics budget")
	}
	x.record(ConditionCheck{Rule: "two", Mode: "external"})
	x.record(a)
	if len(x.checks) != 1 || len(x.seen) != 1 || x.dropped != 1 || x.pending != 1 || x.external != 0 || x.local != 0 || x.checks[0].Rule != a.Rule || x.checks[0].Mode != a.Mode {
		t.Fatal("A/A/B/A must retain one check, truncate only B, and keep counters bounded")
	}
}

func TestExternalContextPublicCompatibilityAndCoverage(t *testing.T) {
	if (ContextReport{}).Complete() {
		t.Fatal("unvalidated zero report silently passes")
	}
	invalid, invalidErr := ValidateWithExternalContext(nil, Options{}, nil, 0)
	if invalidErr == nil || invalid.Complete() {
		t.Fatal("validation error reported complete")
	}
	obj := object.New(nil)
	opts := Options{IOD: "Enhanced CT Image"}
	before, err := Validate(obj, opts)
	if err != nil {
		t.Fatal(err)
	}
	contextual, err := ValidateWithExternalContext(obj, opts, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if contextual.Pending == 0 || contextual.Complete() {
		t.Fatal("missing context silently passes")
	}
	after, err := Validate(obj, opts)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("legacy validation changed")
	}
	for _, f := range after.Findings {
		if f.Rule != "" || f.Source != nil {
			t.Fatal("legacy diagnostics changed")
		}
	}
	coverage := ExternalConditionCoverage()
	if coverage[PerformedProcedureStepParticipation] != 27 || coverage[ContributingSOPInstanceConsistency] != 10 {
		t.Fatal("external inventory bindings missing or broadened")
	}
	t.Logf("external-only capability: PPS rows=%d contributing-consistency rows=%d; dataset-only coverage remains separate", coverage[PerformedProcedureStepParticipation], coverage[ContributingSOPInstanceConsistency])
}
