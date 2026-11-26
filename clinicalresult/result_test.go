package clinicalresult

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestAssociateRequiresAuthorizedPatientAndExactOrderNamespace(t *testing.T) {
	patient := testPatient("authority-a", "patient")
	for _, kind := range []ReferenceKind{RefAccession, RefPlacer, RefFiller, RefServiceRequest} {
		t.Run(string(kind), func(t *testing.T) {
			ledger := testLedger(t, 16)
			ref := OrderReference{Kind: kind, ID: Identifier{"orders-a", "reused"}}
			ourOrder := testOrder("own-order", patient, ref)
			result := testResult(patient, ref, 1, StatusFinal)
			for _, testCase := range []struct {
				name   string
				grant  Patient
				result Result
				orders []Order
				want   error
			}{
				{"other patient", patient, changePatient(result, testPatient("authority-a", "other-patient")), []Order{ourOrder}, ErrPatientScope},
				{"different patient issuer", patient, changePatient(result, testPatient("authority-b", "patient")), []Order{ourOrder}, ErrPatientScope},
				{"unknown patient", patient, changePatient(result, Patient{}), []Order{ourOrder}, ErrInvalidInput},
				{"other patient order", patient, result, []Order{testOrder("other-order", testPatient("authority-a", "other"), ref)}, ErrNoOrder},
				{"different order issuer", patient, result, []Order{testOrder("other-order", patient, OrderReference{kind, Identifier{"orders-b", "reused"}})}, ErrNoOrder},
				{"order issuer absent", patient, withOrderReferences(result, OrderReference{kind, Identifier{"", "reused"}}), []Order{ourOrder}, ErrInvalidInput},
				{"issuer case changed", patient, result, []Order{testOrder("other-order", patient, OrderReference{kind, Identifier{"Orders-a", "reused"}})}, ErrNoOrder},
				{"role changed", patient, result, []Order{testOrder("other-order", patient, OrderReference{"different-role", ref.ID})}, ErrNoOrder},
			} {
				t.Run(testCase.name, func(t *testing.T) {
					got, err := ledger.Apply(testCase.grant, testCase.result, testCase.orders)
					if !errors.Is(err, testCase.want) || got != (Decision{}) {
						t.Fatalf("decision = %+v, error = %v, want only %v", got, err, testCase.want)
					}
				})
			}
			foreign := testOrder("foreign-order", testPatient("authority-b", "patient"), ref)
			got, err := ledger.Apply(patient, result, []Order{foreign, ourOrder})
			if err != nil || got.Disposition != Created || got.Current.OrderID != ourOrder.ID {
				t.Fatalf("exact match: %+v, %v", got, err)
			}
		})
	}
}

func TestAssociateRequiresAllReferencesAndRefusesAmbiguousOrders(t *testing.T) {
	patient := testPatient("authority", "patient")
	placer := OrderReference{RefPlacer, Identifier{"ris", "1234"}}
	filler := OrderReference{RefFiller, Identifier{"pacs", "1234"}}
	first := testOrder("first", patient, placer)
	second := testOrder("second", patient, filler)
	combined := testOrder("both", patient, placer, filler)
	result := withOrderReferences(testResult(patient, placer, 1, StatusFinal), placer, filler)
	ledger := testLedger(t, 2)
	if _, err := ledger.Apply(patient, result, []Order{first, second}); !errors.Is(err, ErrNoOrder) {
		t.Fatalf("split references = %v", err)
	}
	duplicate := testOrder("ambiguous", patient, filler, placer)
	if _, err := ledger.Apply(patient, result, []Order{combined, duplicate}); !errors.Is(err, ErrAmbiguousOrder) {
		t.Fatalf("ambiguous references = %v", err)
	}
	got, err := ledger.Apply(patient, result, []Order{first, second, combined, combined})
	if err != nil || got.Current.OrderID != combined.ID {
		t.Fatalf("unique complete match = %+v, %v", got, err)
	}
}

