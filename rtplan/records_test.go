package rtplan

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/validation"
	"github.com/ThalesMMS/dicom-go/validation/iod"
)

// All data are synthetic and are authored from the PS3.3 module rows. The
// fixture builders deliberately do not call the record reader or writer.
func rsEl(t uint32, vr core.VR, s string) core.Element { return derivedio.Str(ft(t), vr, s) }
func rsDS(es ...core.Element) core.DataSet {
	o := object.New(std.Dictionary)
	for _, e := range es {
		o.Put(e)
	}
	return derivedio.DataSet(o.Elements()...)
}
func rsSeq(t uint32, ds ...core.DataSet) core.Element { return derivedio.Seq(ft(t), ds...) }
func rsIS(t uint32, n int) core.Element {
	if e, ok := std.Dictionary.ByTag(ft(t)); ok {
		switch e.VR {
		case core.VRUS:
			return core.Element{Header: core.ElementHeader{Tag: ft(t), VR: e.VR}, Value: core.Uint16Value{uint16(n)}}
		case core.VRSS:
			return core.Element{Header: core.ElementHeader{Tag: ft(t), VR: e.VR}, Value: core.Int16Value{int16(n)}}
		}
	}
	return derivedio.IS(ft(t), n)
}
func rsFloat(t uint32, v ...float64) core.Element { return derivedio.DS(ft(t), v...) }
func rsFL(t uint32, v ...float32) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: ft(t), VR: core.VRFL}, Value: core.Float32Value(v)}
}
func rsRSafe(start, end string) []core.Element {
	return []core.Element{rsEl(0x30080162, core.VRDA, "20261001"), rsEl(0x30080164, core.VRTM, start), rsEl(0x30080166, core.VRDA, "20261001"), rsEl(0x30080168, core.VRTM, end)}
}
func rsRSession() []core.Element {
	return []core.Element{rsIS(0x30080022, 3), rsEl(0x300a00ce, core.VRCS, "CONTINUATION"), rsEl(0x3008002a, core.VRCS, "OPERATOR"), rsEl(0x3008002c, core.VRCS, "NOT_VERIFIED")}
}
func rsRCP(ion bool, index int, clock string, specified, delivered float64, first bool) core.DataSet {
	es := []core.Element{rsIS(0x300c00f0, index), rsEl(0x30080024, core.VRDA, "20261001"), rsEl(0x30080025, core.VRTM, clock), rsFloat(0x30080042, specified), rsFloat(0x30080044, delivered)}
	if !ion {
		es = append(es, rsEl(0x300a0115, core.VRDS, ""), rsEl(0x30080048, core.VRDS, ""))
	} else {
		es = append(es, rsFL(0x30080045, 5), rsFL(0x30080046, 4))
	}
	if first {
		es = append(es, rsFloat(0x300a0114, 6), rsFloat(0x300a011e, 15), rsEl(0x300a011f, core.VRCS, "NONE"), rsFloat(0x300a0120, 0), rsEl(0x300a0121, core.VRCS, "NONE"), rsFloat(0x300a0122, 0), rsEl(0x300a0123, core.VRCS, "NONE"), rsEl(0x300a0128, core.VRDS, ""), rsEl(0x300a0129, core.VRDS, ""), rsEl(0x300a012a, core.VRDS, ""), rsFloat(0x300a0125, 0), rsEl(0x300a0126, core.VRCS, "NONE"), rsSeq(0x300a011a, rsDS(rsEl(0x300a00b8, core.VRCS, "X"), rsFloat(0x300a011c, -20, 20))))
		if ion {
			es = append(es, rsFL(0x300a0140, 0), rsEl(0x300a0142, core.VRCS, "NONE"), rsFL(0x300a0144, 0), rsEl(0x300a0146, core.VRCS, "NONE"), rsFL(0x300a014a, 0), rsEl(0x300a014c, core.VRCS, "NONE"), rsFL(0x300a030d, 100), rsSeq(0x300a0360, rsDS(rsIS(0x300c0100, 7), rsEl(0x300a0362, core.VRLO, "IN"))))
		} else {
			es = append(es, rsEl(0x300a0015, core.VRCS, "MV"))
		}
	}
	if ion {
		// Table eccentric fields belong to the conventional template, not
		// the standard Ion patient-support macro used by this fixture.
		retained := es[:0]
		for _, e := range es {
			if e.Tag() != ft(0x300a0125) && e.Tag() != ft(0x300a0126) {
				retained = append(retained, e)
			}
		}
		es = retained
		es = append(es, rsEl(0x300a0390, core.VRSH, "SYNTH_TUNE"), rsIS(0x300a0392, 2), rsFL(0x300a0394, -1, 0, 1, 0), rsFL(0x30080047, 1.5, 2.5), rsIS(0x300a039a, 1))
	}
	if !first && ion {
		es = append(es, rsFL(0x30080047, 0, 0))
	}
	return rsDS(es...)
}
func rsRBrachyCP(index int, clock string, pos float64) core.DataSet {
	return rsDS(rsIS(0x300c00f0, index), rsEl(0x30080024, core.VRDA, "20261001"), rsEl(0x30080025, core.VRTM, clock), rsFloat(0x300a02d2, pos))
}
func rsRecordFixture(t testing.TB, sop string, pdr bool) *object.File {
	t.Helper()
	es := []core.Element{derivedio.UI(tagSOPClassUID, sop), derivedio.UI(tagSOPInstanceUID, "2.25.1329004101"), derivedio.UI(tagStudyInstanceUID, "2.25.1329004102"), derivedio.UI(tagSeriesInstanceUID, "2.25.1329004103"), rsIS(0x00200013, 1), rsEl(0x00080005, core.VRCS, "ISO_IR 192"), rsEl(0x00100010, core.VRPN, "SYNTH_RECORD^Å"), rsEl(0x00100020, core.VRLO, "SYNTH_RECORD"), rsEl(0x00100030, core.VRDA, ""), rsEl(0x00100040, core.VRCS, ""), rsEl(0x00080020, core.VRDA, "20261001"), rsEl(0x00080030, core.VRTM, "120000"), rsEl(0x00080090, core.VRPN, ""), rsEl(0x00200010, core.VRSH, "SYNTH_STUDY"), rsEl(0x00080050, core.VRSH, ""), rsIS(0x00200011, 1), rsEl(0x00080060, core.VRCS, "RTRECORD"), rsEl(0x00080070, core.VRLO, "SYNTH_EQUIPMENT"), rsEl(0x00081070, core.VRPN, ""), rsEl(0x30080250, core.VRDA, "20261001"), rsEl(0x30080251, core.VRTM, "120000"), rsEl(0x300a0709, core.VRCS, "DEVICE"), rsEl(0x00080201, core.VRSH, "-0300"), rsSeq(0x300c0002, rsDS(derivedio.UI(tagRefSOPClassUID, RTPlanStorage), derivedio.UI(tagRefSOPInstanceUID, "2.25.1329004110")))}
	if sop != RTTreatmentSummaryRecordStorage {
		es = append(es, rsSeq(0x300a0206, rsDS(rsEl(0x300a00b2, core.VRSH, "SYNTH_UNIT"), rsEl(0x00080070, core.VRLO, "SYNTH_EQUIPMENT"), rsEl(0x00080080, core.VRLO, ""), rsEl(0x00081090, core.VRLO, ""), rsEl(0x00181000, core.VRLO, ""))), rsIS(0x300a0078, 10), rsIS(0x300c0022, 1), rsSeq(0x30080010, rsDS(rsIS(0x30080064, 8), rsEl(0x30040002, core.VRCS, "GY"), rsFloat(0x30080016, 0.5), rsEl(0x30080014, core.VRCS, "ION_CHAMBER"))), rsSeq(0x30080070, rsDS(rsIS(0x30080072, 9), rsFloat(0x30080076, 0.6))))
	}
	switch sop {
	case RTBeamsTreatmentRecordStorage, RTIonBeamsTreatmentRecordStorage:
		ion := sop == RTIonBeamsTreatmentRecordStorage
		b := []core.Element{rsIS(0x300c0006, 1), rsEl(0x300a00c2, core.VRLO, "SYNTH_BEAM_Å"), rsEl(0x300a00c4, core.VRCS, "STATIC"), rsEl(0x300a00c6, core.VRCS, "PHOTON"), rsIS(0x300a00d0, 0), rsIS(0x300a00e0, 0), rsIS(0x300a00ed, 0), rsIS(0x300a00f0, 0), rsIS(0x300a0110, 2), rsSeq(0x300800a0, rsDS(rsEl(0x300a00b8, core.VRCS, "X"), rsIS(0x300a00bc, 1))), rsFloat(0x30080032, 10), rsFloat(0x30080036, 4), rsFloat(0x3008003a, 2), rsFloat(0x3008003b, 1), rsSeq(0x30080080, rsDS(rsIS(0x30080082, 8), rsFloat(0x30080016, 0.5))), rsSeq(0x30080090, rsDS(rsIS(0x30080092, 9), rsFloat(0x30080076, 0.6)))}
		b = append(b, rsRSession()...)
		seq, cpseq := uint32(0x30080020), uint32(0x30080040)
		if ion {
			seq, cpseq = 0x30080021, 0x30080041
			b = append(b, rsEl(0x300a0350, core.VRCS, "TABLE"), rsEl(0x300a00c6, core.VRCS, "PROTON"), rsEl(0x300a0308, core.VRCS, "MODULATED"), rsIS(0x300a0312, 1), rsIS(0x300a0330, 0), rsIS(0x300a0340, 0), rsSeq(0x300800f2, rsDS(rsIS(0x300c0100, 7), rsEl(0x300a0318, core.VRSH, "SYNTH_RS"))), rsSeq(0x300800f0, rsDS(rsEl(0x300a030f, core.VRSH, "SYNTH_SNOUT"))))
			es = append(es, rsSeq(0x300c0002, rsDS(derivedio.UI(tagRefSOPClassUID, RTIonPlanStorage), derivedio.UI(tagRefSOPInstanceUID, "2.25.1329004110"))))
		}
		// Delivery starts at meterset 3, so this is a partial/resumed record. The
		// referenced indices 5/6 need not begin at zero or span the whole plan.
		b = append(b, derivedio.Seq(ft(cpseq), rsRCP(ion, 5, "120000", 0, 3, true), rsRCP(ion, 6, "120001", 10, 7, false)))
		es = append(es, rsEl(0x300a00b3, core.VRCS, "MU"), derivedio.Seq(ft(seq), rsDS(b...)))
	case RTBrachyTreatmentRecordStorage:
		typ := "HDR"
		if pdr {
			typ = "PDR"
		}
		es = append(es, rsEl(0x300a0200, core.VRCS, "INTRACAVITARY"), rsEl(0x300a0202, core.VRCS, typ))
		sources, setups := []core.DataSet{}, []core.DataSet{}
		for n := 1; n <= 2; n++ {
			sources = append(sources, rsDS(rsIS(0x300a0212, n), rsEl(0x300a0214, core.VRCS, "POINT"), rsEl(0x300a0216, core.VRLO, "SYNTH_SOURCE"), rsEl(0x30080105, core.VRLO, ""), rsEl(0x300a0226, core.VRLO, "Ir-192"), rsFloat(0x300a0228, 74), rsEl(0x300a0229, core.VRCS, "AIR_KERMA_RATE"), rsFloat(0x300a022a, 0.125), rsEl(0x300a022c, core.VRDA, "20261001"), rsEl(0x300a022e, core.VRTM, "120000")))
			channels := []core.DataSet{}
			for j := 1; j <= 2; j++ {
				c := []core.Element{rsIS(0x300a0282, j), rsIS(0x00741406, j), rsIS(0x300c000e, n), rsEl(0x300a0284, core.VRDS, ""), rsEl(0x300a0288, core.VRCS, "STEPWISE"), rsFloat(0x30080132, 20), rsFloat(0x30080134, 10), rsEl(0x300a02a2, core.VRIS, ""), rsSeq(0x30080140, rsDS(rsIS(0x30080142, 1), rsEl(0x300a0291, core.VRSH, "SYNTH_APP"), rsEl(0x300a0292, core.VRCS, "RIGID"), rsFloat(0x300a0296, 100), rsFloat(0x300a02a0, 2.5)))}
				cps := []core.DataSet{rsRBrachyCP(4, "120000", 2.5), rsRBrachyCP(5, "120010", 2.5)}
				if pdr {
					cps = append(cps, rsRBrachyCP(4, "121000", 2.5), rsRBrachyCP(5, "121010", 2.5))
					c = append(c, rsIS(0x30080136, 3), rsIS(0x30080138, 2), rsFloat(0x3008013a, 600), rsFloat(0x3008013c, 600))
					pulses := []core.DataSet{}
					for k := 0; k < 2; k++ {
						pe := []core.Element{rsIS(0x30080172, k+2), rsSeq(0x30080173, cps[k*2], cps[k*2+1])}
						pe = append(pe, rsRSafe([]string{"120000", "121000"}[k], []string{"120010", "121010"}[k])...)
						pulses = append(pulses, rsDS(pe...))
					}
					c = append(c, rsSeq(0x30080171, pulses...))
				} else {
					c = append(c, rsRSafe("120000", "120010")...)
				}
				c = append(c, rsIS(0x300a0110, len(cps)), rsSeq(0x30080160, cps...))
				channels = append(channels, rsDS(c...))
			}
			s := []core.Element{rsIS(0x300c000c, n), rsEl(0x300a0232, core.VRCS, "FLETCHER_SUIT"), rsEl(0x300a0236, core.VRLO, "SYNTH_SETUP_Å"), rsFloat(0x300a0250, 0.25), rsSeq(0x30080130, channels...)}
			s = append(s, rsRSession()...)
			setups = append(setups, rsDS(s...))
		}
		es = append(es, rsSeq(0x30080100, sources...), rsSeq(0x30080110, setups...))
	case RTTreatmentSummaryRecordStorage:
		es = append(es, rsEl(0x30080200, core.VRCS, "ON_BREAK"), rsEl(0x30080202, core.VRST, "SYNTH_SUMMARY"), rsEl(0x30080054, core.VRDA, "20260920"), rsEl(0x30080056, core.VRDA, "20261001"), rsSeq(0x30080220, rsDS(rsIS(0x300c0022, 1), rsEl(0x30080224, core.VRCS, "EXTERNAL_BEAM"), rsIS(0x300a0078, 10), rsIS(0x3008005a, 3), rsSeq(0x30080240, rsDS(rsIS(0x30080223, 3), rsIS(0x300a0705, 3), rsEl(0x30080250, core.VRDA, "20261001"), rsEl(0x30080251, core.VRTM, "120000"), rsEl(0x3008002a, core.VRCS, "OPERATOR"))))), rsSeq(0x300800e0, rsDS(rsIS(0x300c0051, 1), rsEl(0x300a0016, core.VRLO, "SYNTH_MEASURED"), rsFloat(0x30080052, 0.5))), rsSeq(0x30080050, rsDS(rsIS(0x300c0051, 1), rsEl(0x300a0016, core.VRLO, "SYNTH_CALCULATED"), rsFloat(0x30080052, 0.6))))
	}
	// Set by tag so deliberate overrides in the fixture are not duplicates.
	ds := object.New(std.Dictionary)
	for _, e := range es {
		ds.Put(e)
	}
	f, err := derivedio.File(sop, "2.25.1329004101", ds)
	if err != nil {
		t.Fatal(err)
	}
	return rsReparse(t, f)
}
func rsReparse(t testing.TB, f *object.File) *object.File {
	t.Helper()
	var b bytes.Buffer
	if e := object.WriteFile(&b, f); e != nil {
		t.Fatal(e)
	}
	out, e := object.ReadFile(bytes.NewReader(b.Bytes()))
	if e != nil {
		t.Fatal(e)
	}
	return out
}
func rsRecordIOD(t testing.TB, f *object.File) {
	t.Helper()
	report, err := iod.Validate(f.Dataset, iod.Options{CheckValues: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Errors()) != 0 {
		t.Fatalf("IOD errors: %+v", report.Errors())
	}
	vr, err := validation.ValidateDataSet(context.Background(), core.DataSet{Elements: f.Dataset.Elements()}, validation.Options{Dictionary: std.Dictionary, ByteOrder: f.Dataset.ValueByteOrder()})
	if err != nil || vr.Report.HasErrors() {
		t.Fatalf("VR/VM: %v %+v", err, vr.Report)
	}
}
func rsReadRecord(t testing.TB, f *object.File) *TreatmentRecord {
	t.Helper()
	r, e := ReadTreatmentRecord(context.Background(), f, ReadOptions{StrictIOD: true})
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func rsReplaceItem(t testing.TB, o *object.Object, seq core.Tag, index int, fn func(*object.Object)) {
	t.Helper()
	items, ok := o.GetSequence(seq)
	if !ok || index >= len(items) {
		t.Fatal("fixture sequence item missing")
	}
	fn(items[index])
	putSequence(o, seq, items)
}

func TestRecordCompleteIODAndNativeRoundTrips(t *testing.T) {
	for _, test := range []struct {
		name, sop string
		pdr       bool
	}{{"beams", RTBeamsTreatmentRecordStorage, false}, {"ion", RTIonBeamsTreatmentRecordStorage, false}, {"brachyHDR", RTBrachyTreatmentRecordStorage, false}, {"brachyPDR", RTBrachyTreatmentRecordStorage, true}, {"summary", RTTreatmentSummaryRecordStorage, false}} {
		t.Run(test.name, func(t *testing.T) {
			f := rsRecordFixture(t, test.sop, test.pdr)
			rsRecordIOD(t, f)
			model := rsReadRecord(t, f)
			// Independently assert declared scalars/counts instead of using validators
			// to compute the expected result.
			if len(model.BeamSessions) > 0 {
				b := model.BeamSessions[0]
				if *b.ControlPoints[0].ReferencedIndex.Value != 5 || b.ControlPoints[0].DeliveredMeterset != 3 || b.ControlPoints[1].DeliveredMeterset != 7 || *b.DeliveredPrimaryMeterset.Value != 4 || b.TerminationStatus != "OPERATOR" {
					t.Fatal("partial delivery scalar oracle")
				}
				if test.name == "ion" {
					sum := float64(b.ControlPoints[0].SpotMetersets[0]) + float64(b.ControlPoints[0].SpotMetersets[1])
					if sum != 4 || len(b.RangeShifters) != 1 || b.RangeShifters[0].Number != 7 {
						t.Fatal("ion delivery scalar oracle")
					}
				}
			}
			if len(model.BrachySessions) > 0 {
				if len(model.BrachySources) != 2 || len(model.BrachySessions) != 2 || len(model.BrachySessions[0].Channels) != 2 {
					t.Fatal("source/setup/channel count oracle")
				}
				ch := model.BrachySessions[1].Channels[1]
				if ch.SourceNumber != 2 || ch.Number != 2 || ch.SpecifiedTime != 20 || ch.DeliveredTime != 10 || ch.ControlPoints[0].RelativePosition != 2.5 {
					t.Fatal("brachy identity/time/coordinate oracle")
				}
				if test.pdr && (*ch.DeliveredPulses.Value != 2 || len(ch.Pulses) != 2 || ch.Pulses[0].Number != 2 || len(ch.ControlPoints) != 4) {
					t.Fatal("PDR pulse/endpoints oracle")
				}
			}
			if model.Summary != nil && model.Summary.Status != "ON_BREAK" {
				t.Fatal("summary status oracle")
			}
			for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian, transfer.ExplicitVRBigEndian} {
				t.Run(syntax.UID, func(t *testing.T) {
					out, e := WriteTreatmentRecord(context.Background(), model, WriteOptions{Template: f, TransferSyntax: syntax})
					if e != nil {
						t.Fatal(e)
					}
					out = rsReparse(t, out)
					rsRecordIOD(t, out)
					rsExportRecord(t, test.name+"-output-"+syntax.UID, out)
					got := rsReadRecord(t, out)
					if !semanticEqual(model, got) {
						t.Fatal("typed semantic round-trip")
					}
					if p, _ := out.Dataset.GetString(ft(0x00100010)); p != "SYNTH_RECORD^Å" {
						t.Fatal("template Unicode changed")
					}
					if test.name == "ion" {
						for j, cp := range model.BeamSessions[0].ControlPoints {
							for k, v := range cp.SpotMetersets {
								if math.Float32bits(v) != math.Float32bits(got.BeamSessions[0].ControlPoints[j].SpotMetersets[k]) {
									t.Fatal("FL bit round-trip")
								}
							}
						}
					}
				})
			}
			rsExportRecord(t, test.name, f)
		})
	}
}

func TestRecordRefusalsAndPrivacy(t *testing.T) {
	cases := []struct {
		name, sop string
		edit      func(testing.TB, *object.Object)
		want      error
		path      string
	}{
		{"USER", RTBeamsTreatmentRecordStorage, func(t testing.TB, o *object.Object) { o.Put(rsEl(0x300a0709, core.VRCS, "USER")) }, ErrUnsupportedFeature, "(300A,0709)"},
		{"count", RTBeamsTreatmentRecordStorage, func(t testing.TB, o *object.Object) {
			rsReplaceItem(t, o, ft(0x30080020), 0, func(b *object.Object) { b.Put(rsIS(0x300a0110, 3)) })
		}, ErrInvalidObject, "(300A,0110)"},
		{"negativeReference", RTIonBeamsTreatmentRecordStorage, func(t testing.TB, o *object.Object) {
			rsReplaceItem(t, o, ft(0x30080021), 0, func(b *object.Object) {
				rsReplaceItem(t, b, ft(0x30080041), 0, func(c *object.Object) { c.Put(rsIS(0x300c00f0, -1)) })
			})
		}, ErrInvalidObject, "(300C,00F0)"},
		{"nonfinite", RTIonBeamsTreatmentRecordStorage, func(t testing.TB, o *object.Object) {
			rsReplaceItem(t, o, ft(0x30080021), 0, func(b *object.Object) {
				rsReplaceItem(t, b, ft(0x30080041), 0, func(c *object.Object) { c.Put(rsFL(0x30080047, float32(math.NaN()), 2.5)) })
			})
		}, ErrInvalidObject, "(3008,0047)"},
		{"spotSum", RTIonBeamsTreatmentRecordStorage, func(t testing.TB, o *object.Object) {
			rsReplaceItem(t, o, ft(0x30080021), 0, func(b *object.Object) {
				rsReplaceItem(t, b, ft(0x30080041), 0, func(c *object.Object) { c.Put(rsFL(0x30080047, 1, 1)) })
			})
		}, ErrInvalidObject, "(3008,0047)"},
		{"spotMap", RTIonBeamsTreatmentRecordStorage, func(t testing.TB, o *object.Object) {
			rsReplaceItem(t, o, ft(0x30080021), 0, func(b *object.Object) {
				rsReplaceItem(t, b, ft(0x30080041), 0, func(c *object.Object) { c.Put(rsFL(0x300a0394, 1, 2)) })
			})
		}, ErrInvalidObject, "(300A,0394)"},
		{"rangeReference", RTIonBeamsTreatmentRecordStorage, func(t testing.TB, o *object.Object) {
			rsReplaceItem(t, o, ft(0x30080021), 0, func(b *object.Object) {
				rsReplaceItem(t, b, ft(0x30080041), 0, func(c *object.Object) {
					rsReplaceItem(t, c, ft(0x300a0360), 0, func(s *object.Object) { s.Put(rsIS(0x300c0100, 999)) })
				})
			})
		}, ErrInvalidObject, "(300A,0360)"},
		{"sourceReference", RTBrachyTreatmentRecordStorage, func(t testing.TB, o *object.Object) {
			rsReplaceItem(t, o, ft(0x30080110), 0, func(s *object.Object) {
				rsReplaceItem(t, s, ft(0x30080130), 0, func(c *object.Object) { c.Put(rsIS(0x300c000e, 999)) })
			})
		}, ErrInvalidObject, "(300C,000E)"},
		{"invalidTime", RTBrachyTreatmentRecordStorage, func(t testing.TB, o *object.Object) {
			rsReplaceItem(t, o, ft(0x30080110), 0, func(s *object.Object) {
				rsReplaceItem(t, s, ft(0x30080130), 0, func(c *object.Object) { c.Put(rsFloat(0x30080134, -1)) })
			})
		}, ErrInvalidObject, "(3008,0130)"},
		{"sourceDuplicate", RTBrachyTreatmentRecordStorage, func(t testing.TB, o *object.Object) {
			rsReplaceItem(t, o, ft(0x30080100), 1, func(s *object.Object) { s.Put(rsIS(0x300a0212, 1)) })
		}, ErrInvalidObject, "(300A,0212)"},
		{"summaryFractionDuplicate", RTTreatmentSummaryRecordStorage, func(t testing.TB, o *object.Object) {
			rsReplaceItem(t, o, ft(0x30080220), 0, func(g *object.Object) {
				items, _ := g.GetSequence(ft(0x30080240))
				putSequence(g, ft(0x30080240), append(items, items[0]))
			})
		}, ErrInvalidObject, "(3008,0223)"},
		{"localDoseMissing", RTBeamsTreatmentRecordStorage, func(t testing.TB, o *object.Object) {
			rsReplaceItem(t, o, ft(0x30080020), 0, func(b *object.Object) {
				rsReplaceItem(t, b, ft(0x30080080), 0, func(d *object.Object) { d.Put(rsIS(0x30080082, 99)) })
			})
		}, ErrInvalidObject, "(3008,0080)"},
		{"correction", RTBeamsTreatmentRecordStorage, func(t testing.TB, o *object.Object) {
			rsReplaceItem(t, o, ft(0x30080020), 0, func(b *object.Object) {
				rsReplaceItem(t, b, ft(0x30080040), 0, func(c *object.Object) { putSequence(c, ft(0x30080068), nil) })
			})
		}, ErrUnsupportedFeature, "(3008,0068)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := rsRecordFixture(t, tc.sop, false)
			tc.edit(t, f.Dataset)
			rec, err := ReadTreatmentRecord(context.Background(), f, ReadOptions{})
			if rec != nil || !errors.Is(err, tc.want) {
				t.Fatalf("%v %v", rec, err)
			}
			if !strings.Contains(err.Error(), tc.path) {
				t.Fatal(err)
			}
			if strings.Contains(err.Error(), "SYNTH") || strings.Contains(err.Error(), "2.25.") {
				t.Fatal("error disclosed source value")
			}
		})
	}
}

