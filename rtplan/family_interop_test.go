package rtplan

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/validation/iod"
)

// Exports only synthetic fixtures and complete qualification findings for an
// optional external oracle. It does not depend on or invoke an external tool.
func TestExportRTFamilyQualification(t *testing.T) {
	dir := os.Getenv("DICOMGO_RT_FAMILY_FIXTURE_DIR")
	if dir == "" {
		t.Skip("set an absolute DICOMGO_RT_FAMILY_FIXTURE_DIR to export synthetic evidence")
	}
	if !filepath.IsAbs(dir) {
		t.Fatal("fixture destination must be absolute")
	}
	t.Setenv("DICOMGO_RT_RECORD_FIXTURE_DIR", dir)
	for _, family := range []string{"ion-plan", "brachy-plan", "brachy-pdr-plan"} {
		fixture := newIonFixture()
		if family != "ion-plan" {
			fixture = newBrachyFixture()
		}
		f := fixture.file(t)
		if family == "brachy-pdr-plan" {
			f.Dataset.Put(derivedio.CS(tagBrachyTreatmentType, "PDR"))
			ss, _ := f.Dataset.GetSequence(tagApplicationSetupSequence)
			for _, s := range ss {
				cc, _ := s.GetSequence(tagChannelSequence)
				for _, c := range cc {
					c.Put(derivedio.IS(ft(0x300a028a), 5))
					c.Put(derivedio.DS(ft(0x300a028c), 3600))
				}
				putSequence(s, tagChannelSequence, cc)
			}
			putSequence(f.Dataset, tagApplicationSetupSequence, ss)
			f = rsReparse(t, f)
		}
		p, err := ReadWithOptions(context.Background(), f, ReadOptions{StrictIOD: true})
		if err != nil {
			t.Fatal(err)
		}
		rsExportRecord(t, family+"-source", f)
		for _, ts := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian, transfer.ExplicitVRBigEndian} {
			out, err := WritePlan(context.Background(), p, WriteOptions{Template: f, TransferSyntax: ts})
			if err != nil {
				t.Fatal(err)
			}
			rsExportRecord(t, family+"-output-"+ts.UID, out)
		}
		if family != "brachy-pdr-plan" {
			bad, err := object.CloneFile(f)
			if err != nil {
				t.Fatal(err)
			}
			bad.Dataset.Remove(tagRTPlanLabel)
			rsExportRecord(t, family+"-invalid-label", bad)
		}
	}
	for _, bits := range []int{8, 16} {
		for _, portal := range []bool{false, true} {
			f := newImageFixture(t, bits, true)
			name := "rtimage-8-drr"
			if bits == 16 {
				name = "rtimage-16-drr"
			}
			if portal {
				if bits == 8 {
					name = "rtimage-8-portal"
				} else {
					name = "rtimage-16-portal"
				}
				f.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: ft(0x00080008), VR: core.VRCS}, Value: core.StringValue{"ORIGINAL", "PRIMARY", "PORTAL"}})
				f.Dataset.Put(derivedio.CS(ft(0x3002000a), "ACTUAL"))
				f.Dataset.Put(derivedio.Seq(ft(0x30020030), derivedio.DataSet(derivedio.Str(ft(0x00180060), core.VRDS, ""), derivedio.Str(ft(0x30020032), core.VRDS, ""), derivedio.IS(tagNumberOfBlocks, 0))))
			}
			f = rsReparse(t, f)
			img, err := ReadImage(context.Background(), f, ReadOptions{StrictIOD: true})
			if err != nil {
				report, _ := iod.Validate(f.Dataset, iod.Options{CheckValues: true})
				t.Fatalf("%s %v %+v", name, err, report.Errors())
			}
			rsExportRecord(t, name+"-source", f)
			for _, ts := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian, transfer.ExplicitVRBigEndian} {
				out, err := WriteImage(context.Background(), img, WriteOptions{Template: f, TransferSyntax: ts})
				if err != nil {
					t.Fatal(err)
				}
				rsExportRecord(t, name+"-output-"+ts.UID, out)
			}
			if bits == 8 && !portal {
				bad, err := object.CloneFile(f)
				if err != nil {
					t.Fatal(err)
				}
				bad.Dataset.Remove(ft(0x30020002))
				rsExportRecord(t, "rtimage-invalid-label", bad)
			}
		}
	}
}
