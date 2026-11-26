package xdsi

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/tags"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

const testStudy = "1.2.826.0.1.3680043.10.50.1"
const testSeries = "1.2.826.0.1.3680043.10.50.2"
const testImage = "1.2.826.0.1.3680043.10.50.3"
const testImage2 = "1.2.826.0.1.3680043.10.50.4"
const testManifestUID = "1.2.826.0.1.3680043.10.50.5"
const testManifestSeries = "1.2.826.0.1.3680043.10.50.6"
const testSource = "1.2.826.0.1.3680043.10.50.7"
const testCT = "1.2.840.10008.5.1.4.1.1.2"

func testImageObject(instance string) *object.Object {
	ds := derivedio.Object(
		derivedio.UI(tags.SOPClassUID, testCT), derivedio.UI(tags.SOPInstanceUID, instance),
		derivedio.UI(tags.StudyInstanceUID, testStudy), derivedio.UI(tags.SeriesInstanceUID, testSeries),
		derivedio.Str(tags.PatientID, core.VRLO, "patient-local"), derivedio.Str(tags.PatientName, core.VRPN, "SYNTHETIC^XDS"),
		derivedio.Str(tags.Modality, core.VRCS, "CT"),
		derivedio.Str(core.NewTag(0x0008, 0x0020), core.VRDA, "20261116"),
		derivedio.Str(core.NewTag(0x0008, 0x0030), core.VRTM, "100000"),
		derivedio.US(core.NewTag(0x0028, 0x0010), 1), derivedio.US(core.NewTag(0x0028, 0x0011), 2),
		derivedio.US(core.NewTag(0x0028, 0x0002), 1), derivedio.Str(core.NewTag(0x0028, 0x0004), core.VRCS, "MONOCHROME2"),
		derivedio.US(core.NewTag(0x0028, 0x0100), 8), derivedio.US(core.NewTag(0x0028, 0x0101), 8),
		derivedio.US(core.NewTag(0x0028, 0x0102), 7), derivedio.US(core.NewTag(0x0028, 0x0103), 0),
		derivedio.Raw(core.NewTag(0x7fe0, 0x0010), core.VROB, []byte{12, 34}),
	)
	ds.SetSequence(tagPatientIssuerQualifiers, derivedio.Object(
		derivedio.Str(tagUniversalEntityID, core.VRUT, "1.2.826.0.1.3680043.10.51"),
		derivedio.Str(tagUniversalEntityIDType, core.VRCS, "ISO"),
	))
	return ds
}

func testManifestOptions() ManifestOptions {
	return ManifestOptions{
		Source: testImageObject(testImage), SOPInstanceUID: testManifestUID, SeriesUID: testManifestSeries,
		RetrieveLocationUID: testSource, RetrieveAETitle: "XDSISOURCE",
		CreatedAt: time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC),
		References: []InstanceRef{
			{StudyUID: testStudy, SeriesUID: testSeries, SOPClassUID: testCT, SOPInstanceUID: testImage},
			{StudyUID: testStudy, SeriesUID: testSeries, SOPClassUID: testCT, SOPInstanceUID: testImage2},
		},
	}
}