func TestRecordPDRCountAndSourceOwnership(t *testing.T) {
	f := rsRecordFixture(t, RTBrachyTreatmentRecordStorage, true)
	model := rsReadRecord(t, f)
	model.BrachySessions[0].Channels[0].ControlPoints[0].RelativePosition = 999
	again := rsReadRecord(t, f)
	if again.BrachySessions[0].Channels[0].ControlPoints[0].RelativePosition != 2.5 {
		t.Fatal("source alias")
	}
	rsReplaceItem(t, f.Dataset, ft(0x30080110), 0, func(s *object.Object) {
		rsReplaceItem(t, s, ft(0x30080130), 0, func(c *object.Object) { c.Put(rsIS(0x30080138, 3)) })
	})
	if model, err := ReadTreatmentRecord(context.Background(), f, ReadOptions{}); model != nil || !errors.Is(err, ErrInvalidObject) {
		t.Fatal(model, err)
	}
}

func TestRecordWriterOwnershipIdentityAndReferences(t *testing.T) {
	f := rsRecordFixture(t, RTIonBeamsTreatmentRecordStorage, false)
	model := rsReadRecord(t, f)
	var snapshot bytes.Buffer
	if e := object.WriteFile(&snapshot, f); e != nil {
		t.Fatal(e)
	}
	out, err := WriteTreatmentRecord(context.Background(), model, WriteOptions{Template: f})
	if err != nil {
		t.Fatal(err)
	}
	rsReplaceItem(t, out.Dataset, ft(0x30080021), 0, func(b *object.Object) {
		rsReplaceItem(t, b, ft(0x30080041), 0, func(c *object.Object) { c.Put(rsFL(0x300a0394, 999, 0, 1, 0)) })
	})
	var after bytes.Buffer
	if e := object.WriteFile(&after, f); e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(snapshot.Bytes(), after.Bytes()) {
		t.Fatal("writer mutated template")
	}
	if model.BeamSessions[0].ControlPoints[0].SpotPositions[0] != -1 {
		t.Fatal("writer mutated model")
	}
	model.BeamSessions[0].Name = "SYNTH_CHANGED"
	if out, err := WriteTreatmentRecord(context.Background(), model, WriteOptions{Template: f}); out != nil || !errors.Is(err, ErrInvalidObject) {
		t.Fatal(out, err)
	}
	out, err = WriteTreatmentRecord(context.Background(), model, WriteOptions{Template: f, SOPInstanceUID: "2.25.1329004201"})
	if err != nil {
		t.Fatal(err)
	}
	got := rsReadRecord(t, rsReparse(t, out))
	if got.SOPInstanceUID != "2.25.1329004201" || got.BeamSessions[0].Name != "SYNTH_CHANGED" {
		t.Fatal("semantic edit identity")
	}
	plan := &Plan{SOPClassUID: RTIonPlanStorage, SOPInstanceUID: "2.25.1329004110", FractionGroups: []FractionGroup{{Number: 1}}, IonBeams: []IonBeam{{Number: 1, ControlPoints: make([]IonControlPoint, 7), RangeShifters: []RangeShifter{{Number: 7}}}}}
	if e := model.ValidateReferencedPlan(context.Background(), plan); e != nil {
		t.Fatal(e)
	}
	plan.SOPInstanceUID = "2.25.1329004202"
	if e := model.ValidateReferencedPlan(context.Background(), plan); !errors.Is(e, ErrInvalidObject) {
		t.Fatal(e)
	}
	plan.SOPInstanceUID = "2.25.1329004110"
	plan.IonBeams[0].ControlPoints = make([]IonControlPoint, 6)
	if e := model.ValidateReferencedPlan(context.Background(), plan); !errors.Is(e, ErrInvalidObject) {
		t.Fatal(e)
	}
}

