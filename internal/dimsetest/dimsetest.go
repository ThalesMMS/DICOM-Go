// Package dimsetest provides in-process DIMSE peers and synthetic instances
// for command-line tool tests. Every instance it writes is synthetic.
package dimsetest

import (
	"context"
	"encoding/binary"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// SecondaryCaptureSOPClassUID is the SOP Class of the synthetic images.
const SecondaryCaptureSOPClassUID = "1.2.840.10008.5.1.4.1.1.7"

// BasicTextSRSOPClassUID is the SOP Class of the synthetic non-images.
const BasicTextSRSOPClassUID = "1.2.840.10008.5.1.4.1.1.88.11"

// StartSCP serves associations accepted with accept on a loopback listener
// until the test ends and returns the listening port. handler owns each
// association; it is closed afterwards.
func StartSCP(t testing.TB, accept ul.AcceptOptions, handler func(context.Context, *ul.Association) error) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	server, err := ul.NewAssociationServer(listener, ul.AssociationServerOptions{
		Accept:                    accept,
		MaxConcurrentAssociations: 4,
		SaturationPolicy:          ul.SaturationReject,
		Handler: func(ctx context.Context, assoc *ul.Association) error {
			defer assoc.Close()
			return handler(ctx, assoc)
		},
	})
	if err != nil {
		cancel()
		_ = listener.Close()
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = server.Serve(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		_ = listener.Close()
		<-done
	})
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// Instance describes one synthetic instance.
type Instance struct {
	StudyUID, SeriesUID, SOPInstanceUID string
	// NonImage writes a Basic Text SR without Rows instead of an image.
	NonImage  bool
	PatientID string
	Modality  string
}

// WriteInstance writes inst as an Explicit VR Little Endian Part 10 file
// named after its SOP Instance UID in dir and returns the path.
func WriteInstance(t testing.TB, dir string, inst Instance) string {
	t.Helper()
	sopClass := SecondaryCaptureSOPClassUID
	if inst.NonImage {
		sopClass = BasicTextSRSOPClassUID
	}
	if inst.PatientID == "" {
		inst.PatientID = "SYNTHETIC-1"
	}
	if inst.Modality == "" {
		inst.Modality = "OT"
		if inst.NonImage {
			inst.Modality = "SR"
		}
	}
	str := func(group, element uint16, vr core.VR, value string) core.Element {
		return dicomtest.StringElement(core.NewTag(group, element), vr, value)
	}
	elements := []core.Element{
		str(0x0008, 0x0016, core.VRUI, sopClass),
		str(0x0008, 0x0018, core.VRUI, inst.SOPInstanceUID),
		str(0x0008, 0x0020, core.VRDA, "20261007"),
		str(0x0008, 0x0050, core.VRSH, "ACC-SYNTH"),
		str(0x0008, 0x0060, core.VRCS, inst.Modality),
		str(0x0008, 0x103E, core.VRLO, "SYNTHETIC SERIES"),
		str(0x0010, 0x0010, core.VRPN, "SYNTHETIC^PATIENT"),
		str(0x0010, 0x0020, core.VRLO, inst.PatientID),
		str(0x0010, 0x0030, core.VRDA, "19700101"),
		str(0x0010, 0x0040, core.VRCS, "O"),
		str(0x0020, 0x000D, core.VRUI, inst.StudyUID),
		str(0x0020, 0x000E, core.VRUI, inst.SeriesUID),
		str(0x0020, 0x0010, core.VRSH, "1"),
		str(0x0020, 0x0011, core.VRIS, "1"),
		str(0x0020, 0x0013, core.VRIS, "1"),
	}
	if !inst.NonImage {
		us := func(element uint16, value uint16) core.Element {
			return dicomtest.Uint16Element(core.NewTag(0x0028, element), core.VRUS, binary.LittleEndian, value)
		}
		elements = append(elements,
			us(0x0002, 1),
			str(0x0028, 0x0004, core.VRCS, "MONOCHROME2"),
			us(0x0010, 2), us(0x0011, 2),
			us(0x0100, 8), us(0x0101, 8), us(0x0102, 7), us(0x0103, 0),
			core.NewRawElement(core.NewTag(0x7FE0, 0x0010), core.VROB, []byte{0, 64, 128, 255}),
		)
	}
	data, err := dicomtest.Part10File(transfer.ExplicitVRLittleEndian, elements...)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, inst.SOPInstanceUID+".dcm")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
