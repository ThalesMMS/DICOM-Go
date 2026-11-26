package audit

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

type AssociationAuditOptions struct {
	Policy              ClinicalPolicy
	Common              Common
	Source, Destination Participant
	Requestor           *Participant
	// Emit must observe ctx and must not re-enter Finish. Success means accepted
	// by a caller-owned queue; transport delivery is a separate operation.
	Emit func(context.Context, Message) error
}
type InstanceTransfer struct {
	Time                        time.Time
	Outcome, Action             string
	Patient                     PatientIdentity
	Study                       StudyIdentity
	SOPClassUID, SOPInstanceUID string
	Encrypted, Anonymized       *bool
}
type aggregateKey struct{ patient, issuer, study, action, outcome, encrypted, anonymized string }
type aggregateGroup struct {
	input     InstanceTransfer
	instances map[string]string
	classes   map[string]int
}
type AssociationAuditStats struct {
	Recorded, Duplicates, Rejected, Emitted int
	Finished                                bool
}

// AssociationAudit owns a bounded, detached aggregate for one association.
// Record observes completed suboperations; Finish is explicitly called by the
// association owner on release, abort, error, timeout and cancellation. No
// best-effort telemetry queue is used as clinical authority.
type AssociationAudit struct {
	id                       string
	opts                     AssociationAuditOptions
	mu                       sync.Mutex
	gate                     chan struct{}
	groups                   map[aggregateKey]*aggregateGroup
	studyPatients            map[string]PatientIdentity
	identityBytes, instances int
	closing                  bool
	pending                  []Message
	next                     int
	stats                    AssociationAuditStats
}