func TestRecordLimitsCancellationAndPresence(t *testing.T) {
	f := rsRecordFixture(t, RTIonBeamsTreatmentRecordStorage, false)
	for _, limits := range []Limits{{MaxBeams: 0, MaxControlPoints: 1}, {MaxSpots: 1}, {MaxModelBytes: 128}, {MaxValueBytes: 1}} {
		rec, err := ReadTreatmentRecord(context.Background(), f, ReadOptions{Limits: limits})
		if rec != nil || !errors.Is(err, ErrResourceLimit) {
			t.Fatal(rec, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if rec, err := ReadTreatmentRecord(ctx, f, ReadOptions{}); rec != nil || !errors.Is(err, context.Canceled) {
		t.Fatal(rec, err)
	}
	model := rsReadRecord(t, f)
	if out, err := WriteTreatmentRecord(ctx, model, WriteOptions{Template: f}); out != nil || !errors.Is(err, context.Canceled) {
		t.Fatal(out, err)
	}
	if out, err := WriteTreatmentRecord(context.Background(), model, WriteOptions{Template: f, Limits: Limits{MaxControlPoints: 1}}); out != nil || !errors.Is(err, ErrResourceLimit) {
		t.Fatal(out, err)
	}
	c := model.BeamSessions[0].ControlPoints
	if !c[0].State.TableTopVerticalPosition.Present || c[0].State.TableTopVerticalPosition.Value != nil || !c[1].State.TableTopVerticalPosition.Present || c[1].State.GantryAngle.Value == c[0].State.GantryAngle.Value {
		t.Fatal("Type2/inherited owned state")
	}
	*c[1].State.GantryAngle.Value = 25
	if *c[0].State.GantryAngle.Value != 15 {
		t.Fatal("inherited scalar aliases earlier control point")
	}
	if c[0].State.DevicePositions[0].Positions[0] != -20 {
		t.Fatal("machine device scalar")
	}
	c[1].State.DevicePositions[0].Positions[0] = 0
	if c[0].State.DevicePositions[0].Positions[0] != -20 {
		t.Fatal("inherited device aliases earlier control point")
	}
}

func TestRecordTemplateItemsFollowIdentityWhenReordered(t *testing.T) {
	f := rsRecordFixture(t, RTBrachyTreatmentRecordStorage, false)
	rsReplaceItem(t, f.Dataset, ft(0x30080110), 0, func(s *object.Object) {
		rsReplaceItem(t, s, ft(0x30080130), 0, func(c *object.Object) { c.Put(rsEl(0x77771010, core.VRLO, "SYNTH_OPAQUE_CHANNEL_1")) })
	})
	model := rsReadRecord(t, f)
	ch := model.BrachySessions[0].Channels
	ch[0], ch[1] = ch[1], ch[0]
	model.BrachySessions[0].Channels = ch
	out, e := WriteTreatmentRecord(context.Background(), model, WriteOptions{Template: f, SOPInstanceUID: "2.25.1329004301"})
	if e != nil {
		t.Fatal(e)
	}
	ss, _ := out.Dataset.GetSequence(ft(0x30080110))
	cc, _ := ss[0].GetSequence(ft(0x30080130))
	v, _ := cc[1].GetString(ft(0x77771010))
	if v != "SYNTH_OPAQUE_CHANNEL_1" || cc[0].Has(ft(0x77771010)) {
		t.Fatal("opaque attribute followed array position")
	}
	got := rsReadRecord(t, out)
	if !reflect.DeepEqual(model.BrachySessions[0].Channels, got.BrachySessions[0].Channels) {
		t.Fatal("reordered channel semantics")
	}
}

func rsExportRecord(t testing.TB, name string, f *object.File) {
	t.Helper()
	dir := os.Getenv("DICOMGO_RT_RECORD_FIXTURE_DIR")
	if dir == "" {
		return
	}
	if !filepath.IsAbs(dir) {
		t.Fatal("fixture destination must be absolute")
	}
	if e := os.MkdirAll(dir, 0700); e != nil {
		t.Fatal(e)
	}
	var b bytes.Buffer
	if e := object.WriteFile(&b, f); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(dir, name+".dcm"), b.Bytes(), 0600); e != nil {
		t.Fatal(e)
	}
	// This inventory includes informational/manual IOD findings; zero errors
	// is a bounded structural qualification, not complete clinical validation.
	parsed := rsReparse(t, f)
	report, err := iod.Validate(parsed.Dataset, iod.Options{CheckValues: true})
	if err != nil {
		t.Fatal(err)
	}
	vr, err := validation.ValidateDataSet(context.Background(), core.DataSet{Elements: parsed.Dataset.Elements()}, validation.Options{Dictionary: std.Dictionary, ByteOrder: parsed.Dataset.ValueByteOrder()})
	if err != nil {
		t.Fatal(err)
	}
	sop, _ := parsed.Dataset.GetUID(tagSOPClassUID)
	metadata := struct {
		File, SHA256, SOPClassUID, TransferSyntax string
		IOD                                       iod.Report
		VRVM                                      validation.Report
	}{name + ".dcm", fmt.Sprintf("%x", sha256.Sum256(b.Bytes())), sop, parsed.TransferSyntax.UID, report, vr.Report}
	raw, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".qualification.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}

}

func TestRecordRealBigEndianSourceAndOpaqueFL(t *testing.T) {
	f := rsRecordFixture(t, RTIonBeamsTreatmentRecordStorage, false)
	rsReplaceItem(t, f.Dataset, ft(0x30080021), 0, func(b *object.Object) {
		b.Put(rsEl(0x77770010, core.VRLO, "SYNTH_RT_FAMILY"))
		b.Put(rsFL(0x77771010, 1.25, -3.5))
		b.Put(rsFL(0x300a0356, 1.25))
	})
	f.Dataset = object.FromElements(rsBEElements(f.Dataset.Elements()), std.Dictionary)
	f.Dataset.SetValueByteOrder(binary.BigEndian)
	f.TransferSyntax = transfer.ExplicitVRBigEndian
	f = rsReparse(t, f)
	rsRecordIOD(t, f)
	model := rsReadRecord(t, f)
	if f.Dataset.ValueByteOrder().String() != "BigEndian" {
		t.Fatal("fixture is not a real BE parsed source")
	}
	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian, transfer.ExplicitVRBigEndian} {
		out, e := WriteTreatmentRecord(context.Background(), model, WriteOptions{Template: f, TransferSyntax: syntax})
		if e != nil {
			t.Fatal(e)
		}
		out = rsReparse(t, out)
		rsRecordIOD(t, out)
		got := rsReadRecord(t, out)
		if !semanticEqual(model, got) {
			t.Fatal("BE source typed round-trip")
		}
		beams, _ := out.Dataset.GetSequence(ft(0x30080021))
		floats, e := beams[0].GetFloats(ft(0x300a0356))
		if e != nil || len(floats) != 1 || floats[0] != 1.25 {
			t.Fatal("unmodelled standard FL lost byte order", floats, e)
		}
		private, _ := beams[0].Get(ft(0x77771010))
		if syntax.UID == transfer.ImplicitVRLittleEndian.UID {
			raw, ok := private.Value.(core.RawValue)
			if private.VR() != core.VRUN || !ok || !bytes.Equal(raw, []byte{0, 0, 160, 63, 0, 0, 96, 192}) {
				t.Fatal("implicit unknown private bytes are not preserved as LE UN")
			}
		} else {
			values, e := beams[0].GetFloats(ft(0x77771010))
			if e != nil || len(values) != 2 || values[0] != 1.25 || values[1] != -3.5 {
				t.Fatal("explicit unknown private FL lost byte order")
			}
		}
		rsExportRecord(t, "ion-from-BE-output-"+syntax.UID, out)
		sourceBeams, _ := f.Dataset.GetSequence(ft(0x30080021))
		el, _ := beams[0].Get(ft(0x77771010))
		if raw, ok := el.Value.(core.RawValue); ok && len(raw) > 0 {
			raw[0] ^= 255
		}
		sourceFloats, _ := sourceBeams[0].GetFloats(ft(0x77771010))
		if sourceFloats[0] != 1.25 || sourceFloats[1] != -3.5 {
			t.Fatal("output opaque data aliases source")
		}
	}
	rsExportRecord(t, "ion-BE-source", f)
}

func TestRecordPassiveIonAndOriginProfiles(t *testing.T) {
	f := rsRecordFixture(t, RTIonBeamsTreatmentRecordStorage, false)
	rsReplaceItem(t, f.Dataset, ft(0x30080021), 0, func(b *object.Object) {
		b.Put(rsEl(0x300a00c6, core.VRCS, "ION"))
		b.Put(rsEl(0x300a0308, core.VRCS, "NONE"))
		b.Put(rsIS(0x300a0302, 12))
		b.Put(rsIS(0x300a0304, 6))
		b.Put(rsIS(0x300a0306, 6))
		points, _ := b.GetSequence(ft(0x30080041))
		for _, p := range points {
			for _, tag := range []uint32{0x300a0390, 0x300a0392, 0x300a0394, 0x30080047, 0x300a039a, 0x300a0393, 0x300a0391} {
				p.Remove(ft(tag))
			}
		}
		putSequence(b, ft(0x30080041), points)
	})
	f = rsReparse(t, f)
	rsRecordIOD(t, f)
	for _, origin := range []string{"", "SIMULATION", "DEVICE"} {
		if origin == "" {
			f.Dataset.Remove(ft(0x300a0709))
		} else {
			f.Dataset.Put(rsEl(0x300a0709, core.VRCS, origin))
		}
		r := rsReadRecord(t, f)
		if *r.BeamSessions[0].ChargeState.Value != 6 || r.BeamSessions[0].ScanMode != "NONE" || len(r.BeamSessions[0].ControlPoints[0].SpotMetersets) != 0 {
			t.Fatal("passive species scalar oracle")
		}
		for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian, transfer.ExplicitVRBigEndian} {
			out, e := WriteTreatmentRecord(context.Background(), r, WriteOptions{Template: f, TransferSyntax: syntax})
			if e != nil {
				t.Fatal(e)
			}
			rsRecordIOD(t, rsReparse(t, out))
			if !semanticEqual(r, rsReadRecord(t, rsReparse(t, out))) {
				t.Fatal("passive ion round-trip")
			}
		}
	}
	rsExportRecord(t, "ion-passive-ION", f)
}

