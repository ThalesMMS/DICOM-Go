package quirkcorpus

type Manifest struct {
	Schema     int
	Provenance string
	Missing    []string
	Fixtures   []ManifestFixture
}

type ManifestFixture struct {
	Fixture
	File         string
	Bytes        int
	SourceSHA256 string
}

func NewManifest(fixtures []Fixture) Manifest {
	m := Manifest{Schema: 1, Provenance: "Own synthetic, non-PHI structural analogues; repository license; no vendor/GDCM data imported", Missing: []string{"Original PMS/GEMS/Siemens/Philips/GE/ACUSON/RMI vendor files", "Original gdcmConformanceTests/gdcmData files and their file-specific redistribution licenses", "Actual 512x512x2496 ultrasound and 215 MiB XADC workloads; no large-file performance evidence"}}
	for _, f := range fixtures {
		size, hash := len(f.Wire), Hash(f.Wire)
		f.Wire = nil // The manifest describes the carrier; it never embeds it.
		m.Fixtures = append(m.Fixtures, ManifestFixture{Fixture: f, File: f.ID + ".dcm", Bytes: size, SourceSHA256: hash})
	}
	return m
}
