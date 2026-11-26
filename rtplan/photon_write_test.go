package rtplan

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/clifile"
	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func photonFixture(t *testing.T) *object.File {
	t.Helper()
	f := newPlanFixture()
	arc := &f.beams[1]
	last := &arc.cps[len(arc.cps)-1]
	last.attrs.set(derivedio.CS(tagGantryRotationDirection, RotationNone))
	f.fractionGroups[0].attrs.set(derivedio.CS(tagBeamDoseMeaning, "BEAM_LEVEL"))
	return f.file(t)
}

func TestWritePhotonPlanPart10RoundTrip(t *testing.T) {
	const retained = "SYNTH_LINAC_SITE"
	ctx := context.Background()
	template := photonFixture(t)
	beams, _ := template.Dataset.GetSequence(tagBeamSequence)
	beams[0].Put(derivedio.LO(ft(0x00080080), retained))
	putSequence(template.Dataset, tagBeamSequence, beams)

	plan, err := ReadWithOptions(ctx, template, ReadOptions{StrictIOD: true})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Beams[1].ControlPoints[1].Encodes(FieldGantryRotationDirection) ||
		plan.Beams[1].ControlPoints[2].Encodes(FieldDevicePositions) {
		t.Fatal("source must exercise sparse control point encoding")
	}
	before := photonFileBytes(t, template)
	for _, syntax := range []transfer.Syntax{
		transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian, transfer.ExplicitVRBigEndian,
	} {
		t.Run(syntax.UID, func(t *testing.T) {
			out, err := WritePlan(ctx, plan, WriteOptions{Template: template, TransferSyntax: syntax})
			if err != nil {
				t.Fatal(err)
			}
			reopened := rsReparse(t, out)
			back, err := ReadWithOptions(ctx, reopened, ReadOptions{StrictIOD: true})
			if err != nil {
				t.Fatal(err)
			}
			if !semanticEqual(plan, back) || back.SOPInstanceUID != testPlanUID || reopened.TransferSyntax.UID != syntax.UID {
				t.Fatal("effective photon plan, file identity or transfer syntax changed")
			}
			metaUID, _ := reopened.Meta.GetUID(ft(0x00020003))
			if metaUID != testPlanUID || back.ReferencedStructureSet.SOPInstanceUID != testStructUID ||
				back.FractionGroups[0].ReferencedBeams[1].BeamNumber != 2 ||
				back.FractionGroups[0].BeamDoseMeaning != "BEAM_LEVEL" ||
				*back.Beams[0].ReferencedPatientSetupNumber != 1 || *back.Beams[0].ReferencedToleranceTableNumber != 1 {
				t.Fatal("referential identity or plan relationships changed")
			}
			static := back.Beams[0].ControlPoints
			arc := back.Beams[1].ControlPoints
			if static[1].Encodes(FieldDevicePositions) || static[1].Encodes(FieldGantryAngle) {
				t.Fatal("static final point should inherit stationary devices and gantry")
			}
			for i, cp := range arc {
				if !cp.Encodes(FieldGantryAngle) || !cp.Encodes(FieldGantryRotationDirection) {
					t.Fatalf("arc point %d omitted a parameter that changes in the beam", i)
				}
				if i > 0 && !slices.Equal(cp.EncodedDeviceTypes, []string{DeviceMLCX}) {
					t.Fatalf("arc point %d should encode only the changing MLC, got %v", i, cp.EncodedDeviceTypes)
				}
			}
			mlc, ok := arc[2].Device(DeviceMLCX)
			if !ok || !slices.Equal(mlc.Positions, banks(-10, 10)) {
				t.Fatal("final arc MLC did not preserve the source's inherited positions")
			}
			rawBeams, ok := reopened.Dataset.GetSequence(tagBeamSequence)
			if site, _ := rawBeams[0].GetString(ft(0x00080080)); !ok || site != retained {
				t.Fatal("keyed beam lost optional institution metadata")
			}
		})
	}
	if !bytes.Equal(before, photonFileBytes(t, template)) {
		t.Fatal("writing mutated the template")
	}
}

