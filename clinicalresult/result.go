// Package clinicalresult associates already-authorized diagnostic results
// with orders and tracks their producer-assigned revisions.
package clinicalresult

import (
	"errors"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

var (
	ErrInvalidInput     = errors.New("clinical result: invalid or out-of-profile input")
	ErrPatientScope     = errors.New("clinical result: subject outside authorized patient scope")
	ErrIdentityConflict = errors.New("clinical result: conflicting patient identity")
	ErrNoOrder          = errors.New("clinical result: no matching order in authorized scope")
	ErrAmbiguousOrder   = errors.New("clinical result: ambiguous order in authorized scope")
	ErrOrderChanged     = errors.New("clinical result: revision cannot change the associated order")
	ErrRevisionConflict = errors.New("clinical result: conflicting or invalid revision transition")
	ErrCapacity         = errors.New("clinical result: ledger capacity exceeded")
)

const (
	maxIdentifierBytes = 512
	maxOrderReferences = 16
	maxCandidateOrders = 10_000
	maxLedgerEntries   = 100_000
)

// Identifier pairs a value with its exact assigning namespace. Both components
// are required. Any canonicalization or crosswalk belongs to the caller.
type Identifier struct {
	System string
	Value  string
}

// Patient binds a resolved identifier. BirthDate (YYYY, YYYY-MM or YYYY-MM-DD)
// and Sex (male, female or other) are optional conflict checks.
type Patient struct {
	ID        Identifier
	BirthDate string
	Sex       string
}

// ReferenceKind preserves the role of each order number.
type ReferenceKind string

const (
	RefPlacer         ReferenceKind = "placer"
	RefFiller         ReferenceKind = "filler"
	RefAccession      ReferenceKind = "accession"
	RefServiceRequest ReferenceKind = "service-request"
)

type OrderReference struct {
	Kind ReferenceKind
	ID   Identifier
}

type Order struct {
	ID         Identifier
	Patient    Patient
	References []OrderReference
}

type Status string

const (
	StatusFinal     Status = "final"
	StatusCorrected Status = "corrected"
	StatusWithdrawn Status = "withdrawn"
)

// Result is a complete revision, scoped to a trusted producer and logical
// diagnostic result. Sequence is a positive producer-supplied ordering, and
// RevisionID is an opaque label from that producer. Digest binds the payload.
type Result struct {
	Source          string
	ID              Identifier
	Patient         Patient
	OrderReferences []OrderReference
	Sequence        uint64
	RevisionID      string
	Status          Status
	Digest          [32]byte
}

type Revision struct {
	Sequence uint64
	ID       string
	Status   Status
	Digest   [32]byte
}

// Current records the latest admitted revision, including withdrawals.
type Current struct {
	OrderID  Identifier
	Revision Revision
}

type Disposition string

const (
	Created  Disposition = "created"
	Replaced Disposition = "replaced"
	Replay   Disposition = "replay"
	Stale    Disposition = "stale"
)

// Decision describes an in-memory state transition. Previous is populated
// when the admitted revision replaced an earlier one.
type Decision struct {
	Disposition Disposition
	Current     Current
	Previous    Revision
}

type resultKey struct {
	Patient Identifier
	Source  string
	Result  Identifier
}

type entry struct {
	patient Patient
	current Current
}

// Ledger is a bounded in-memory index of latest revision metadata. A caller
// must authenticate its producer and resolve the authorized patient before
// passing a grant to Apply or Lookup. Supply the current set of eligible orders
// to Apply. Applications own payload storage and durable publication.
type Ledger struct {
	mu      sync.RWMutex
	limit   int
	current map[resultKey]entry
}

// NewLedger creates an empty Ledger. maxEntries must be in [1, 100000].
func NewLedger(maxEntries int) (*Ledger, error) {
	if maxEntries < 1 || maxEntries > maxLedgerEntries {
		return nil, ErrInvalidInput
	}
	return &Ledger{limit: maxEntries, current: make(map[resultKey]entry)}, nil
}

// Apply associates every supplied order reference with one eligible order for
// the authorized patient, then atomically updates that result's lineage.
// Authorization and order checks run for corrections, replays and stale input.
func (l *Ledger) Apply(authorized Patient, result Result, orders []Order) (Decision, error) {
	if l == nil || l.limit == 0 || !patientValid(authorized) || !resultValid(result) || len(orders) > maxCandidateOrders {
		return Decision{}, ErrInvalidInput
	}
	if authorized.ID != result.Patient.ID {
		return Decision{}, ErrPatientScope
	}
	patient, err := combinePatient(authorized, result.Patient)
	if err != nil {
		return Decision{}, err
	}
	candidate, patient, err := selectOrder(patient, result.OrderReferences, orders)
	if err != nil {
		return Decision{}, err
	}
	current := Current{
		OrderID: candidate,
		Revision: Revision{
			Sequence: result.Sequence, ID: result.RevisionID, Status: result.Status, Digest: result.Digest,
		},
	}
	return l.publish(resultKey{patient.ID, result.Source, result.ID}, patient, current)
}

func (l *Ledger) publish(key resultKey, patient Patient, incoming Current) (Decision, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	prior, exists := l.current[key]
	if !exists {
		if len(l.current) == l.limit {
			return Decision{}, ErrCapacity
		}
		l.current[key] = entry{patient: patient, current: incoming}
		return Decision{Disposition: Created, Current: incoming}, nil
	}
	patient, err := combinePatient(patient, prior.patient)
	if err != nil {
		return Decision{}, err
	}
	if prior.current.OrderID != incoming.OrderID {
		return Decision{}, ErrOrderChanged
	}
	before, after := prior.current.Revision, incoming.Revision
	switch {
	case after.Sequence < before.Sequence:
		return Decision{Disposition: Stale, Current: prior.current}, nil
	case after == before:
		return Decision{Disposition: Replay, Current: prior.current}, nil
	case after.Sequence == before.Sequence, after.ID == before.ID, after.Status == StatusFinal:
		return Decision{}, ErrRevisionConflict
	case before.Status == StatusWithdrawn && after.Status != StatusWithdrawn:
		return Decision{}, ErrRevisionConflict
	default:
		l.current[key] = entry{patient: patient, current: incoming}
		return Decision{Disposition: Replaced, Current: incoming, Previous: before}, nil
	}
}

// Lookup returns zero, false for an absent lineage. It only looks within the
// exact authorized patient, producer and result identifier.
func (l *Ledger) Lookup(authorized Patient, source string, result Identifier) (Current, bool, error) {
	if l == nil || l.limit == 0 || !patientValid(authorized) || !identifierPartValid(source) || !identifierValid(result) {
		return Current{}, false, ErrInvalidInput
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	prior, exists := l.current[resultKey{authorized.ID, source, result}]
	if !exists {
		return Current{}, false, nil
	}
	if _, err := combinePatient(authorized, prior.patient); err != nil {
		return Current{}, false, err
	}
	return prior.current, true, nil
}

func selectOrder(patient Patient, required []OrderReference, candidates []Order) (Identifier, Patient, error) {
	var selected Identifier
	ambiguous := false
	var partialPatient = patient
	for _, order := range candidates {
		if order.Patient.ID != patient.ID {
			continue
		}
		if len(order.References) > maxOrderReferences {
			return Identifier{}, Patient{}, ErrInvalidInput
		}
		matches := 0
		for _, reference := range required {
			if slices.Contains(order.References, reference) {
				matches++
			}
		}
		if matches == 0 {
			continue
		}
		if !orderValid(order) {
			return Identifier{}, Patient{}, ErrInvalidInput
		}
		var err error
		partialPatient, err = combinePatient(partialPatient, order.Patient)
		if err != nil {
			return Identifier{}, Patient{}, err
		}
		if matches == len(required) {
			ambiguous = ambiguous || (selected != (Identifier{}) && selected != order.ID)
			selected = order.ID
		}
	}
	if ambiguous {
		return Identifier{}, Patient{}, ErrAmbiguousOrder
	}
	if selected == (Identifier{}) {
		return Identifier{}, Patient{}, ErrNoOrder
	}
	return selected, partialPatient, nil
}

func combinePatient(a, b Patient) (Patient, error) {
	aDate, bDate := a.BirthDate, b.BirthDate
	shared := min(len(aDate), len(bDate))
	if a.ID != b.ID || aDate[:shared] != bDate[:shared] ||
		(a.Sex != "" && b.Sex != "" && a.Sex != b.Sex) {
		return Patient{}, ErrIdentityConflict
	}
	if len(bDate) > len(aDate) {
		a.BirthDate = bDate
	}
	if a.Sex == "" {
		a.Sex = b.Sex
	}
	return a, nil
}

func identifierValid(id Identifier) bool {
	return identifierPartValid(id.System) && identifierPartValid(id.Value)
}

func identifierPartValid(value string) bool {
	return value != "" && len(value) <= maxIdentifierBytes && utf8.ValidString(value) &&
		value == strings.TrimSpace(value) && strings.IndexFunc(value, unicode.IsControl) < 0
}

func patientValid(patient Patient) bool {
	if !identifierValid(patient.ID) {
		return false
	}
	switch patient.Sex {
	case "", "male", "female", "other":
	default:
		return false
	}
	date := patient.BirthDate
	if date == "" {
		return true
	}
	var layout string
	switch len(date) {
	case 4:
		layout = "2006"
	case 7:
		layout = "2006-01"
	case 10:
		layout = "2006-01-02"
	default:
		return false
	}
	parsed, err := time.Parse(layout, date)
	return err == nil && parsed.Year() > 0 && parsed.Format(layout) == date
}

func referencesValid(references []OrderReference) bool {
	if len(references) == 0 || len(references) > maxOrderReferences {
		return false
	}
	for _, ref := range references {
		if !identifierValid(ref.ID) {
			return false
		}
		switch ref.Kind {
		case RefPlacer, RefFiller, RefAccession, RefServiceRequest:
		default:
			return false
		}
	}
	return true
}

func orderValid(order Order) bool {
	return identifierValid(order.ID) && patientValid(order.Patient) && referencesValid(order.References)
}

func resultValid(result Result) bool {
	switch result.Status {
	case StatusFinal, StatusCorrected, StatusWithdrawn:
	default:
		return false
	}
	return identifierPartValid(result.Source) && identifierValid(result.ID) && patientValid(result.Patient) &&
		referencesValid(result.OrderReferences) && result.Sequence > 0 && identifierPartValid(result.RevisionID) &&
		result.Digest != ([32]byte{})
}
