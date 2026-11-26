package index

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"unsafe"
)

func poolFixture(id string) Record {
	return Record{Source: SourceInfo{Origin: "synthetic", Size: 42},
		FileMeta: FileMeta{TransferSyntaxUID: "1.2.840", TransferSyntaxName: "synthetic syntax", MediaStorageSOPClassUID: "1.2.3", MediaStorageSOPInstanceUID: id},
		Patient:  &Patient{Name: "Synthétique^患者", ID: "synthetic", BirthDate: "19700101", Sex: "O"},
		Study:    Study{InstanceUID: "1.2.3.4", AccessionNumber: "synthetic", Date: "20261010", Time: "010203", Description: "Síntese 合成"},
		Series:   Series{InstanceUID: "1.2.3.4.5", Modality: "CT", Number: "1", Date: "20261010", Time: "010203", Description: "Síntese 合成"},
		Instance: Instance{SOPClassUID: "1.2.3", SOPInstanceUID: id, Number: "1", NumberOfFrames: 2},
		Selected: map[string][]SelectedElement{"synthetic": nil}, Geometry: &Geometry{Rows: 2, Columns: 3, PixelSpacing: []float64{.7, 1.3}},
		Missing: []MissingField{{Field: "synthetic-missing"}}, Diagnostics: []Diagnostic{{Field: "synthetic-field", Message: "value-free"}}}
}

func TestStringPoolContentUnicodeAndOwnership(t *testing.T) {
	p, err := NewStringPool(StringPoolOptions{})
	if err != nil {
		t.Fatal(err)
	}
	input := poolFixture("1.2.3.4.5.6")
	got, err := p.ShareRecord(nil, input)
	if err != nil || !reflect.DeepEqual(got, input) {
		t.Fatal("field content changed", err)
	}
	// Distinct backing, same byte sequence: identity is observed only in tests.
	second := poolFixture("1.2.3.4.5.7")
	second.Patient.Name = strings.Clone(input.Patient.Name)
	shared, err := p.ShareRecord(nil, second)
	if err != nil || !reflect.DeepEqual(shared, second) {
		t.Fatal("second content", err)
	}
	if unsafe.StringData(got.Patient.Name) != unsafe.StringData(shared.Patient.Name) {
		t.Fatal("equal strings not shared")
	}
	if got.Patient == input.Patient {
		t.Fatal("patient aliased during field replacement")
	}
	got.Patient.Name = "changed"
	if input.Patient.Name != "Synthétique^患者" {
		t.Fatal("input mutated")
	}
	if input.Source.Origin != got.Source.Origin || input.Geometry != got.Geometry {
		t.Fatal("unpooled fields changed")
	}
	if p.Stats().Hits == 0 {
		t.Fatal("no observed sharing")
	}
}