func TestWritePhotonSemanticEditNeedsNewIdentity(t *testing.T) {
	ctx := context.Background()
	template := photonFixture(t)
	p, err := Read(template)
	if err != nil {
		t.Fatal(err)
	}
	p.Beams[1], p.Beams[0] = p.Beams[0], p.Beams[1]
	p.FractionGroups[0].BeamDoseMeaning = "FRACTION_LEVEL"
	p.Beams[0].ControlPoints[0].DoseReferences = []ControlPointDoseReference{{Number: 1}}
	if file, err := WritePlan(ctx, p, WriteOptions{Template: template}); file != nil || !errors.Is(err, ErrInvalidObject) {
		t.Fatalf("edited plan retained UID, file=%v err=%v", file != nil, err)
	}
	const nextUID = "2.25.184401"
	file, err := WritePlan(ctx, p, WriteOptions{Template: template, SOPInstanceUID: nextUID})
	if err != nil {
		t.Fatal(err)
	}
	back, err := Read(rsReparse(t, file))
	if err != nil || back.SOPInstanceUID != nextUID || !semanticEqual(p, back) ||
		back.Beams[0].Number != 2 || back.FractionGroups[0].ReferencedBeams[0].BeamNumber != 1 {
		t.Fatalf("edited plan lost ordered beam identity or relationships: %v", err)
	}
	beams, _ := file.Dataset.GetSequence(tagBeamSequence)
	points, _ := beams[0].GetSequence(tagControlPointSequence)
	refs, _ := points[0].GetSequence(tagReferencedDoseReferenceSeq)
	if len(refs) != 1 || !refs[0].Has(tagCumulativeDoseRefCoefficient) {
		t.Fatal("new control-point dose reference lost required empty coefficient")
	}
}

func TestWritePhotonRejectsInvalidAndUnsupportedWithoutEditingTemplate(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		edit func(*Plan, *object.File)
		want error
	}{
		{"unknown fraction beam reference", func(p *Plan, _ *object.File) { p.FractionGroups[0].ReferencedBeams[0].BeamNumber = 42 }, ErrInvalidObject},
		{"wrong structure set class", func(p *Plan, _ *object.File) { p.ReferencedStructureSet.SOPClassUID = RTPlanStorage }, ErrInvalidObject},
		{"missing patient setup", func(p *Plan, _ *object.File) { p.PatientSetups = nil }, ErrInvalidObject},
		{"missing tolerance table", func(p *Plan, _ *object.File) { p.ToleranceTables = nil }, ErrInvalidObject},
		{"reference to new plan itself", func(p *Plan, _ *object.File) { p.ReferencedStructureSet.SOPInstanceUID = "2.25.184402" }, ErrInvalidObject},
		{"invalid index", func(p *Plan, _ *object.File) { p.Beams[1].ControlPoints[2].Index = 4 }, ErrInvalidObject},
		{"nonzero starting meterset", func(p *Plan, _ *object.File) { *p.Beams[1].ControlPoints[0].CumulativeMetersetWeight = 0.1 }, ErrInvalidObject},
		{"angle jumps without direction", func(p *Plan, _ *object.File) { p.Beams[1].ControlPoints[0].GantryRotationDirection = RotationNone }, ErrInvalidObject},
		{"moving static aperture", func(p *Plan, _ *object.File) { p.Beams[0].ControlPoints[1].DevicePositions[0].Positions[0] = -49 }, ErrInvalidObject},
		{"approval of a new instance", func(p *Plan, _ *object.File) { p.ApprovalStatus = "APPROVED" }, ErrInvalidObject},
		{"electron beam", func(p *Plan, _ *object.File) { p.Beams[0].RadiationType = "ELECTRON" }, ErrUnsupportedFeature},
		{"changing nominal energy", func(p *Plan, _ *object.File) { *p.Beams[1].ControlPoints[2].NominalBeamEnergy = 10 }, ErrUnsupportedFeature},
		{"wedge", func(p *Plan, _ *object.File) { p.Beams[0].NumberOfWedges = 1 }, ErrUnsupportedFeature},
		{"gantry pitch in template", func(_ *Plan, f *object.File) {
			beams, _ := f.Dataset.GetSequence(tagBeamSequence)
			points, _ := beams[0].GetSequence(tagControlPointSequence)
			points[0].Put(core.Element{
				Header: core.ElementHeader{Tag: ft(0x300a014a), VR: core.VRFL},
				Value:  core.Float32Value{0},
			})
			putSequence(beams[0], tagControlPointSequence, points)
			putSequence(f.Dataset, tagBeamSequence, beams)
		}, ErrUnsupportedFeature},
		{"enhanced device flag in template", func(_ *Plan, f *object.File) {
			beams, _ := f.Dataset.GetSequence(tagBeamSequence)
			beams[0].Put(derivedio.CS(ft(0x300800a3), "YES"))
			putSequence(f.Dataset, tagBeamSequence, beams)
		}, ErrUnsupportedFeature},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := photonFixture(t)
			p, err := Read(f)
			if err != nil {
				t.Fatal(err)
			}
			tc.edit(p, f)
			before := photonFileBytes(t, f)
			out, err := WritePlan(ctx, p, WriteOptions{Template: f, SOPInstanceUID: "2.25.184402"})
			if out != nil || !errors.Is(err, tc.want) {
				t.Fatalf("out=%v error=%v, expected %v", out != nil, err, tc.want)
			}
			if !bytes.Equal(before, photonFileBytes(t, f)) {
				t.Fatal("failed write changed the source")
			}
		})
	}
}