func TestRecordPDRDetailedEndpointsAndDwellPairs(t *testing.T) {
	for _, pdr := range []bool{true, false} {
		f := rsRecordFixture(t, RTBrachyTreatmentRecordStorage, pdr)
		rsReplaceItem(t, f.Dataset, ft(0x30080110), 0, func(s *object.Object) {
			rsReplaceItem(t, s, ft(0x30080130), 0, func(c *object.Object) {
				if pdr {
					rsReplaceItem(t, c, ft(0x30080171), 0, func(p *object.Object) {
						rsReplaceItem(t, p, ft(0x30080173), 1, func(cp *object.Object) { cp.Put(rsFloat(0x300a02d2, 7.5)) })
					})
				} else {
					rsReplaceItem(t, c, ft(0x30080160), 1, func(cp *object.Object) { cp.Put(rsFloat(0x300a02d2, 7.5)) })
				}
			})
		})
		if r, e := ReadTreatmentRecord(context.Background(), f, ReadOptions{}); r != nil || !errors.Is(e, ErrInvalidObject) {
			t.Fatal(r, e)
		}
	}
}

func TestRecordDeclaredMetersetRelations(t *testing.T) {
	for _, tag := range []uint32{0x30080036, 0x30080044} {
		f := rsRecordFixture(t, RTBeamsTreatmentRecordStorage, false)
		rsReplaceItem(t, f.Dataset, ft(0x30080020), 0, func(b *object.Object) {
			if tag == 0x30080036 {
				b.Put(rsFloat(tag, 99))
			} else {
				rsReplaceItem(t, b, ft(0x30080040), 0, func(cp *object.Object) { cp.Put(rsFloat(0x30080042, 5)) })
			}
		})
		if r, e := ReadTreatmentRecord(context.Background(), f, ReadOptions{}); r != nil || !errors.Is(e, ErrInvalidObject) {
			t.Fatal(r, e)
		}
	}
}