func TestStringPoolBudgetsFullAndLargeBacking(t *testing.T) {
	for _, o := range []StringPoolOptions{{MaxEntries: -1}, {MaxBytes: -1}} {
		if _, err := NewStringPool(o); !errors.Is(err, ErrInvalidOptions) {
			t.Fatal("negative budget")
		}
	}
	p, _ := NewStringPool(StringPoolOptions{MaxEntries: 1, MaxBytes: 8})
	value := strings.Repeat("x", 1<<20)
	short := value[:4]
	input := Record{Study: Study{InstanceUID: short, Description: value}, Series: Series{Modality: "new"}}
	got, err := p.ShareRecord(nil, input)
	if err != nil || !reflect.DeepEqual(got, input) {
		t.Fatal("full pool affected content", err)
	}
	if unsafe.StringData(got.Study.InstanceUID) == unsafe.StringData(short) {
		t.Fatal("large backing retained instead of cloned payload")
	}
	s := p.Stats()
	if s.Entries != 1 || s.Bytes != 4 || s.Bypasses != 2 {
		t.Fatal("budget accounting", s)
	}
	same, _ := p.ShareRecord(nil, Record{Study: Study{InstanceUID: strings.Clone(short)}})
	if unsafe.StringData(same.Study.InstanceUID) != unsafe.StringData(got.Study.InstanceUID) {
		t.Fatal("full pool does not reuse existing entry")
	}
	for i := 0; i < 100; i++ {
		_, err = p.ShareRecord(nil, Record{Study: Study{InstanceUID: strings.Repeat("z", 9)}})
		if err != nil {
			t.Fatal(err)
		}
	}
	if p.Stats().Entries > 1 || p.Stats().Bytes > 8 {
		t.Fatal("unbounded bypass")
	}
	bytesOnly, _ := NewStringPool(StringPoolOptions{MaxEntries: 8, MaxBytes: 5})
	byteInput := Record{Study: Study{InstanceUID: "abcd"}, Series: Series{Modality: "zz"}}
	byteOut, err := bytesOnly.ShareRecord(nil, byteInput)
	if err != nil || !reflect.DeepEqual(byteOut, byteInput) || bytesOnly.Stats().Entries != 1 || bytesOnly.Stats().Bytes != 4 || bytesOnly.Stats().Bypasses != 1 {
		t.Fatal("independent byte budget", err, bytesOnly.Stats())
	}
}

type poolCancelContext struct {
	context.Context
	calls, at int
}

func (c *poolCancelContext) Err() error {
	c.calls++
	if c.calls >= c.at {
		return context.Canceled
	}
	return nil
}
func TestStringPoolCancellationAndCloseLifetime(t *testing.T) {
	p, _ := NewStringPool(StringPoolOptions{})
	input := poolFixture("1.2.3.8")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if out, err := p.ShareRecord(ctx, input); !errors.Is(err, context.Canceled) || !reflect.DeepEqual(out, Record{}) || p.Stats().Entries != 0 {
		t.Fatal("pre-cancel")
	}
	during := &poolCancelContext{Context: context.Background(), at: 8}
	if out, err := p.ShareRecord(during, input); !errors.Is(err, context.Canceled) || !reflect.DeepEqual(out, Record{}) {
		t.Fatal("partial record published")
	}
	got, err := p.ShareRecord(nil, input)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Close(); err != nil {
		t.Fatal(err)
	}
	if err = p.Close(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, input) || !p.Stats().Closed || p.Stats().Entries != 0 || p.Stats().Bytes != 0 {
		t.Fatal("close invalidated emitted strings or retained pool")
	}
	if _, err = p.ShareRecord(nil, input); !errors.Is(err, ErrStringPoolClosed) {
		t.Fatal("closed admission")
	}
}

func TestStringPoolConcurrentBatchesAndClose(t *testing.T) {
	p, _ := NewStringPool(StringPoolOptions{MaxEntries: 16, MaxBytes: 256})
	start := make(chan struct{})
	ready := make(chan struct{}, 8)
	continueWork := make(chan struct{})
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			input := poolFixture("1.2.3.9")
			got, err := p.ShareRecord(nil, input)
			if err != nil || !reflect.DeepEqual(got, input) {
				t.Error("initial concurrent admission", err)
			}
			ready <- struct{}{}
			<-continueWork
			for i := 0; i < 100; i++ {
				input := poolFixture("1.2.3.9")
				got, err := p.ShareRecord(nil, input)
				if err != nil && !errors.Is(err, ErrStringPoolClosed) {
					t.Error(err)
					return
				}
				if err == nil && !reflect.DeepEqual(got, input) {
					t.Error("concurrent content")
				}
				_ = p.Stats()
			}
		}()
	}
	close(start)
	for i := 0; i < 8; i++ {
		<-ready
	}
	close(continueWork)
	_ = p.Close()
	wg.Wait()
	q, _ := NewStringPool(StringPoolOptions{})
	_, err := q.ShareRecord(nil, poolFixture("1.2.3.9"))
	if err != nil || q.Stats().Entries == 0 {
		t.Fatal("new batch lifetime", err)
	}
	_ = q.Close()
}