func mustManifest(t *testing.T) *Manifest {
	t.Helper()
	m, err := BuildManifest(context.Background(), testManifestOptions())
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestManifestUsesExplicitPatientAndEvidenceRetrievalIdentities(t *testing.T) {
	m := mustManifest(t)
	if len(m.refs) != 2 || m.sourceUID != testSource || m.patient != (PatientID{"patient-local", "1.2.826.0.1.3680043.10.51"}) {
		t.Fatalf("manifest identity %#v", m)
	}
	file, err := object.ReadFile(bytes.NewReader(m.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	studies, _ := file.Dataset.GetSequence(core.NewTag(0x0040, 0xA375))
	series, _ := studies[0].GetSequence(core.NewTag(0x0008, 0x1115))
	ae, _ := series[0].GetString(core.NewTag(0x0008, 0x0054))
	location, _ := series[0].GetString(core.NewTag(0x0040, 0xE011))
	if ae != "XDSISOURCE" || location != testSource {
		t.Fatal("missing image source route")
	}
	parsed, err := ParseManifest(context.Background(), m.Bytes())
	if err != nil || len(parsed.References()) != 2 {
		t.Fatalf("roundtrip %v", err)
	}
	out := m.Bytes()
	out[0] = 99
	if m.Bytes()[0] == 99 {
		t.Fatal("manifest bytes alias")
	}
	refs := m.References()
	refs[0].SOPInstanceUID = "changed"
	if m.References()[0].SOPInstanceUID == "changed" {
		t.Fatal("references alias")
	}
}

func TestManifestRejectsAmbiguousOrMissingMapping(t *testing.T) {
	tests := map[string]func(*ManifestOptions){
		"no issuer":         func(o *ManifestOptions) { o.Source.SetSequence(tagPatientIssuerQualifiers) },
		"duplicate SOP":     func(o *ManifestOptions) { o.References[1] = o.References[0] },
		"foreign study":     func(o *ManifestOptions) { o.References[1].StudyUID = "1.2.3" },
		"non-image":         func(o *ManifestOptions) { o.References[1].SOPClassUID = "1.2.840.10008.5.1.4.1.1.88.59" },
		"no source":         func(o *ManifestOptions) { o.RetrieveLocationUID = "" },
		"invalid AE":        func(o *ManifestOptions) { o.RetrieveAETitle = "bad\\title" },
		"missing timestamp": func(o *ManifestOptions) { o.CreatedAt = time.Time{} },
		"too many refs":     func(o *ManifestOptions) { o.References = make([]InstanceRef, maxReferences+1) },
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			o := testManifestOptions()
			change(&o)
			if got, err := BuildManifest(context.Background(), o); err == nil || got != nil {
				t.Fatal("invalid manifest accepted")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := BuildManifest(ctx, testManifestOptions()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel = %v", err)
	}
}

func mutatedManifest(t *testing.T, change func(*object.Object)) []byte {
	t.Helper()
	file, err := object.ReadFile(bytes.NewReader(mustManifest(t).Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	change(file.Dataset)
	var out bytes.Buffer
	if err := object.WriteFile(&out, file); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestManifestRejectsDetachedEvidenceAndForeignRoutes(t *testing.T) {
	for name, change := range map[string]func(*object.Object){
		"title": func(ds *object.Object) {
			title, _ := ds.GetSequence(core.NewTag(0x0040, 0xA043))
			title[0].Put(derivedio.Str(core.NewTag(0x0008, 0x0100), core.VRSH, "113001"))
			ds.SetSequence(core.NewTag(0x0040, 0xA043), title...)
		},
		"route": func(ds *object.Object) {
			studies, _ := ds.GetSequence(tagRequestedEvidence)
			series, _ := studies[0].GetSequence(tagReferencedSeries)
			series[0].Put(derivedio.UI(tagRetrieveLocationUID, ""))
			studies[0].SetSequence(tagReferencedSeries, series...)
			ds.SetSequence(tagRequestedEvidence, studies...)
		},
		"content mismatch": func(ds *object.Object) {
			content, _ := ds.GetSequence(core.NewTag(0x0040, 0xA730))
			ds.SetSequence(core.NewTag(0x0040, 0xA730), content[:1]...)
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseManifest(context.Background(), mutatedManifest(t, change))
			if !errors.Is(err, ErrManifest) {
				t.Fatalf("parse = %v", err)
			}
		})
	}
}

func imageBytes(t *testing.T, instance string) []byte {
	t.Helper()
	file, err := derivedio.File(testCT, instance, testImageObject(instance))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := object.WriteFile(&out, file); err != nil {
		t.Fatal(err)
	}
	if file.TransferSyntax.UID != transfer.ExplicitVRLittleEndian.UID {
		t.Fatal("unexpected transfer syntax")
	}
	return out.Bytes()
}

func TestManifestBoundsInheritedCommonMetadata(t *testing.T) {
	for name, change := range map[string]func(*ManifestOptions){
		"large patient": func(o *ManifestOptions) {
			o.Source.Put(derivedio.Str(core.NewTag(0x0010, 0x4000), core.VRLT, strings.Repeat("x", maxManifestBytes)))
		},
		"deep patient reference": func(o *ManifestOptions) {
			node := derivedio.Object(derivedio.Str(tags.PatientID, core.VRLO, "child"))
			for i := 0; i < 13; i++ {
				next := object.New(nil)
				next.SetSequence(core.NewTag(0x0008, 0x1120), node)
				node = next
			}
			o.Source.SetSequence(core.NewTag(0x0008, 0x1120), node)
		},
	} {
		t.Run(name, func(t *testing.T) {
			o := testManifestOptions()
			change(&o)
			if m, err := BuildManifest(context.Background(), o); !errors.Is(err, ErrLimit) || m != nil {
				t.Fatalf("limit %v", err)
			}
		})
	}
}

func TestLeaseReleasedWhenAdmissionReturnsError(t *testing.T) {
	client, peer, _, _ := peerFixture(t)
	released := false
	client.hold = func(context.Context, []InstanceRef) (func(), error) {
		return func() { released = true }, ErrAvailability
	}
	if _, err := client.Publish(context.Background(), mustManifest(t), testMetadata()); !errors.Is(err, ErrAvailability) || !released || peer.published != 0 {
		t.Fatalf("released %v / error %v", released, err)
	}
}