func NewAssociationAudit(id string, opts AssociationAuditOptions) (*AssociationAudit, error) {
	p, err := opts.Policy.Normalize()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(id) == "" || len(id) > 256 {
		return nil, clinicalError("required", "Association.ID")
	}
	if opts.Emit == nil {
		return nil, clinicalError("required", "Emit")
	}
	opts.Policy = p
	if opts.Requestor != nil {
		v := *opts.Requestor
		opts.Requestor = &v
	}
	a := &AssociationAudit{id: strings.Clone(id), opts: opts, gate: make(chan struct{}, 1), groups: map[aggregateKey]*aggregateGroup{}, studyPatients: map[string]PatientIdentity{}}
	a.gate <- struct{}{}
	return a, nil
}
func flagKey(v *bool) string {
	if v == nil {
		return ""
	}
	if *v {
		return "true"
	}
	return "false"
}
func cloneFlag(v *bool) *bool {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}
func cloneStudy(s StudyIdentity) StudyIdentity {
	s.UID = strings.Clone(s.UID)
	s.Name = strings.Clone(s.Name)
	s.Query = append([]byte(nil), s.Query...)
	s.Accessions = append([]string(nil), s.Accessions...)
	s.SOPClasses = append([]SOPClass(nil), s.SOPClasses...)
	for i := range s.SOPClasses {
		s.SOPClasses[i].Instances = append([]StudyID(nil), s.SOPClasses[i].Instances...)
	}
	s.Encrypted = cloneFlag(s.Encrypted)
	s.Anonymized = cloneFlag(s.Anonymized)
	return s
}
func cloneInput(r InstanceTransfer) InstanceTransfer {
	r.Patient = PatientIdentity{ID: strings.Clone(r.Patient.ID), Name: strings.Clone(r.Patient.Name), Issuer: strings.Clone(r.Patient.Issuer)}
	r.Study = cloneStudy(r.Study)
	r.SOPClassUID = strings.Clone(r.SOPClassUID)
	r.SOPInstanceUID = strings.Clone(r.SOPInstanceUID)
	r.Encrypted = cloneFlag(r.Encrypted)
	r.Anonymized = cloneFlag(r.Anonymized)
	return r
}
func (a *AssociationAudit) Record(r InstanceTransfer) error {
	if a == nil {
		return clinicalError("required", "Association")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	reject := func(err error) error { a.stats.Rejected++; return err }
	if a.closing {
		return reject(clinicalError("finished", "Association"))
	}
	if !a.opts.Policy.IncludeIdentities {
		return reject(clinicalError("opt_in", "Identity"))
	}
	if !clinicalUID(r.SOPClassUID) || !clinicalUID(r.SOPInstanceUID) {
		return reject(clinicalError("uid", "Instance"))
	}
	if r.Action == "" {
		r.Action = ActionRead
	}
	if r.Outcome == "" {
		r.Outcome = OutcomeSuccess
	}
	if r.Time.IsZero() {
		r.Time = time.Now()
	}
	r.Time = r.Time.UTC()
	// Validate a one-instance envelope before retaining any identity. Do not
	// trust the caller's incoming SOP counts or instance lists.
	r.Study.SOPClasses = []SOPClass{{UID: r.SOPClassUID, NumberOfInstances: 1}}
	r.Study.Encrypted = cloneFlag(r.Encrypted)
	r.Study.Anonymized = cloneFlag(r.Anonymized)
	c := a.opts.Common
	c.Time = r.Time
	c.Outcome = r.Outcome
	if _, err := BuildInstancesTransferred(a.opts.Policy, ClinicalTransfer{Common: c, Source: a.opts.Source, Destination: a.opts.Destination, Requestor: a.opts.Requestor, Patient: r.Patient, Studies: []StudyIdentity{r.Study}}, r.Action); err != nil {
		return reject(err)
	}
	if previous, ok := a.studyPatients[r.Study.UID]; ok && previous != r.Patient {
		return reject(clinicalError("identity_conflict", "Study.Patient"))
	}
	key := aggregateKey{r.Patient.ID, r.Patient.Issuer, r.Study.UID, r.Action, r.Outcome, flagKey(r.Encrypted), flagKey(r.Anonymized)}
	g := a.groups[key]
	if g != nil {
		if g.input.Study.Name != r.Study.Name || string(g.input.Study.Query) != string(r.Study.Query) {
			return reject(clinicalError("identity_conflict", "Study"))
		}
		if previous, ok := g.instances[r.SOPInstanceUID]; ok {
			if previous != r.SOPClassUID {
				return reject(clinicalError("identity_conflict", "Instance"))
			}
			a.stats.Duplicates++
			if r.Time.After(g.input.Time) {
				g.input.Time = r.Time
			}
			return nil
		}
	}
	if a.instances >= a.opts.Policy.Limits.MaxInstances {
		return reject(clinicalError("limit", "Instances"))
	}
	added := len(r.SOPInstanceUID) + len(r.SOPClassUID)
	if g == nil {
		if len(a.groups) >= a.opts.Policy.Limits.MaxGroups {
			return reject(clinicalError("limit", "Groups"))
		}
		added += len(r.Patient.ID) + len(r.Patient.Name) + len(r.Patient.Issuer) + len(r.Study.UID) + len(r.Study.Name) + len(r.Study.Query)
		for _, v := range r.Study.Accessions {
			added += len(v)
		}
	} else if _, ok := g.classes[r.SOPClassUID]; !ok && len(g.classes) >= a.opts.Policy.Limits.MaxClasses {
		return reject(clinicalError("limit", "Classes"))
	}
	if added > a.opts.Policy.Limits.MaxIdentityBytes-a.identityBytes {
		return reject(clinicalError("limit", "Identity"))
	}
	if g == nil {
		r = cloneInput(r)
		g = &aggregateGroup{input: r, instances: map[string]string{}, classes: map[string]int{}}
		a.groups[key] = g
		a.studyPatients[r.Study.UID] = r.Patient
	}
	g.instances[strings.Clone(r.SOPInstanceUID)] = strings.Clone(r.SOPClassUID)
	g.classes[r.SOPClassUID]++
	if r.Time.After(g.input.Time) {
		g.input.Time = r.Time
	}
	a.identityBytes += added
	a.instances++
	a.stats.Recorded++
	return nil
}
func (a *AssociationAudit) Stats() AssociationAuditStats {
	if a == nil {
		return AssociationAuditStats{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stats
}
func invokeClinicalEmit(ctx context.Context, emit func(context.Context, Message) error, m Message) (err error) {
	defer func() {
		if recover() != nil {
			err = clinicalError("sink", "Emit")
		}
	}()
	if err = emit(ctx, cloneClinicalMessage(m)); err != nil {
		return clinicalError("sink", "Emit")
	}
	return nil
}

// Finish keeps unsent messages on failure; a retry resumes after the last
// successful Emit without replaying earlier groups. Concurrent calls serialize
// with a cancelable gate. Successful completion releases captured identities.
func (a *AssociationAudit) Finish(ctx context.Context) error {
	if a == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return clinicalError("canceled", "Finish")
	case <-a.gate:
	}
	defer func() { a.gate <- struct{}{} }()
	a.mu.Lock()
	if a.stats.Finished {
		a.mu.Unlock()
		return nil
	}
	a.closing = true
	if a.pending == nil {
		keys := make([]aggregateKey, 0, len(a.groups))
		for k := range a.groups {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			x, y := keys[i], keys[j]
			return strings.Join([]string{x.patient, x.issuer, x.study, x.action, x.outcome, x.encrypted, x.anonymized}, "\x00") < strings.Join([]string{y.patient, y.issuer, y.study, y.action, y.outcome, y.encrypted, y.anonymized}, "\x00")
		})
		messages := make([]Message, 0, len(keys))
		for _, k := range keys {
			g := a.groups[k]
			study := cloneStudy(g.input.Study)
			study.SOPClasses = nil
			classes := make([]string, 0, len(g.classes))
			for uid := range g.classes {
				classes = append(classes, uid)
			}
			sort.Strings(classes)
			for _, uid := range classes {
				study.SOPClasses = append(study.SOPClasses, SOPClass{UID: uid, NumberOfInstances: g.classes[uid]})
			}
			c := a.opts.Common
			c.Time = g.input.Time
			c.Outcome = g.input.Outcome
			m, err := BuildInstancesTransferred(a.opts.Policy, ClinicalTransfer{Common: c, Source: a.opts.Source, Destination: a.opts.Destination, Requestor: a.opts.Requestor, Patient: g.input.Patient, Studies: []StudyIdentity{study}}, g.input.Action)
			if err != nil {
				a.mu.Unlock()
				return err
			}
			messages = append(messages, m)
		}
		a.pending = messages
	}
	a.mu.Unlock()
	for {
		a.mu.Lock()
		if a.next == len(a.pending) {
			a.stats.Finished = true
			a.groups = nil
			a.studyPatients = nil
			a.pending = nil
			a.identityBytes = 0
			a.instances = 0
			a.mu.Unlock()
			return nil
		}
		m := a.pending[a.next]
		a.mu.Unlock()
		if ctx.Err() != nil {
			return clinicalError("canceled", "Finish")
		}
		if err := invokeClinicalEmit(ctx, a.opts.Emit, m); err != nil {
			return err
		}
		a.mu.Lock()
		a.next++
		a.stats.Emitted++
		a.mu.Unlock()
	}
}

// Query emits a separately qualified query; query bytes are never stored in
// the transfer aggregate or its stats. It has its own explicit PHI consent.
func (a *AssociationAudit) Query(ctx context.Context, q QueryRequest) error {
	if a == nil {
		return clinicalError("required", "Association")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return clinicalError("canceled", "Query")
	case <-a.gate:
	}
	defer func() { a.gate <- struct{}{} }()
	a.mu.Lock()
	closed := a.closing
	a.mu.Unlock()
	if closed {
		return clinicalError("finished", "Association")
	}
	q.Source = a.opts.Source
	q.Destination = a.opts.Destination
	q.Common.Source = a.opts.Common.Source
	m, err := BuildQuery(a.opts.Policy, q)
	if err != nil {
		return err
	}
	return invokeClinicalEmit(ctx, a.opts.Emit, m)
}
