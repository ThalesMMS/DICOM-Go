package index

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func persistentFixture() PersistentRecord {
	return PersistentRecord{Record: Record{Source: SourceInfo{Size: 400}, FileMeta: FileMeta{TransferSyntaxUID: "1.2.840.10008.1.2.1", MediaStorageSOPClassUID: "1.2.840.10008.5.1.4.1.1.7", MediaStorageSOPInstanceUID: "2.25.1357"}, Patient: &Patient{ID: "SYNTHETIC"}, Study: Study{InstanceUID: "2.25.13571"}, Series: Series{InstanceUID: "2.25.13572"}, Instance: Instance{SOPClassUID: "1.2.840.10008.5.1.4.1.1.7", SOPInstanceUID: "2.25.1357"}}, Attributes: map[string]string{"00200010": "TEST"}, FileBytes: 400, SHA256: sha256.Sum256([]byte("synthetic bytes"))}
}

func TestPersistentIndexRefusesAmbiguousJSON(t *testing.T) {
	row := persistentFixture()
	data, err := json.Marshal(persistentEnvelope{Version: 1, Value: row})
	if err != nil {
		t.Fatal(err)
	}
	key := persistentKey(row.Record.Instance.SOPInstanceUID)
	for _, bad := range [][]byte{[]byte(strings.Replace(string(data), `"version":1`, `"version":1,"version":1`, 1)), append(append([]byte{}, data...), []byte(`{}`)...), []byte(strings.Replace(string(data), `"version":1`, `"version":2`, 1)), []byte(strings.Replace(string(data), `"SYNTHETIC"`, `"`+string([]byte{0xFF})+`"`, 1))} {
		if _, err := decodePersistent(key, bad); err == nil {
			t.Fatal("ambiguous record admitted")
		}
	}
	var shape map[string]any
	alias := []byte(strings.Replace(string(data), `"version":1`, `"version":1,"Version":1`, 1))
	if _, err := decodePersistent(key, alias); err == nil {
		t.Fatal("logical duplicate alias admitted")
	}
	unicodeAlias := []byte(strings.Replace(string(data), `"version":1`, `"version":0,"verſion":1`, 1))
	if _, err := decodePersistent(key, unicodeAlias); err == nil {
		t.Fatal("Unicode case alias admitted")
	}
	if err := json.Unmarshal(data, &shape); err != nil {
		t.Fatal(err)
	}
	value := shape["value"].(map[string]any)
	value["sha256"] = []int{1}
	bad, _ := json.Marshal(shape)
	if _, err := decodePersistent(key, bad); err == nil {
		t.Fatal("short digest admitted")
	}
}

func TestPersistentAdmissionMatchesEncodingAndRejectsAuthoredBudget(t *testing.T) {
	for _, text := range []string{"ordinary", `<>&"\\`, "\x00\n\t", "日本語\u2028\u2029"} {
		row := persistentFixture()
		row.Record.Source.Origin = text
		row.Record.Geometry = &Geometry{PixelSpacing: []float64{1e-8, 1e22}, ImagePositionPatient: []float64{1.23456789}}
		data, err := json.Marshal(persistentEnvelope{Version: 1, Value: row})
		if err != nil {
			t.Fatal(err)
		}
		if err := persistentAdmission(row, int64(len(data))); err != nil {
			t.Fatalf("exact admission %d %v", len(data), err)
		}
		if err := persistentAdmission(row, int64(len(data)-1)); !errors.Is(err, ErrResourceLimit) {
			t.Fatalf("one-byte quota %v", err)
		}
	}
	p, err := OpenPersistentStore(context.Background(), t.TempDir(), PersistentStoreOptions{MaxRecordBytes: 2048, MaxTotalBytes: 8192})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for _, edit := range []func(*PersistentRecord){func(r *PersistentRecord) { r.Record.Source.Origin = strings.Repeat("x", 4096) }, func(r *PersistentRecord) { r.Record.Patient.Name = strings.Repeat("<", 1000) }, func(r *PersistentRecord) { r.Record.Geometry = &Geometry{PixelSpacing: make([]float64, 65)} }, func(r *PersistentRecord) { r.Record.Diagnostics = make([]Diagnostic, DefaultMaxDiagnostics+1) }} {
		row := persistentFixture()
		edit(&row)
		if _, err := p.Insert(context.Background(), row); !errors.Is(err, ErrResourceLimit) {
			t.Fatalf("authored admission %v", err)
		}
	}
}

func TestPersistentIndexReplayDetachedMetadata(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	p, err := OpenPersistentStore(ctx, dir, PersistentStoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	row := persistentFixture()
	created, err := p.Insert(ctx, row)
	if err != nil || !created {
		t.Fatalf("insert %v %v", created, err)
	}
	row.Record.Patient.ID = "changed"
	row.Attributes["00200010"] = "changed"
	if created, err := p.Insert(ctx, persistentFixture()); err != nil || created {
		t.Fatalf("duplicate %v %v", created, err)
	}
	p.Close()
	p, err = OpenPersistentStore(ctx, dir, PersistentStoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	rows, err := p.Records(ctx)
	if err != nil || len(rows) != 1 || rows[0].Record.Patient.ID != "SYNTHETIC" || rows[0].Attributes["00200010"] != "TEST" {
		t.Fatalf("replay %+v %v", rows, err)
	}
	rows[0].Record.Patient.ID = "borrowed"
	rows[0].Attributes["00200010"] = "borrowed"
	again, _ := p.Records(ctx)
	if again[0].Record.Patient.ID != "SYNTHETIC" || again[0].Attributes["00200010"] != "TEST" {
		t.Fatal("borrowed metadata")
	}
}

func TestPersistentIndexIdentityAndBudgets(t *testing.T) {
	p, err := OpenPersistentStore(context.Background(), t.TempDir(), PersistentStoreOptions{MaxRecords: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for _, mutation := range []func(*PersistentRecord){func(r *PersistentRecord) { r.Record.Series.InstanceUID = "invalid" }, func(r *PersistentRecord) { r.Record.FileMeta.MediaStorageSOPInstanceUID = "2.25.1" }, func(r *PersistentRecord) { r.FileBytes++ }, func(r *PersistentRecord) { r.SHA256 = [32]byte{} }, func(r *PersistentRecord) { r.Attributes["bad"] = "x" }} {
		r := persistentFixture()
		mutation(&r)
		if _, err := p.Insert(context.Background(), r); !errors.Is(err, ErrPersistentIndex) {
			t.Fatal(err)
		}
	}
	r := persistentFixture()
	if _, err := p.Insert(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	r.Record.Instance.SOPInstanceUID = "2.25.222"
	r.Record.FileMeta.MediaStorageSOPInstanceUID = r.Record.Instance.SOPInstanceUID
	if _, err := p.Insert(context.Background(), r); !errors.Is(err, ErrResourceLimit) {
		t.Fatal(err)
	}
}