func TestAssociateNeverUsesContradictoryDemographics(t *testing.T) {
	patient := testPatient("authority", "patient")
	patient.BirthDate, patient.Sex = "1985-02", "female"
	ref := OrderReference{RefPlacer, Identifier{"ris", "1234"}}
	var partial, conflicting, matching = patient, patient, patient
	partial.BirthDate = "1985"
	conflicting.BirthDate = "1985-02-18"
	matching.BirthDate = "1985-02-19"
	ledger := testLedger(t, 2)
	result := testResult(partial, ref, 1, StatusFinal)
	errOrder := testOrder("wrong-birth", conflicting, ref)
	validOrder := testOrder("right-birth", matching, ref)
	for _, candidates := range [][]Order{{errOrder, validOrder}, {validOrder, errOrder}} {
		if _, err := ledger.Apply(patient, result, candidates); !errors.Is(err, ErrIdentityConflict) {
			t.Fatalf("conflicting orders = %v", err)
		}
	}
	if _, err := ledger.Apply(patient, result, []Order{validOrder}); err != nil {
		t.Fatalf("partial compatible demographics: %v", err)
	}
	result.Status, result.Sequence, result.RevisionID = StatusCorrected, 2, "correction"
	result.Patient = conflicting
	if _, err := ledger.Apply(patient, result, []Order{testOrder("right-birth", partial, ref)}); !errors.Is(err, ErrIdentityConflict) {
		t.Fatalf("stored evidence forgotten by correction: %v", err)
	}
	if got, ok, err := ledger.Lookup(conflicting, result.Source, result.ID); ok || !errors.Is(err, ErrIdentityConflict) || got != (Current{}) {
		t.Fatalf("lookup with contradictory birth date returned %+v, %t, %v", got, ok, err)
	}
}

func TestResultCorrectionDoesNotReachAnotherPatientOrSource(t *testing.T) {
	first := testPatient("identifiers", "one")
	second := testPatient("identifiers", "two")
	ref := OrderReference{RefFiller, Identifier{"pacs", "same-order-number"}}
	ours := testOrder("order-1", first, ref)
	other := testOrder("order-2", second, ref)
	orders := []Order{ours, other}
	ledger := testLedger(t, 3)
	base := testResult(first, ref, 9, StatusFinal)
	foreign := testResult(second, ref, 99, StatusFinal)
	differentSource := testResult(first, ref, 77, StatusFinal)
	differentSource.Source = "different-sender"
	for _, original := range []Result{base, foreign, differentSource} {
		if _, err := ledger.Apply(original.Patient, original, orders); err != nil {
			t.Fatal(err)
		}
	}
	correction := testResult(first, ref, 11, StatusCorrected)
	correction.RevisionID = "opaque-AAA" // Ordering has no relation to this string.
	got, err := ledger.Apply(first, correction, orders)
	if err != nil || got.Disposition != Replaced || got.Previous.Sequence != 9 || got.Current.OrderID != ours.ID {
		t.Fatalf("correction = %+v, %v", got, err)
	}
	for _, foreign := range []Result{foreign, differentSource} {
		got, ok, err := ledger.Lookup(foreign.Patient, foreign.Source, foreign.ID)
		if err != nil || !ok || got.Revision.Sequence != foreign.Sequence {
			t.Fatalf("neighboring lineage = %+v, %t, %v", got, ok, err)
		}
	}
	late := testResult(first, ref, 10, StatusCorrected)
	got, err = ledger.Apply(first, late, orders)
	if err != nil || got.Disposition != Stale || got.Current.Revision.ID != correction.RevisionID {
		t.Fatalf("late result = %+v, %v", got, err)
	}
	got, err = ledger.Apply(first, correction, orders)
	if err != nil || got.Disposition != Replay || got.Previous != (Revision{}) {
		t.Fatalf("replay = %+v, %v", got, err)
	}
	if _, err := ledger.Apply(second, correction, orders); !errors.Is(err, ErrPatientScope) {
		t.Fatalf("unauthorized correction replay = %v", err)
	}
	correction.Digest = sha256.Sum256([]byte("changed"))
	if _, err := ledger.Apply(first, correction, orders); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("conflicting replay = %v", err)
	}
}

func TestResultOrderAndWithdrawalAreStable(t *testing.T) {
	patient := testPatient("hospital", "exact")
	ref := OrderReference{RefAccession, Identifier{"hospital-acc", "acc"}}
	orders := []Order{testOrder("current", patient, ref)}
	ledger := testLedger(t, 1)
	base := testResult(patient, ref, 1, StatusFinal)
	if _, err := ledger.Apply(patient, base, orders); err != nil {
		t.Fatal(err)
	}
	update := testResult(patient, ref, 2, StatusCorrected)
	if _, err := ledger.Apply(patient, update, []Order{testOrder("unrelated-order", patient, ref)}); !errors.Is(err, ErrOrderChanged) {
		t.Fatalf("changed order = %v", err)
	}
	update.Status = StatusFinal
	if _, err := ledger.Apply(patient, update, orders); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("undeclared correction = %v", err)
	}
	update.Status = StatusWithdrawn
	if got, err := ledger.Apply(patient, update, orders); err != nil || got.Current.Revision.Status != StatusWithdrawn {
		t.Fatalf("withdrawal = %+v, %v", got, err)
	}
	if _, err := ledger.Apply(patient, testResult(patient, ref, 3, StatusCorrected), orders); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("implicit resurrection = %v", err)
	}
	other := base
	other.ID.Value = "capacity"
	if _, err := ledger.Apply(patient, other, orders); !errors.Is(err, ErrCapacity) {
		t.Fatalf("capacity refusal = %v", err)
	}
	if got, ok, err := ledger.Lookup(patient, base.Source, base.ID); err != nil || !ok || got.Revision.Status != StatusWithdrawn {
		t.Fatalf("capacity evicted tombstone: %+v, %t, %v", got, ok, err)
	}
}