// Build a genuine BE source independently from the writer under test. The
// fixture started as parsed LE RawValues. Re-encode only numeric VR words;
// text bytes remain unchanged and sequence children retain their structure.
func rsBEElements(es []core.Element) []core.Element {
	out := make([]core.Element, len(es))
	for i, e := range es {
		out[i] = e
		if raw, ok := e.Value.(core.RawValue); ok {
			b := append(core.RawValue(nil), raw...)
			width := 0
			switch e.VR() {
			case core.VRUS, core.VRSS:
				width = 2
			case core.VRUL, core.VRSL, core.VRFL, core.VRAT:
				width = 4
			case core.VRUV, core.VRSV, core.VRFD:
				width = 8
			}
			if width > 0 {
				for start := 0; start+width <= len(b); start += width {
					if e.VR() == core.VRAT {
						b[start], b[start+1] = b[start+1], b[start]
						b[start+2], b[start+3] = b[start+3], b[start+2]
					} else {
						for left, right := start, start+width-1; left < right; left, right = left+1, right-1 {
							b[left], b[right] = b[right], b[left]
						}
					}
				}
			}
			out[i].Value = b
		}
		if sq, ok := e.Value.(core.SequenceValue); ok {
			items := make([]core.DataSet, len(sq.Items))
			for k, it := range sq.Items {
				items[k] = core.DataSet{Elements: rsBEElements(it.Elements)}
			}
			out[i].Value = core.SequenceValue{Items: items}
		}
	}
	return out
}