func TestWritePhotonLimitsCancellationAndBigEndianTemplate(t *testing.T) {
	ctx := context.Background()
	template := photonFixture(t)
	plan, err := Read(template)
	if err != nil {
		t.Fatal(err)
	}
	before := photonFileBytes(t, template)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	for _, tc := range []struct {
		name    string
		ctx     context.Context
		options WriteOptions
		want    error
	}{
		{"beams", ctx, WriteOptions{Template: template, Limits: Limits{MaxBeams: 1}}, ErrResourceLimit},
		{"control points", ctx, WriteOptions{Template: template, Limits: Limits{MaxControlPoints: 4}}, ErrResourceLimit},
		{"cancelled", cancelled, WriteOptions{Template: template}, context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if out, err := WritePlan(tc.ctx, plan, tc.options); out != nil || !errors.Is(err, tc.want) {
				t.Fatalf("out=%v err=%v, expected %v", out != nil, err, tc.want)
			}
		})
	}
	if !bytes.Equal(before, photonFileBytes(t, template)) {
		t.Fatal("admission failure mutated template")
	}
	be, err := WritePlan(ctx, plan, WriteOptions{Template: template, TransferSyntax: transfer.ExplicitVRBigEndian})
	if err != nil {
		t.Fatal(err)
	}
	be = rsReparse(t, be)
	if be.Dataset.ValueByteOrder() != binary.BigEndian {
		t.Fatal("test requires a real Big Endian source")
	}
	model, err := Read(be)
	if err != nil {
		t.Fatal(err)
	}
	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian, transfer.ExplicitVRBigEndian} {
		out, err := WritePlan(ctx, model, WriteOptions{Template: be, TransferSyntax: syntax})
		if err != nil {
			t.Fatal(err)
		}
		back, err := Read(rsReparse(t, out))
		if err != nil || !semanticEqual(plan, back) {
			t.Fatal("real Big Endian source round trip", err)
		}
	}
}