func TestResultConcurrentCorrectionsKeepHighestSequence(t *testing.T) {
	patient := testPatient("hospital", "exact")
	ref := OrderReference{RefPlacer, Identifier{"hospital-ris", "ord"}}
	orders := []Order{testOrder("canonical", patient, ref)}
	ledger := testLedger(t, 1)
	var wg sync.WaitGroup
	base := testResult(patient, ref, 1, StatusFinal)
	if _, err := ledger.Apply(patient, base, orders); err != nil {
		t.Fatal(err)
	}
	for i := uint64(2); i <= 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for repeat := 0; repeat < 2; repeat++ {
				if _, err := ledger.Apply(patient, testResult(patient, ref, i, StatusCorrected), orders); err != nil {
					t.Errorf("sequence %d: %v", i, err)
				}
			}
		}()
	}
	wg.Wait()
	got, ok, err := ledger.Lookup(patient, base.Source, base.ID)
	if err != nil || !ok || got.Revision.Sequence != 100 {
		t.Fatalf("latest = %+v, %t, %v, want sequence 100", got, ok, err)
	}
}

func TestResultBoundsAndMalformedValues(t *testing.T) {
	patient := testPatient("hospital", "exact")
	ref := OrderReference{RefPlacer, Identifier{"hospital-ris", "ord"}}
	orders := []Order{testOrder("canonical", patient, ref)}
	ledger := testLedger(t, 1)
	for _, testCase := range []struct {
		name   string
		mutate func(*Result)
	}{
		{"revision ID control", func(result *Result) { result.RevisionID = "private\ntext" }},
		{"zero sequence", func(result *Result) { result.Sequence = 0 }},
		{"empty digest", func(result *Result) { result.Digest = [32]byte{} }},
		{"date", func(result *Result) { result.Patient.BirthDate = "2023-02-29" }},
		{"result ID length", func(result *Result) { result.ID.Value = strings.Repeat("a", maxIdentifierBytes+1) }},
		{"references", func(result *Result) { result.OrderReferences = make([]OrderReference, maxOrderReferences+1) }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			input := testResult(patient, ref, 1, StatusFinal)
			testCase.mutate(&input)
			got, err := ledger.Apply(patient, input, orders)
			if got != (Decision{}) || err != ErrInvalidInput {
				t.Fatalf("invalid input = %+v, %v", got, err)
			}
		})
	}
	if _, err := ledger.Apply(patient, testResult(patient, ref, 1, StatusFinal), make([]Order, maxCandidateOrders+1)); err != ErrInvalidInput {
		t.Fatalf("candidate bounds = %v", err)
	}
}

func testLedger(t *testing.T, limit int) *Ledger {
	t.Helper()
	ledger, err := NewLedger(limit)
	if err != nil {
		t.Fatal(err)
	}
	return ledger
}

func testPatient(system, value string) Patient {
	return Patient{ID: Identifier{system, value}}
}

func testOrder(orderID string, patient Patient, refs ...OrderReference) Order {
	return Order{ID: Identifier{"order-ledger", orderID}, Patient: patient, References: refs}
}

func testResult(patient Patient, ref OrderReference, sequence uint64, status Status) Result {
	id := fmt.Sprintf("v-%d", sequence)
	return Result{
		Source: "provider", ID: Identifier{"reports", "report"}, Patient: patient, OrderReferences: []OrderReference{ref},
		Sequence: sequence, RevisionID: id, Status: status, Digest: sha256.Sum256([]byte(id)),
	}
}

func changePatient(result Result, patient Patient) Result {
	result.Patient = patient
	return result
}

func withOrderReferences(result Result, refs ...OrderReference) Result {
	result.OrderReferences = refs
	return result
}