func TestRecordFileIdentityAndWriterRefusals(t *testing.T) {
	for _, tag := range []uint32{0x00020002, 0x00020003, 0x00020010} {
		f := rsRecordFixture(t, RTIonBeamsTreatmentRecordStorage, false)
		f.Meta.Put(rsEl(tag, core.VRUI, "2.25.1329999"))
		if rec, err := ReadTreatmentRecord(context.Background(), f, ReadOptions{}); rec != nil || !errors.Is(err, ErrInvalidObject) {
			t.Fatal("file identity accepted", rec, err)
		}
	}
	f := rsRecordFixture(t, RTBeamsTreatmentRecordStorage, false)
	r := rsReadRecord(t, f)
	r.BeamSessions[0].DeliveredPrimaryMeterset = Value(math.Inf(1))
	if out, err := WriteTreatmentRecord(context.Background(), r, WriteOptions{Template: f, SOPInstanceUID: "2.25.1329004401"}); out != nil || !errors.Is(err, ErrInvalidObject) {
		t.Fatal(out, err)
	}
	if rec, err := ReadTreatmentRecordDataset(context.Background(), nil, ReadOptions{}); rec != nil || !errors.Is(err, ErrInvalidObject) {
		t.Fatal(rec, err)
	}
	if out, err := WriteTreatmentRecord(context.Background(), nil, WriteOptions{}); out != nil || !errors.Is(err, ErrInvalidObject) {
		t.Fatal(out, err)
	}
}