func TestWritePhotonPlanAtomicPublicationFailure(t *testing.T) {
	dir := t.TempDir()
	f := photonFixture(t)
	p, err := Read(f)
	if err != nil {
		t.Fatal(err)
	}
	out, err := WritePlan(context.Background(), p, WriteOptions{Template: f})
	if err != nil {
		t.Fatal(err)
	}

	destination := filepath.Join(dir, "plan.dcm")
	previous := []byte("previous destination")
	if err := os.WriteFile(destination, previous, 0600); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected serialization failure after prefix")
	err = clifile.WriteAtomic(destination, func(w io.Writer) error {
		return object.WriteFile(photonPrefixFailure{w, injected}, out)
	})
	if !errors.Is(err, injected) {
		t.Fatalf("atomic serializer must report partial write failure, got %v", err)
	}
	after, err := os.ReadFile(destination)
	if err != nil || !bytes.Equal(after, previous) {
		t.Fatalf("failed write altered destination: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("failed write retained temporary files: %v", err)
	}
}

type photonPrefixFailure struct {
	io.Writer
	err error
}

func (w photonPrefixFailure) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p[:min(len(p), 70)])
	if err != nil {
		return n, err
	}
	return n, w.err
}

func photonFileBytes(t *testing.T, f *object.File) []byte {
	t.Helper()
	var wire bytes.Buffer
	if err := object.WriteFile(&wire, f); err != nil {
		t.Fatal(err)
	}
	return wire.Bytes()
}

// Export synthetic Part 10 files through the existing RT family evidence helper
// for DCMTK drtdump and dicom3tools dciodvfy.
func TestExportPhotonPlanQualification(t *testing.T) {
	dir := os.Getenv("DICOMGO_RT_FAMILY_FIXTURE_DIR")
	if dir == "" {
		t.Skip("set an absolute DICOMGO_RT_FAMILY_FIXTURE_DIR for synthetic evidence")
	}
	t.Setenv("DICOMGO_RT_RECORD_FIXTURE_DIR", dir)
	ctx := context.Background()
	template := photonFixture(t)
	p, err := Read(template)
	if err != nil {
		t.Fatal(err)
	}
	rsExportRecord(t, "photon-plan-sparse-template", template)
	for _, syntax := range []transfer.Syntax{
		transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian, transfer.ExplicitVRBigEndian,
	} {
		out, err := WritePlan(ctx, p, WriteOptions{Template: template, TransferSyntax: syntax})
		if err != nil {
			t.Fatal(err)
		}
		rsExportRecord(t, "photon-plan-"+strings.ReplaceAll(syntax.UID, ".", "_"), out)
	}
	bad, err := object.CloneFile(template)
	if err != nil {
		t.Fatal(err)
	}
	beams, _ := bad.Dataset.GetSequence(tagBeamSequence)
	beams[0].Remove(tagBeamNumber)
	putSequence(bad.Dataset, tagBeamSequence, beams)
	rsExportRecord(t, "photon-plan-invalid-beam-number", bad)
}

func TestPhotonWriterOutputIsReadByDCMTK(t *testing.T) {
	drtdump := dcmtkoracle.Tool(t, "drtdump")
	template := photonFixture(t)
	plan, err := Read(template)
	if err != nil {
		t.Fatal(err)
	}
	for _, syntax := range []transfer.Syntax{
		transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian, transfer.ExplicitVRBigEndian,
	} {
		t.Run(syntax.UID, func(t *testing.T) {
			file, err := WritePlan(context.Background(), plan, WriteOptions{Template: template, TransferSyntax: syntax})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "photon.dcm")
			if err := os.WriteFile(path, photonFileBytes(t, file), 0600); err != nil {
				t.Fatal(err)
			}
			out := string(dcmtkoracle.Run(t, dcmtkoracle.Accepted, drtdump, "-v", path))
			if !strings.Contains(out, "RT Plan object") {
				t.Fatal("DCMTK did not read output as RT Plan", out)
			}
			for _, line := range strings.Split(out, "\n") {
				if strings.HasPrefix(line, "E:") || strings.HasPrefix(line, "W:") {
					t.Fatal("DCMTK output diagnostic", line)
				}
			}
		})
	}
}
