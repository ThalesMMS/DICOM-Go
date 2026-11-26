package rtplan

import (
	"context"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
)

// TreatmentRecord describes declared delivery data for the four first-generation
// RT record IODs. It does not verify treatment, compute dose, or resolve external
// references. USER-origin salvage records are outside the admitted profile.
type TreatmentRecord struct {
	SOPClassUID, SOPInstanceUID, StudyInstanceUID, SeriesInstanceUID string
	InstanceNumber                                                   int
	Date, Time, TimezoneOffset, ContentOrigin                        Attribute[string]
	ReferencedPlan                                                   *Reference
	RelatedRecords                                                   []Reference
	Machine                                                          *RecordMachine
	FractionGroupNumber, FractionsPlanned                            Attribute[int]
	PrimaryDosimeterUnit                                             string
	PatientSetups                                                    []PatientSetup
	MeasuredDoseReferences, CalculatedDoseReferences                 []RecordDoseReference
	BeamSessions                                                     []RecordBeamSession
	BrachyTechnique, BrachyType                                      string
	BrachySources                                                    []RecordedBrachySource
	BrachySessions                                                   []RecordBrachySession
	Summary                                                          *RecordSummary
	Warnings                                                         []Problem
}

// RecordMachine contains declared equipment identification. Type 2 empties
// remain distinguishable from absent values.
type RecordMachine struct {
	Name, Manufacturer, Institution, Model, SerialNumber Attribute[string]
	TableTopPositionAlignmentUID                         string
}

// RecordDoseReference has exactly one local number or external-plan number.
// Values are stored quantities; Measured Units are GY or RELATIVE, while
// calculated quantities are Gy. Number is local to this TreatmentRecord.
type RecordDoseReference struct {
	Number, PlanNumber Attribute[int]
	Units              string
	Value              Attribute[float64]
	Type               Attribute[string]
	Description        string
}

// RecordDoseValue references a local measured/calculated point or a plan point.
type RecordDoseValue struct {
	PlanNumber, LocalNumber Attribute[int]
	Value                   float64
}

type RecordBeamSession struct {
	ReferencedBeamNumber, ReferencedPatientSetupNumber, ReferencedToleranceTableNumber Attribute[int]
	Name, Description, Type, RadiationType                                             string
	FractionNumber, ClinicalFractionNumber                                             Attribute[int]
	DeliveryType, VerificationStatus                                                   Attribute[string]
	TerminationStatus                                                                  string
	SpecifiedPrimaryMeterset, SpecifiedSecondaryMeterset                               Attribute[float64]
	DeliveredPrimaryMeterset, DeliveredSecondaryMeterset                               Attribute[float64]
	SpecifiedTreatmentTime, DeliveredTreatmentTime, SourceAxisDistance                 Attribute[float64]
	Devices                                                                            []BeamLimitingDevice
	MeasuredDoses, CalculatedDoses                                                     []RecordDoseValue
	VerificationImages                                                                 []Reference
	AtomicNumber, MassNumber, ChargeState                                              Attribute[int]
	ScanMode, SnoutID, PatientSupportType                                              string
	RangeShifters                                                                      []RecordedRangeShifter
	ControlPoints                                                                      []RecordControlPoint
}

type RecordedRangeShifter struct {
	Number int
	ID     string
}

// RecordMachineState consists of absolute IEC machine coordinates. It does
// not imply a patient isocentre or a Frame of Reference. Later items inherit
// only states that the corresponding RT session module permits to be sparse.
type RecordMachineState struct {
	Energy, GantryAngle, BeamLimitingDeviceAngle, PatientSupportAngle                               Attribute[float64]
	TableTopEccentricAngle, TableTopEccentricAxisDistance                                           Attribute[float64]
	TableTopPitchAngle, TableTopRollAngle, GantryPitchAngle                                         Attribute[float64]
	TableTopVerticalPosition, TableTopLongitudinalPosition, TableTopLateralPosition                 Attribute[float64]
	SnoutPosition                                                                                   Attribute[float64]
	GantryDirection, BeamLimitingDeviceDirection, PatientSupportDirection                           Attribute[string]
	TableTopEccentricDirection, TableTopPitchDirection, TableTopRollDirection, GantryPitchDirection Attribute[string]
	DevicePositions                                                                                 []DevicePosition
	RangeShifterSettings                                                                            []RangeShifterSetting
}

// RecordControlPoint is an item in delivered order. ReferencedIndex refers to
// a plan CP; it is not a local ordinal and is optional in conventional records.
// FL spot maps/metersets retain binary32 values exactly. Spot indices are
// one-based references to prescribed spots, and may repeat for repainting.
type RecordControlPoint struct {
	ReferencedIndex              Attribute[int]
	Date, Time                   string
	SpecifiedMeterset            Attribute[float64]
	DeliveredMeterset            float64
	RateSet, RateDelivered       Attribute[float64]
	EnergyUnit                   string
	State                        RecordMachineState
	Encoded                      []core.Tag
	ScanSpotTuneID               string
	SpotPositions, SpotMetersets []float32
	NumberOfPaintings            Attribute[int]
	SpotReordered                Attribute[string]
	SpotPrescribedIndices        []int
}

// RecordedBrachySource adds recorded equipment fields to the source primitive.
type RecordedBrachySource struct {
	BrachySource
	Manufacturer, RecordedSerialNumber Attribute[string]
	ModelID                            string
}

type RecordBrachySession struct {
	ReferencedSetupNumber            Attribute[int]
	Type, Name, Check                string
	FractionNumber                   Attribute[int]
	DeliveryType, VerificationStatus Attribute[string]
	TerminationStatus                string
	TotalReferenceAirKerma           float64
	VerificationImages               []Reference
	MeasuredDoses, CalculatedDoses   []RecordDoseValue
	Channels                         []RecordBrachyChannel
}

type RecordSafePosition struct{ ExitDate, ExitTime, ReturnDate, ReturnTime string }
type RecordBrachyChannel struct {
	Number, SourceNumber                           int
	ReferencedChannelNumber                        Attribute[int]
	Movement                                       string
	Length, EffectiveLength, InnerLength           Attribute[float64]
	AfterloaderChannelID                           string
	SpecifiedTime, DeliveredTime                   float64 // seconds, summed over pulses for PDR
	SpecifiedPulses, DeliveredPulses               Attribute[int]
	SpecifiedPulseInterval, DeliveredPulseInterval Attribute[float64]
	TransferTubeNumber                             Attribute[int]
	TransferTubeLength                             Attribute[float64]
	SafePosition                                   RecordSafePosition
	Applicators                                    []RecordedSourceApplicator
	MeasuredDoses, CalculatedDoses                 []RecordDoseValue
	ControlPoints                                  []RecordBrachyControlPoint
	Pulses                                         []RecordBrachyPulse
}

type RecordedSourceApplicator struct {
	Number              Attribute[int]
	ID                  Attribute[string]
	Type, Name          string
	Length              float64
	TipLength, StepSize Attribute[float64]
}

type RecordBrachyControlPoint struct {
	ReferencedIndex  Attribute[int]
	Date, Time       string
	RelativePosition float64 // mm from distal-most possible source position
}

type RecordBrachyPulse struct {
	Number        int
	SafePosition  RecordSafePosition
	ControlPoints []RecordBrachyControlPoint
}

type RecordSummary struct {
	Status, Comment                string
	FirstDate, MostRecentDate      Attribute[string]
	FractionGroups                 []RecordFractionGroupSummary
	MeasuredDoses, CalculatedDoses []RecordSummaryDose
}

type RecordFractionGroupSummary struct {
	ReferencedNumber   Attribute[int]
	Type               Attribute[string]
	Planned, Delivered Attribute[int]
	Fractions          []RecordFractionStatus
}

type RecordFractionStatus struct {
	Number                        int
	ClinicalNumber                Attribute[int]
	Date, Time, TerminationStatus Attribute[string]
}

type RecordSummaryDose struct {
	PlanNumber     Attribute[int]
	Description    string
	CumulativeDose float64 // Gy; declared value only
}

// ReadTreatmentRecord reads a record independently of the Plan API. Inputs
// must have materialized metadata; outputs own their scalar and slice fields.
func ReadTreatmentRecord(ctx context.Context, f *object.File, opts ReadOptions) (*TreatmentRecord, error) {
	if f == nil {
		return nil, pathError(ErrInvalidObject, "", "file is nil")
	}
	opts, err := fileReadOptions(ctx, f, opts)
	if err != nil {
		return nil, err
	}
	return ReadTreatmentRecordDataset(ctx, f.Dataset, opts)
}

// ReadTreatmentRecordDataset admits materialized metadata before decoding.
// It returns an owned model, or nil on cancellation, limits or invalid content.
func ReadTreatmentRecordDataset(ctx context.Context, obj *object.Object, opts ReadOptions) (*TreatmentRecord, error) {
	b, err := preflight(ctx, obj, opts.Limits, false)
	if err != nil {
		return nil, err
	}
	obj, err = freshFacade(ctx, obj, false)
	if err != nil {
		return nil, err
	}
	sop, _ := obj.GetUID(tagSOPClassUID)
	if !oneOf(sop, RTBeamsTreatmentRecordStorage, RTIonBeamsTreatmentRecordStorage, RTBrachyTreatmentRecordStorage, RTTreatmentSummaryRecordStorage) {
		return nil, ErrUnsupportedSOPClass
	}
	if err := rsProfile(ctx, obj, sop, ""); err != nil {
		return nil, err
	}
	if opts.StrictIOD {
		if err := strictObject(ctx, obj, b.limits); err != nil {
			return nil, err
		}
	}
	r := &reader{ctx: ctx, maxProblems: b.limits.MaxProblems}
	rec := &TreatmentRecord{SOPClassUID: sop, SOPInstanceUID: r.uid(obj, tagSOPInstanceUID), StudyInstanceUID: r.uid(obj, tagStudyInstanceUID), SeriesInstanceUID: r.uid(obj, tagSeriesInstanceUID), InstanceNumber: r.requiredInt(obj, ft(0x00200013), "", "Instance Number"), Date: rsText(r, obj, ft(0x30080250)), Time: rsText(r, obj, ft(0x30080251)), TimezoneOffset: rsText(r, obj, ft(0x00080201)), ContentOrigin: rsText(r, obj, ft(0x300a0709)), FractionGroupNumber: intAttribute(r, obj, ft(0x300c0022), ""), FractionsPlanned: intAttribute(r, obj, tagNumberOfFractionsPlanned, ""), PrimaryDosimeterUnit: r.code(obj, tagPrimaryDosimeterUnit), BrachyTechnique: r.code(obj, tagBrachyTreatmentTechnique), BrachyType: r.code(obj, tagBrachyTreatmentType)}
	if r.code(obj, tagModality) != "RTRECORD" {
		r.fail(tagModality.String(), "Modality is not RTRECORD")
	}
	for _, v := range []struct {
		t core.Tag
		s string
	}{{tagSOPInstanceUID, rec.SOPInstanceUID}, {tagStudyInstanceUID, rec.StudyInstanceUID}, {tagSeriesInstanceUID, rec.SeriesInstanceUID}} {
		if !rsUID(v.s) {
			r.fail(v.t.String(), "UID is missing or invalid")
		}
	}
	rsRequiredPresence(r, obj, ft(0x30080250), "")
	rsRequiredPresence(r, obj, ft(0x30080251), "")
	rsRequiredPresence(r, obj, tagReferencedRTPlanSequence, "")
	refs := rsReferences(r, obj, tagReferencedRTPlanSequence, "")
	if len(refs) > 1 {
		r.fail(tagReferencedRTPlanSequence.String(), "plan reference item count")
	}
	if len(refs) == 1 {
		rec.ReferencedPlan = &refs[0]
		if !oneOf(refs[0].SOPClassUID, RTPlanStorage, RTIonPlanStorage) {
			r.fail(tagReferencedRTPlanSequence.String(), "referenced SOP is not an admitted plan")
		}
	}
	rec.RelatedRecords = rsReferences(r, obj, ft(0x30080030), "")
	rsCheckDateTime(r, rec.Date, rec.Time, ft(0x30080250), ft(0x30080251), "")
	if rec.TimezoneOffset.Value != nil && !rsTimezone(*rec.TimezoneOffset.Value) {
		r.fail(ft(0x00080201).String(), "timezone offset is invalid")
	}
	machines := r.sequence(obj, ft(0x300a0206))
	if sop != RTTreatmentSummaryRecordStorage && len(machines) != 1 {
		r.fail(ft(0x300a0206).String(), "one treatment machine is required")
	}
	if len(machines) > 1 {
		r.fail(ft(0x300a0206).String(), "machine item count")
	}
	if len(machines) == 1 {
		m := machines[0]
		p := item("", ft(0x300a0206), 0)
		rec.Machine = &RecordMachine{Name: rsText(r, m, tagTreatmentMachineName), Manufacturer: rsText(r, m, ft(0x00080070)), Institution: rsText(r, m, ft(0x00080080)), Model: rsText(r, m, ft(0x00081090)), SerialNumber: rsText(r, m, ft(0x00181000)), TableTopPositionAlignmentUID: r.uid(m, ft(0x300a0054))}
		for _, t := range []core.Tag{tagTreatmentMachineName, ft(0x00080070), ft(0x00080080), ft(0x00081090), ft(0x00181000)} {
			rsRequiredPresence(r, m, t, p)
		}
		if rec.Machine.TableTopPositionAlignmentUID != "" && !rsUID(rec.Machine.TableTopPositionAlignmentUID) {
			r.fail(join(p, ft(0x300a0054)), "alignment UID is invalid")
		}
	}
	for i, o := range r.sequence(obj, tagPatientSetupSequence) {
		p := item("", tagPatientSetupSequence, i)
		s := PatientSetup{Number: r.requiredInt(o, tagPatientSetupNumber, p, "Patient Setup Number"), Label: r.str(o, tagPatientSetupLabel), PatientPosition: r.code(o, tagPatientPosition), PatientAdditionalPosition: r.str(o, tagPatientAdditionalPosition), SetupTechnique: r.code(o, tagSetupTechnique)}
		for _, old := range rec.PatientSetups {
			if old.Number == s.Number {
				r.fail(join(p, tagPatientSetupNumber), "patient setup number is duplicated")
			}
		}
		rec.PatientSetups = append(rec.PatientSetups, s)
	}
	rec.MeasuredDoseReferences = rsDoseReferences(r, obj, false)
	rec.CalculatedDoseReferences = rsDoseReferences(r, obj, true)
	switch sop {
	case RTBeamsTreatmentRecordStorage, RTIonBeamsTreatmentRecordStorage:
		rsReadBeams(r, obj, rec)
	case RTBrachyTreatmentRecordStorage:
		rsReadBrachy(r, obj, rec)
	case RTTreatmentSummaryRecordStorage:
		rec.Summary = rsReadSummary(r, obj)
	}
	if rec.Summary == nil && obj.Has(ft(0x30080200)) {
		rec.Summary = rsReadSummary(r, obj)
	}
	rsValidateDoses(r, rec)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(r.errors) > 0 {
		return nil, &ValidationError{Problems: r.errors}
	}
	rec.Warnings = r.warnings
	return rec, nil
}

func rsText(r *reader, o *object.Object, t core.Tag) Attribute[string] {
	if !o.Has(t) {
		return Attribute[string]{}
	}
	s := r.str(o, t)
	if s == "" {
		return Attribute[string]{Present: true}
	}
	return Value(s)
}
func rsCopy[T any](a Attribute[T]) Attribute[T] {
	if a.Value != nil {
		return Value(*a.Value)
	}
	return a
}
func rsRequiredPresence(r *reader, o *object.Object, t core.Tag, p string) {
	if !o.Has(t) {
		r.fail(join(p, t), "required attribute is absent")
	}
}
func rsUID(s string) bool {
	if s == "" || len(s) > 64 || strings.Trim(s, "0123456789.") != "" {
		return false
	}
	for _, part := range strings.Split(s, ".") {
		if part == "" || len(part) > 1 && part[0] == '0' {
			return false
		}
	}
	return true
}
func rsReferences(r *reader, o *object.Object, t core.Tag, p string) []Reference {
	out := r.references(o, t)
	seen := map[string]bool{}
	for i, ref := range out {
		ip := item(p, t, i)
		if !rsUID(ref.SOPClassUID) || !rsUID(ref.SOPInstanceUID) {
			r.fail(ip, "reference identity is missing or invalid")
		}
		if seen[ref.SOPInstanceUID] {
			r.fail(ip, "reference identity is duplicated")
		}
		seen[ref.SOPInstanceUID] = true
	}
	return out
}
func rsDate(s string) bool {
	if len(s) != 8 {
		return false
	}
	_, e := time.Parse("20060102", s)
	return e == nil
}
func rsTime(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) > 2 {
		return false
	}
	base := parts[0]
	if len(base) != 2 && len(base) != 4 && len(base) != 6 {
		return false
	}
	if len(parts) == 2 && (len(base) != 6 || len(parts[1]) < 1 || len(parts[1]) > 6 || strings.Trim(parts[1], "0123456789") != "") {
		return false
	}
	if strings.Trim(base, "0123456789") != "" {
		return false
	}
	for i := 0; i < len(base); i += 2 {
		n, _ := strconv.Atoi(base[i : i+2])
		max := 59
		if i == 0 {
			max = 23
		}
		if i == 4 {
			max = 60
		}
		if n > max {
			return false
		}
	}
	return true
}
func rsTimezone(s string) bool {
	if len(s) != 5 || !oneOf(s[:1], "+", "-") || strings.Trim(s[1:], "0123456789") != "" {
		return false
	}
	h, _ := strconv.Atoi(s[1:3])
	m, _ := strconv.Atoi(s[3:])
	return m < 60 && (s[0] == '+' && h <= 14 && (h < 14 || m == 0) || s[0] == '-' && h <= 12 && (h < 12 || m == 0))
}
func rsCheckDateTime(r *reader, d, t Attribute[string], dt, tt core.Tag, p string) {
	if d.Value != nil && !rsDate(*d.Value) {
		r.fail(join(p, dt), "date is invalid")
	}
	if t.Value != nil && !rsTime(*t.Value) {
		r.fail(join(p, tt), "time is invalid")
	}
}
func rsNonnegative(r *reader, a Attribute[float64], t core.Tag, p string) {
	attrNonnegative(r, a, t, p, false)
}
func rsPositiveInt(r *reader, a Attribute[int], t core.Tag, p string, required bool) {
	if required && a.Value == nil || a.Value != nil && *a.Value <= 0 {
		r.fail(join(p, t), "positive integer is missing or invalid")
	}
}

func rsProfile(ctx context.Context, o *object.Object, sop, p string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p == "" {
		if origin, _ := o.GetString(ft(0x300a0709)); clean(origin) != "" && !oneOf(clean(origin), "DEVICE", "SIMULATION") {
			return pathError(ErrUnsupportedFeature, ft(0x300a0709).String(), "content origin is outside DEVICE/SIMULATION profile")
		}
		for _, seq := range []core.Tag{ft(0x30080020), ft(0x30080021), ft(0x30080110)} {
			if o.Has(seq) && (sop == RTBeamsTreatmentRecordStorage && seq != ft(0x30080020) || sop == RTIonBeamsTreatmentRecordStorage && seq != ft(0x30080021) || sop == RTBrachyTreatmentRecordStorage && seq != ft(0x30080110) || sop == RTTreatmentSummaryRecordStorage) {
				return pathError(ErrInvalidObject, seq.String(), "session does not match SOP class")
			}
		}
	}
	for _, x := range []struct {
		t     core.Tag
		allow []string
	}{{tagBrachyTreatmentTechnique, []string{"INTRACAVITARY", "INTERSTITIAL", "INTRALUMENARY", "INTRAVASCULAR", "CONTACT"}}, {ft(0x300a0288), []string{"STEPWISE", "FIXED"}}} {
		if v, ok := o.GetString(x.t); ok && clean(v) != "" && !oneOf(clean(v), x.allow...) {
			return pathError(ErrUnsupportedFeature, join(p, x.t), "brachy feature is outside profile")
		}
	}
	if sop == RTIonBeamsTreatmentRecordStorage {
		for _, x := range []struct {
			t     core.Tag
			allow []string
		}{{tagRadiationType, []string{"PROTON", "ION"}}, {ft(0x300a0308), []string{"NONE", "MODULATED"}}} {
			if v, ok := o.GetString(x.t); ok && clean(v) != "" && !oneOf(clean(v), x.allow...) {
				return pathError(ErrUnsupportedFeature, join(p, x.t), "ion feature is outside profile")
			}
		}
	}
	for _, t := range []core.Tag{tagNumberOfWedges, tagNumberOfCompensators, tagNumberOfBoli, tagNumberOfBlocks, ft(0x300a0330), ft(0x300a0340)} {
		r := &reader{}
		if n := r.intOpt(o, t, p); n != nil && *n != 0 {
			return pathError(ErrUnsupportedFeature, join(p, t), "nonzero accessory count is outside profile")
		}
	}
	for _, t := range []core.Tag{ft(0x30080060), ft(0x30080068), ft(0x300800a1), ft(0x300800a2), ft(0x300800b0), ft(0x300800c0), ft(0x300800d0), ft(0x300800d1), ft(0x300c00b0), ft(0x300a0107), ft(0x300a0420), ft(0x300a0116), ft(0x300a03ac), ft(0x300800f4), ft(0x300800f6), ft(0x300a0370), ft(0x300a0380), ft(0x300a0506), ft(0x300c0120), ft(0x300a065a), ft(0x300c0125), ft(0x30080120), ft(0x30080150)} {
		if o.Has(t) {
			return pathError(ErrUnsupportedFeature, join(p, t), "accessory, correction, or advanced delivery structure is outside profile")
		}
	}
	if flag, _ := o.GetString(ft(0x300800a3)); flag != "" && flag != "NO" {
		return pathError(ErrUnsupportedFeature, join(p, ft(0x300800a3)), "enhanced devices are outside profile")
	}
	for _, e := range o.Elements() {
		if sq, ok := e.Value.(core.SequenceValue); ok {
			for i, it := range sq.Items {
				child := object.FromElements(it.Elements, std.Dictionary)
				child.SetValueByteOrder(o.ValueByteOrder())
				if err := rsProfile(ctx, child, sop, item(p, e.Tag(), i)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func rsDoseReferences(r *reader, o *object.Object, calc bool) []RecordDoseReference {
	seq, num, val, desc := ft(0x30080010), ft(0x30080064), ft(0x30080016), ft(0x30080012)
	if calc {
		seq, num, val, desc = ft(0x30080070), ft(0x30080072), ft(0x30080076), ft(0x30080074)
	}
	out := []RecordDoseReference(nil)
	seen := map[string]bool{}
	for i, d := range r.sequence(o, seq) {
		p := item("", seq, i)
		v := RecordDoseReference{Number: intAttribute(r, d, num, p), PlanNumber: intAttribute(r, d, tagReferencedDoseReferenceNumber, p), Value: floatAttribute(r, d, val, p), Units: r.code(d, ft(0x30040002)), Type: rsText(r, d, ft(0x30080014)), Description: r.str(d, desc)}
		rsDoseChoice(r, v.Number, v.PlanNumber, num, p)
		key := rsDoseKey(v.Number, v.PlanNumber)
		if seen[key] {
			r.fail(p, "dose reference is duplicated")
		}
		seen[key] = true
		rsRequiredPresence(r, d, val, p)
		rsNonnegative(r, v.Value, val, p)
		if !calc {
			if !oneOf(v.Units, "GY", "RELATIVE") {
				r.fail(join(p, ft(0x30040002)), "measured dose unit is invalid")
			}
			rsRequiredPresence(r, d, ft(0x30080014), p)
		}
		out = append(out, v)
	}
	return out
}
func rsDoseKey(local, plan Attribute[int]) string {
	if local.Value != nil {
		return "L" + strconv.Itoa(*local.Value)
	}
	if plan.Value != nil {
		return "P" + strconv.Itoa(*plan.Value)
	}
	return ""
}
func rsDoseChoice(r *reader, local, plan Attribute[int], t core.Tag, p string) {
	if (local.Value == nil) == (plan.Value == nil) {
		r.fail(join(p, t), "exactly one local or plan dose reference is required")
	}
	rsPositiveInt(r, local, t, p, false)
	rsPositiveInt(r, plan, tagReferencedDoseReferenceNumber, p, false)
}
func rsDoseValues(r *reader, o *object.Object, p string, calc bool) []RecordDoseValue {
	seq, num, val := ft(0x30080080), ft(0x30080082), ft(0x30080016)
	if calc {
		seq, num, val = ft(0x30080090), ft(0x30080092), ft(0x30080076)
	}
	out := []RecordDoseValue(nil)
	seen := map[string]bool{}
	for i, d := range r.sequence(o, seq) {
		dp := item(p, seq, i)
		v := RecordDoseValue{PlanNumber: intAttribute(r, d, tagReferencedDoseReferenceNumber, dp), LocalNumber: intAttribute(r, d, num, dp), Value: requiredFloat(r, d, val, dp)}
		rsDoseChoice(r, v.LocalNumber, v.PlanNumber, num, dp)
		if v.Value < 0 {
			r.fail(join(dp, val), "dose value is negative")
		}
		key := rsDoseKey(v.LocalNumber, v.PlanNumber)
		if seen[key] {
			r.fail(dp, "dose reference is duplicated")
		}
		seen[key] = true
		out = append(out, v)
	}
	return out
}

func rsReadBeams(r *reader, o *object.Object, rec *TreatmentRecord) {
	ion := rec.SOPClassUID == RTIonBeamsTreatmentRecordStorage
	seq, cpseq := ft(0x30080020), ft(0x30080040)
	if ion {
		seq, cpseq = ft(0x30080021), ft(0x30080041)
	}
	rsRequiredPresence(r, o, tagNumberOfFractionsPlanned, "")
	if !oneOf(rec.PrimaryDosimeterUnit, "MU", "MINUTE") && !ion || ion && !oneOf(rec.PrimaryDosimeterUnit, "MU", "NP") {
		r.fail(tagPrimaryDosimeterUnit.String(), "primary dosimeter unit is outside profile")
	}
	items := r.sequence(o, seq)
	if len(items) == 0 {
		r.fail(seq.String(), "beam sessions are missing")
	}
	for i, b := range items {
		if r.ctx.Err() != nil {
			return
		}
		p := item("", seq, i)
		v := RecordBeamSession{ReferencedBeamNumber: intAttribute(r, b, tagReferencedBeamNumber, p), ReferencedPatientSetupNumber: intAttribute(r, b, tagReferencedPatientSetupNumber, p), ReferencedToleranceTableNumber: intAttribute(r, b, tagReferencedToleranceTableNumber, p), Name: r.str(b, tagBeamName), Description: r.str(b, tagBeamDescription), Type: requiredText(r, b, tagBeamType, p), RadiationType: requiredText(r, b, tagRadiationType, p), FractionNumber: intAttribute(r, b, ft(0x30080022), p), ClinicalFractionNumber: intAttribute(r, b, ft(0x300a0705), p), DeliveryType: rsText(r, b, tagTreatmentDeliveryType), VerificationStatus: rsText(r, b, ft(0x3008002c)), TerminationStatus: requiredText(r, b, ft(0x3008002a), p), SpecifiedPrimaryMeterset: floatAttribute(r, b, ft(0x30080032), p), SpecifiedSecondaryMeterset: floatAttribute(r, b, ft(0x30080033), p), DeliveredPrimaryMeterset: floatAttribute(r, b, ft(0x30080036), p), DeliveredSecondaryMeterset: floatAttribute(r, b, ft(0x30080037), p), SpecifiedTreatmentTime: floatAttribute(r, b, ft(0x3008003a), p), DeliveredTreatmentTime: floatAttribute(r, b, ft(0x3008003b), p), SourceAxisDistance: floatAttribute(r, b, tagSourceAxisDistance, p), AtomicNumber: intAttribute(r, b, ft(0x300a0304), p), MassNumber: intAttribute(r, b, ft(0x300a0302), p), ChargeState: intAttribute(r, b, ft(0x300a0306), p), ScanMode: r.code(b, ft(0x300a0308))}
		rsValidateSession(r, b, p, v.FractionNumber, v.DeliveryType, v.VerificationStatus, v.TerminationStatus)
		if !oneOf(v.Type, "STATIC", "DYNAMIC") {
			r.fail(join(p, tagBeamType), "beam type is invalid")
		}
		if ion {
			if !oneOf(v.RadiationType, "PROTON", "ION") {
				r.fail(join(p, tagRadiationType), "ion radiation type is outside profile")
			}
			v.PatientSupportType = requiredText(r, b, ft(0x300a0350), p)
			if !oneOf(v.ScanMode, "NONE", "MODULATED") {
				r.fail(join(p, ft(0x300a0308)), "scan mode is outside profile")
			}
			rsPositiveInt(r, v.ReferencedBeamNumber, tagReferencedBeamNumber, p, true)
			if v.Name == "" {
				r.fail(join(p, tagBeamName), "ion beam name is missing")
			}
			if v.RadiationType == "ION" {
				if v.AtomicNumber.Value == nil || v.MassNumber.Value == nil || v.ChargeState.Value == nil {
					r.fail(join(p, ft(0x300a0302)), "ion species is incomplete")
				} else if *v.AtomicNumber.Value <= 0 || *v.MassNumber.Value < *v.AtomicNumber.Value || *v.ChargeState.Value == 0 || math.Abs(float64(*v.ChargeState.Value)) > float64(*v.AtomicNumber.Value) {
					r.fail(join(p, ft(0x300a0306)), "ion species is inconsistent")
				}
			}
		} else if !oneOf(v.RadiationType, "PHOTON", "ELECTRON", "NEUTRON", "PROTON") {
			r.fail(join(p, tagRadiationType), "radiation type is outside profile")
		}
		for _, t := range []core.Tag{tagNumberOfWedges, tagNumberOfCompensators, tagNumberOfBoli, tagNumberOfBlocks} {
			a := intAttribute(r, b, t, p)
			if t == tagNumberOfWedges || ion {
				rsRequiredPresence(r, b, t, p)
				if a.Value == nil {
					r.fail(join(p, t), "accessory count is missing")
				}
			}
			if a.Value != nil && *a.Value != 0 {
				r.fail(join(p, t), "nonzero accessory count is outside profile")
			}
		}
		if ion {
			for _, t := range []core.Tag{ft(0x300a0330), ft(0x300a0340)} {
				n := r.requiredInt(b, t, p, "Accessory Count")
				if n != 0 {
					r.fail(join(p, t), "nonzero accessory count is outside profile")
				}
			}
		}
		for j, d := range r.sequence(b, ft(0x300800a0)) {
			dp := item(p, ft(0x300800a0), j)
			device := BeamLimitingDevice{Type: requiredText(r, d, tagRTBeamLimitingDeviceType, dp), Pairs: r.requiredInt(d, tagNumberOfLeafJawPairs, dp, "Number of Leaf/Jaw Pairs")}
			if !oneOf(device.Type, "X", "Y", "ASYMX", "ASYMY", "MLCX", "MLCY") || device.Pairs <= 0 || !device.IsMLC() && device.Pairs != 1 {
				r.fail(dp, "beam limiting device is invalid")
			}
			for _, old := range v.Devices {
				if old.Type == device.Type {
					r.fail(dp, "beam limiting device is duplicated")
				}
			}
			v.Devices = append(v.Devices, device)
		}
		if !ion && len(v.Devices) == 0 {
			r.fail(join(p, ft(0x300800a0)), "legacy beam limiting devices are missing")
		}
		if ion {
			snouts := r.sequence(b, ft(0x300800f0))
			if len(snouts) > 1 {
				r.fail(join(p, ft(0x300800f0)), "snout item count")
			}
			if len(snouts) == 1 {
				v.SnoutID = requiredText(r, snouts[0], ft(0x300a030f), item(p, ft(0x300800f0), 0))
			}
			for j, s := range r.sequence(b, ft(0x300800f2)) {
				sp := item(p, ft(0x300800f2), j)
				sh := RecordedRangeShifter{Number: r.requiredInt(s, ft(0x300c0100), sp, "Referenced Range Shifter Number"), ID: requiredText(r, s, ft(0x300a0318), sp)}
				for _, old := range v.RangeShifters {
					if old.Number == sh.Number {
						r.fail(sp, "range shifter reference is duplicated")
					}
				}
				v.RangeShifters = append(v.RangeShifters, sh)
			}
			n := r.requiredInt(b, ft(0x300a0312), p, "Number of Range Shifters")
			if n != len(v.RangeShifters) {
				r.fail(join(p, ft(0x300800f2)), "range shifter count is inconsistent")
			}
		}
		for _, x := range []struct {
			a Attribute[float64]
			t core.Tag
		}{{v.SpecifiedPrimaryMeterset, ft(0x30080032)}, {v.SpecifiedSecondaryMeterset, ft(0x30080033)}, {v.DeliveredPrimaryMeterset, ft(0x30080036)}, {v.DeliveredSecondaryMeterset, ft(0x30080037)}, {v.SpecifiedTreatmentTime, ft(0x3008003a)}, {v.DeliveredTreatmentTime, ft(0x3008003b)}, {v.SourceAxisDistance, tagSourceAxisDistance}} {
			rsNonnegative(r, x.a, x.t, p)
		}
		v.MeasuredDoses = rsDoseValues(r, b, p, false)
		v.CalculatedDoses = rsDoseValues(r, b, p, true)
		v.VerificationImages = rsReferences(r, b, ft(0x300c0040), p)
		points := r.sequence(b, cpseq)
		n := r.requiredInt(b, tagNumberOfControlPoints, p, "Number of Control Points")
		if n != len(points) || n < 2 {
			r.fail(join(p, tagNumberOfControlPoints), "control point count is inconsistent")
		}
		state := RecordMachineState{}
		for j, c := range points {
			if r.ctx.Err() != nil {
				return
			}
			pp := item(p, cpseq, j)
			cp := RecordControlPoint{ReferencedIndex: intAttribute(r, c, ft(0x300c00f0), pp), Date: requiredText(r, c, ft(0x30080024), pp), Time: requiredText(r, c, ft(0x30080025), pp), SpecifiedMeterset: floatAttribute(r, c, ft(0x30080042), pp), DeliveredMeterset: requiredFloat(r, c, ft(0x30080044), pp), EnergyUnit: r.code(c, ft(0x300a0015))}
			rsCheckDateTime(r, Value(cp.Date), Value(cp.Time), ft(0x30080024), ft(0x30080025), pp)
			rsRequiredPresence(r, c, ft(0x30080042), pp)

			if ion {
				for _, x := range []struct {
					t core.Tag
					a Attribute[int]
				}{{ft(0x300a0302), v.MassNumber}, {ft(0x300a0304), v.AtomicNumber}, {ft(0x300a0306), v.ChargeState}} {
					if c.Has(x.t) {
						a := intAttribute(r, c, x.t, pp)
						if a.Value == nil || x.a.Value == nil || *a.Value != *x.a.Value {
							r.fail(join(pp, x.t), "control point species differs from admitted beam")
						}
					}
				}
			}
			if cp.ReferencedIndex.Value != nil && *cp.ReferencedIndex.Value < 0 || ion && cp.ReferencedIndex.Value == nil {
				r.fail(join(pp, ft(0x300c00f0)), "control point reference is missing or invalid")
			}
			if cp.DeliveredMeterset < 0 || j > 0 && cp.DeliveredMeterset < v.ControlPoints[j-1].DeliveredMeterset {
				r.fail(join(pp, ft(0x30080044)), "delivered meterset decreases or is negative")
			}
			rsNonnegative(r, cp.SpecifiedMeterset, ft(0x30080042), pp)
			rateSet, rateDelivered := tagDoseRateSet, ft(0x30080048)
			if ion {
				rateSet, rateDelivered = ft(0x30080045), ft(0x30080046)
			}
			cp.RateSet = floatAttribute(r, c, rateSet, pp)
			cp.RateDelivered = floatAttribute(r, c, rateDelivered, pp)
			if !ion {
				rsRequiredPresence(r, c, rateSet, pp)
				rsRequiredPresence(r, c, rateDelivered, pp)
			}
			rsNonnegative(r, cp.RateSet, rateSet, pp)
			rsNonnegative(r, cp.RateDelivered, rateDelivered, pp)
			rsReadState(r, c, &state, &cp, pp, j == 0, ion, &v)
			cp.State = rsCloneState(state)
			if !ion && cp.State.Energy.Present {
				if cp.EnergyUnit == "" && j > 0 && !c.Has(tagNominalBeamEnergy) {
					cp.EnergyUnit = v.ControlPoints[j-1].EnergyUnit
				}
				if !oneOf(cp.EnergyUnit, "MV", "MEV") || v.RadiationType == "PHOTON" && cp.EnergyUnit != "MV" || v.RadiationType == "ELECTRON" && cp.EnergyUnit != "MEV" {
					r.fail(join(pp, ft(0x300a0015)), "nominal energy unit is missing or inconsistent")
				}
			}
			if ion && v.ScanMode == "MODULATED" {
				cp.ScanSpotTuneID = requiredText(r, c, ft(0x300a0390), pp)
				count := r.requiredInt(c, ft(0x300a0392), pp, "Number of Scan Spot Positions")
				cp.SpotPositions = readFL(r, c, ft(0x300a0394), pp)
				cp.SpotMetersets = readFL(r, c, ft(0x30080047), pp)
				cp.NumberOfPaintings = intAttribute(r, c, ft(0x300a039a), pp)
				cp.SpotReordered = rsText(r, c, ft(0x300a0393))
				if count <= 0 || count > math.MaxInt/2 || len(cp.SpotPositions) != 2*count || len(cp.SpotMetersets) != count {
					r.fail(join(pp, ft(0x300a0394)), "delivered spot count is inconsistent")
				}
				rsPositiveInt(r, cp.NumberOfPaintings, ft(0x300a039a), pp, true)
				for k, m := range cp.SpotMetersets {
					if k&1023 == 0 && r.ctx.Err() != nil {
						return
					}
					if m < 0 {
						r.fail(join(pp, ft(0x30080047)), "delivered spot meterset is negative")
					}
				}
				if cp.SpotReordered.Value != nil && !oneOf(*cp.SpotReordered.Value, "YES", "NO") {
					r.fail(join(pp, ft(0x300a0393)), "spot reordered flag is invalid")
				}
				cp.SpotPrescribedIndices = rsInts(r, c, ft(0x300a0391), pp)
				if cp.SpotReordered.Value != nil && *cp.SpotReordered.Value == "YES" && len(cp.SpotPrescribedIndices) != count || len(cp.SpotPrescribedIndices) != 0 && len(cp.SpotPrescribedIndices) != count {
					r.fail(join(pp, ft(0x300a0391)), "prescribed spot index count is inconsistent")
				}
				for _, n := range cp.SpotPrescribedIndices {
					if n <= 0 {
						r.fail(join(pp, ft(0x300a0391)), "prescribed spot index must be positive")
					}
				}
			} else if ion && (c.Has(ft(0x300a0394)) || c.Has(ft(0x30080047))) {
				r.fail(pp, "spots are outside passive scanning profile")
			}
			v.ControlPoints = append(v.ControlPoints, cp)
		}
		if ion && v.ScanMode == "MODULATED" {
			for j := 0; j+1 < len(v.ControlPoints); j++ {
				cp, next := v.ControlPoints[j], v.ControlPoints[j+1]
				sum := 0.0
				for k, m := range cp.SpotMetersets {
					if k&1023 == 0 && r.ctx.Err() != nil {
						return
					}
					sum += float64(m)
				}
				tol := math.Ldexp(sum, -23) + math.Ldexp(math.Abs(cp.DeliveredMeterset)+math.Abs(next.DeliveredMeterset), -40)
				if !finite(sum) || math.Abs(sum-(next.DeliveredMeterset-cp.DeliveredMeterset)) > tol {
					r.fail(item(p, cpseq, j)+"/"+ft(0x30080047).String(), "spot sum differs from delivered segment meterset")
				}
			}
		}
		rsValidateMetersets(r, &v, p, cpseq)
		rec.BeamSessions = append(rec.BeamSessions, v)
	}
}

func rsInts(r *reader, o *object.Object, t core.Tag, p string) []int {
	if !o.Has(t) {
		return nil
	}
	values, e := o.GetInts(t)
	if e != nil {
		r.fail(join(p, t), "integer list cannot be decoded")
		return nil
	}
	out := make([]int, len(values))
	for i, n := range values {
		if n < math.MinInt32 || n > math.MaxInt32 {
			r.fail(join(p, t), "integer is outside signed 32-bit range")
			return nil
		}
		out[i] = int(n)
	}
	return out
}
func rsValidateSession(r *reader, o *object.Object, p string, fraction Attribute[int], delivery, verification Attribute[string], termination string) {
	for _, t := range []core.Tag{ft(0x30080022), tagTreatmentDeliveryType, ft(0x3008002c)} {
		rsRequiredPresence(r, o, t, p)
	}
	rsPositiveInt(r, fraction, ft(0x30080022), p, false)
	if !oneOf(termination, "NORMAL", "OPERATOR", "MACHINE", "UNKNOWN") {
		r.fail(join(p, ft(0x3008002a)), "termination status is invalid")
	}
	if verification.Value != nil && !oneOf(*verification.Value, "VERIFIED", "VERIFIED_OVR", "NOT_VERIFIED") {
		r.fail(join(p, ft(0x3008002c)), "verification status is invalid")
	}
	if delivery.Value != nil && !oneOf(*delivery.Value, "TREATMENT", "OPEN_PORTFILM", "TRMT_PORTFILM", "CONTINUATION", "SETUP", "VERIFICATION") {
		r.fail(join(p, tagTreatmentDeliveryType), "delivery type is outside profile")
	}
}

func rsStateNumbers(s *RecordMachineState) []struct {
	t core.Tag
	a *Attribute[float64]
} {
	return []struct {
		t core.Tag
		a *Attribute[float64]
	}{{tagNominalBeamEnergy, &s.Energy}, {tagGantryAngle, &s.GantryAngle}, {tagBLDAngle, &s.BeamLimitingDeviceAngle}, {tagPatientSupportAngle, &s.PatientSupportAngle}, {tagTableTopEccentricAngle, &s.TableTopEccentricAngle}, {ft(0x300a0124), &s.TableTopEccentricAxisDistance}, {tagTableTopPitchAngle, &s.TableTopPitchAngle}, {tagTableTopRollAngle, &s.TableTopRollAngle}, {ft(0x300a014a), &s.GantryPitchAngle}, {tagTableTopVerticalPosition, &s.TableTopVerticalPosition}, {tagTableTopLongitudinalPosition, &s.TableTopLongitudinalPosition}, {tagTableTopLateralPosition, &s.TableTopLateralPosition}, {ft(0x300a030d), &s.SnoutPosition}}
}
func rsStateTexts(s *RecordMachineState) []struct {
	t core.Tag
	a *Attribute[string]
} {
	return []struct {
		t core.Tag
		a *Attribute[string]
	}{{tagGantryRotationDirection, &s.GantryDirection}, {tagBLDRotationDirection, &s.BeamLimitingDeviceDirection}, {tagPatientSupportRotationDir, &s.PatientSupportDirection}, {tagTableTopEccentricRotationDir, &s.TableTopEccentricDirection}, {ft(0x300a0142), &s.TableTopPitchDirection}, {ft(0x300a0146), &s.TableTopRollDirection}, {ft(0x300a014c), &s.GantryPitchDirection}}
}
func rsCloneState(s RecordMachineState) RecordMachineState {
	for _, x := range rsStateNumbers(&s) {
		*x.a = rsCopy(*x.a)
	}
	for _, x := range rsStateTexts(&s) {
		*x.a = rsCopy(*x.a)
	}
	s.DevicePositions = append([]DevicePosition(nil), s.DevicePositions...)
	for i := range s.DevicePositions {
		s.DevicePositions[i].Positions = append([]float64(nil), s.DevicePositions[i].Positions...)
	}
	s.RangeShifterSettings = append([]RangeShifterSetting(nil), s.RangeShifterSettings...)
	for i := range s.RangeShifterSettings {
		s.RangeShifterSettings[i].Distance = rsCopy(s.RangeShifterSettings[i].Distance)
		s.RangeShifterSettings[i].WaterEquivalentThickness = rsCopy(s.RangeShifterSettings[i].WaterEquivalentThickness)
	}
	return s
}
func rsReadState(r *reader, o *object.Object, s *RecordMachineState, cp *RecordControlPoint, p string, first, ion bool, b *RecordBeamSession) {
	for _, x := range rsStateNumbers(s) {
		a := floatAttribute(r, o, x.t, p)
		if a.Present {
			*x.a = a
			cp.Encoded = append(cp.Encoded, x.t)
		}
	}
	for _, x := range rsStateTexts(s) {
		a := rsText(r, o, x.t)
		if a.Present {
			*x.a = a
			cp.Encoded = append(cp.Encoded, x.t)
		}
		if x.a.Value != nil && !oneOf(*x.a.Value, "CW", "CC", "NONE") {
			r.fail(join(p, x.t), "rotation direction is invalid")
		}
	}
	if first {
		for _, t := range []core.Tag{tagGantryAngle, tagGantryRotationDirection, tagBLDAngle, tagBLDRotationDirection, tagPatientSupportAngle, tagPatientSupportRotationDir} {
			if requiredText(r, o, t, p) == "" {
				continue
			}
		}
		for _, t := range []core.Tag{tagTableTopVerticalPosition, tagTableTopLongitudinalPosition, tagTableTopLateralPosition} {
			rsRequiredPresence(r, o, t, p)
		}
		if ion {
			requiredFloat(r, o, tagNominalBeamEnergy, p)
			for _, t := range []core.Tag{tagTableTopPitchAngle, ft(0x300a0142), tagTableTopRollAngle, ft(0x300a0146), ft(0x300a014a), ft(0x300a014c), ft(0x300a030d)} {
				rsRequiredPresence(r, o, t, p)
			}
		} else {
			requiredFloat(r, o, tagTableTopEccentricAngle, p)
			requiredText(r, o, tagTableTopEccentricRotationDir, p)
		}
	}
	if s.Energy.Value != nil && *s.Energy.Value < 0 {
		r.fail(join(p, tagNominalBeamEnergy), "nominal energy is negative")
	}
	if o.Has(tagBLDPositionSequence) {
		cp.Encoded = append(cp.Encoded, tagBLDPositionSequence)
		seen := map[string]bool{}
		for j, d := range r.sequence(o, tagBLDPositionSequence) {
			dp := item(p, tagBLDPositionSequence, j)
			v := DevicePosition{DeviceType: requiredText(r, d, tagRTBeamLimitingDeviceType, dp), Positions: r.numbers(d, tagLeafJawPositions, dp)}
			pairs := 0
			for _, def := range b.Devices {
				if def.Type == v.DeviceType {
					pairs = def.Pairs
				}
			}
			if pairs <= 0 || len(v.Positions) != 2*pairs || seen[v.DeviceType] {
				r.fail(dp, "device position reference or count is inconsistent")
			}
			seen[v.DeviceType] = true
			replaced := false
			for k, old := range s.DevicePositions {
				if old.DeviceType == v.DeviceType {
					s.DevicePositions[k] = v
					replaced = true
				}
			}
			if !replaced {
				s.DevicePositions = append(s.DevicePositions, v)
			}
		}
	}
	if first && len(s.DevicePositions) != len(b.Devices) {
		r.fail(join(p, tagBLDPositionSequence), "first device positions are incomplete")
	}
	if o.Has(ft(0x300a0360)) {
		cp.Encoded = append(cp.Encoded, ft(0x300a0360))
		seen := map[int]bool{}
		for j, d := range r.sequence(o, ft(0x300a0360)) {
			dp := item(p, ft(0x300a0360), j)
			v := RangeShifterSetting{Number: r.requiredInt(d, ft(0x300c0100), dp, "Referenced Range Shifter Number"), Setting: requiredText(r, d, ft(0x300a0362), dp), Distance: floatAttribute(r, d, ft(0x300a0364), dp), WaterEquivalentThickness: floatAttribute(r, d, ft(0x300a0366), dp)}
			found := false
			for _, def := range b.RangeShifters {
				if def.Number == v.Number {
					found = true
				}
			}
			if !found || seen[v.Number] {
				r.fail(dp, "range shifter reference is missing or duplicated")
			}
			seen[v.Number] = true
			replaced := false
			for k, old := range s.RangeShifterSettings {
				if old.Number == v.Number {
					s.RangeShifterSettings[k] = v
					replaced = true
				}
			}
			if !replaced {
				s.RangeShifterSettings = append(s.RangeShifterSettings, v)
			}
		}
	}
	if first && len(s.RangeShifterSettings) != len(b.RangeShifters) {
		r.fail(join(p, ft(0x300a0360)), "first range shifter settings are incomplete")
	}
}

func rsReadBrachy(r *reader, o *object.Object, rec *TreatmentRecord) {
	rsRequiredPresence(r, o, tagNumberOfFractionsPlanned, "")
	if rec.BrachyTechnique == "PERMANENT" || rec.BrachyTechnique == "" {
		r.fail(tagBrachyTreatmentTechnique.String(), "brachy technique is outside profile")
	}
	if !oneOf(rec.BrachyType, "HDR", "MDR", "LDR", "PDR", "MANUAL") {
		r.fail(tagBrachyTreatmentType.String(), "brachy type is outside profile")
	}
	sources := map[int]bool{}
	for i, s := range r.sequence(o, ft(0x30080100)) {
		p := item("", ft(0x30080100), i)
		v := RecordedBrachySource{BrachySource: BrachySource{Number: r.requiredInt(s, ft(0x300a0212), p, "Source Number"), Type: requiredText(r, s, ft(0x300a0214), p), IsotopeName: requiredText(r, s, ft(0x300a0226), p), IsotopeHalfLife: floatAttribute(r, s, ft(0x300a0228), p), StrengthUnits: r.code(s, ft(0x300a0229)), ReferenceAirKermaRate: floatAttribute(r, s, ft(0x300a022a), p), Strength: floatAttribute(r, s, ft(0x300a022b), p), StrengthReferenceDate: requiredText(r, s, ft(0x300a022c), p), StrengthReferenceTime: requiredText(r, s, ft(0x300a022e), p), SerialNumber: r.str(s, ft(0x30080105))}, Manufacturer: rsText(r, s, ft(0x300a0216)), RecordedSerialNumber: rsText(r, s, ft(0x30080105)), ModelID: r.str(s, ft(0x300a021b))}
		if sources[v.Number] {
			r.fail(join(p, ft(0x300a0212)), "source number is duplicated")
		}
		sources[v.Number] = true
		if !oneOf(v.Type, "POINT", "LINE", "CYLINDER", "SPHERE") {
			r.fail(join(p, ft(0x300a0214)), "source type is outside profile")
		}
		for _, t := range []core.Tag{ft(0x300a0216), ft(0x30080105)} {
			rsRequiredPresence(r, s, t, p)
		}
		if v.IsotopeHalfLife.Value == nil || v.ReferenceAirKermaRate.Value == nil {
			r.fail(p, "source half life or air kerma is missing")
		}
		attrNonnegative(r, v.IsotopeHalfLife, ft(0x300a0228), p, true)
		rsNonnegative(r, v.ReferenceAirKermaRate, ft(0x300a022a), p)
		rsNonnegative(r, v.Strength, ft(0x300a022b), p)
		if v.StrengthUnits != "" && !oneOf(v.StrengthUnits, "AIR_KERMA_RATE", "DOSE_RATE_WATER") {
			r.fail(join(p, ft(0x300a0229)), "source strength unit is outside profile")
		}
		if v.StrengthUnits == "DOSE_RATE_WATER" && (v.Strength.Value == nil || v.ReferenceAirKermaRate.Value != nil && *v.ReferenceAirKermaRate.Value != 0) {
			r.fail(join(p, ft(0x300a022b)), "non-gamma source strength is inconsistent")
		}
		rsCheckDateTime(r, Value(v.StrengthReferenceDate), Value(v.StrengthReferenceTime), ft(0x300a022c), ft(0x300a022e), p)
		rec.BrachySources = append(rec.BrachySources, v)
	}
	if len(rec.BrachySources) == 0 {
		r.fail(ft(0x30080100).String(), "recorded sources are missing")
	}
	for i, s := range r.sequence(o, ft(0x30080110)) {
		p := item("", ft(0x30080110), i)
		v := RecordBrachySession{ReferencedSetupNumber: intAttribute(r, s, tagReferencedBrachySetupNumber, p), Type: requiredText(r, s, tagApplicationSetupType, p), Name: r.str(s, tagApplicationSetupName), Check: r.code(s, ft(0x30080116)), FractionNumber: intAttribute(r, s, ft(0x30080022), p), DeliveryType: rsText(r, s, tagTreatmentDeliveryType), VerificationStatus: rsText(r, s, ft(0x3008002c)), TerminationStatus: requiredText(r, s, ft(0x3008002a), p), TotalReferenceAirKerma: requiredFloat(r, s, tagTotalReferenceAirKerma, p), VerificationImages: rsReferences(r, s, ft(0x300c0040), p), MeasuredDoses: rsDoseValues(r, s, p, false), CalculatedDoses: rsDoseValues(r, s, p, true)}
		rsValidateSession(r, s, p, v.FractionNumber, v.DeliveryType, v.VerificationStatus, v.TerminationStatus)
		if v.TotalReferenceAirKerma < 0 {
			r.fail(join(p, tagTotalReferenceAirKerma), "reference air kerma is negative")
		}
		if v.Check != "" && !oneOf(v.Check, "PASSED", "FAILED", "UNKNOWN") {
			r.fail(join(p, ft(0x30080116)), "application setup check is invalid")
		}
		channels := map[int]bool{}
		for j, c := range r.sequence(s, ft(0x30080130)) {
			cp := item(p, ft(0x30080130), j)
			ch := RecordBrachyChannel{Number: r.requiredInt(c, ft(0x300a0282), cp, "Channel Number"), SourceNumber: r.requiredInt(c, tagReferencedSourceNumber, cp, "Referenced Source Number"), ReferencedChannelNumber: intAttribute(r, c, ft(0x00741406), cp), Movement: requiredText(r, c, ft(0x300a0288), cp), Length: floatAttribute(r, c, ft(0x300a0284), cp), EffectiveLength: floatAttribute(r, c, ft(0x300a0271), cp), InnerLength: floatAttribute(r, c, ft(0x300a0272), cp), AfterloaderChannelID: r.str(c, ft(0x300a0273)), SpecifiedTime: requiredFloat(r, c, ft(0x30080132), cp), DeliveredTime: requiredFloat(r, c, ft(0x30080134), cp), SpecifiedPulses: intAttribute(r, c, ft(0x30080136), cp), DeliveredPulses: intAttribute(r, c, ft(0x30080138), cp), SpecifiedPulseInterval: floatAttribute(r, c, ft(0x3008013a), cp), DeliveredPulseInterval: floatAttribute(r, c, ft(0x3008013c), cp), TransferTubeNumber: intAttribute(r, c, ft(0x300a02a2), cp), TransferTubeLength: floatAttribute(r, c, ft(0x300a02a4), cp), SafePosition: rsSafe(r, c), MeasuredDoses: rsDoseValues(r, c, cp, false), CalculatedDoses: rsDoseValues(r, c, cp, true)}
			if channels[ch.Number] {
				r.fail(join(cp, ft(0x300a0282)), "channel number is duplicated")
			}
			channels[ch.Number] = true
			if !sources[ch.SourceNumber] {
				r.fail(join(cp, tagReferencedSourceNumber), "source reference is missing")
			}
			if !oneOf(ch.Movement, "STEPWISE", "FIXED") {
				r.fail(join(cp, ft(0x300a0288)), "source movement is outside profile")
			}
			if ch.SpecifiedTime < 0 || ch.DeliveredTime < 0 {
				r.fail(cp, "channel time is negative")
			}
			rsRequiredPresence(r, c, ft(0x300a0284), cp)
			rsRequiredPresence(r, c, ft(0x300a02a2), cp)
			if ch.TransferTubeNumber.Value != nil {
				rsRequiredPresence(r, c, ft(0x300a02a4), cp)
			}
			if ch.EffectiveLength.Present {
				rsRequiredPresence(r, c, ft(0x300a0272), cp)
			}
			for _, x := range []struct {
				a Attribute[float64]
				t core.Tag
			}{{ch.Length, ft(0x300a0284)}, {ch.EffectiveLength, ft(0x300a0271)}, {ch.InnerLength, ft(0x300a0272)}, {ch.TransferTubeLength, ft(0x300a02a4)}, {ch.SpecifiedPulseInterval, ft(0x3008013a)}, {ch.DeliveredPulseInterval, ft(0x3008013c)}} {
				rsNonnegative(r, x.a, x.t, cp)
			}
			for k, a := range r.sequence(c, ft(0x30080140)) {
				ap := item(cp, ft(0x30080140), k)
				app := RecordedSourceApplicator{Number: intAttribute(r, a, ft(0x30080142), ap), ID: rsText(r, a, ft(0x300a0291)), Type: requiredText(r, a, ft(0x300a0292), ap), Name: r.str(a, ft(0x300a0294)), Length: requiredFloat(r, a, ft(0x300a0296), ap), TipLength: floatAttribute(r, a, ft(0x300a0274), ap), StepSize: floatAttribute(r, a, ft(0x300a02a0), ap)}
				rsRequiredPresence(r, a, ft(0x30080142), ap)
				rsRequiredPresence(r, a, ft(0x300a0291), ap)
				if !oneOf(app.Type, "FLEXIBLE", "RIGID") || app.Length <= 0 {
					r.fail(ap, "source applicator is invalid")
				}
				if ch.Movement == "STEPWISE" && (app.StepSize.Value == nil || *app.StepSize.Value <= 0) {
					r.fail(join(ap, ft(0x300a02a0)), "positive applicator step size is required")
				}
				if ch.EffectiveLength.Present {
					rsRequiredPresence(r, a, ft(0x300a0274), ap)
				}
				ch.Applicators = append(ch.Applicators, app)
			}
			ch.ControlPoints = rsReadBrachyCP(r, c, ft(0x30080160), cp)
			n := r.requiredInt(c, tagNumberOfControlPoints, cp, "Number of Control Points")
			if n != len(ch.ControlPoints) || n < 2 {
				r.fail(join(cp, ft(0x30080160)), "control point count is inconsistent")
			}
			if rec.BrachyType != "MANUAL" && rec.BrachyType != "PDR" {
				rsValidateSafe(r, ch.SafePosition, cp)
			}
			for k, po := range r.sequence(c, ft(0x30080171)) {
				pp := item(cp, ft(0x30080171), k)
				pulse := RecordBrachyPulse{Number: r.requiredInt(po, ft(0x30080172), pp, "Pulse Number"), SafePosition: rsSafe(r, po), ControlPoints: rsReadBrachyCP(r, po, ft(0x30080173), pp)}
				if pulse.Number <= 0 || k > 0 && pulse.Number != ch.Pulses[k-1].Number+1 {
					r.fail(join(pp, ft(0x30080172)), "pulse number is invalid or not consecutive")
				}
				rsValidateSafe(r, pulse.SafePosition, pp)
				if len(pulse.ControlPoints) < 2 {
					r.fail(join(pp, ft(0x30080173)), "pulse control points are missing")
				}
				ch.Pulses = append(ch.Pulses, pulse)
			}
			if rec.BrachyType == "PDR" {
				rsPositiveInt(r, ch.SpecifiedPulses, ft(0x30080136), cp, true)
				if ch.DeliveredPulses.Value == nil || *ch.DeliveredPulses.Value < 0 {
					r.fail(join(cp, ft(0x30080138)), "delivered pulse count is missing or negative")
				}
				if ch.SpecifiedPulseInterval.Value == nil || ch.DeliveredPulseInterval.Value == nil {
					r.fail(cp, "PDR pulse intervals are missing")
				}
				if ch.DeliveredPulses.Value != nil {
					if *ch.DeliveredPulses.Value > math.MaxInt/2 || len(ch.ControlPoints) != 2*(*ch.DeliveredPulses.Value) {
						r.fail(join(cp, ft(0x30080160)), "PDR endpoint count differs from delivered pulses")
					}
					if c.Has(ft(0x30080171)) && len(ch.Pulses) != *ch.DeliveredPulses.Value {
						r.fail(join(cp, ft(0x30080171)), "pulse sequence count differs from delivered pulses")
					}
				}
			} else if len(ch.Pulses) != 0 {
				r.fail(join(cp, ft(0x30080171)), "pulse records are outside non-PDR profile")
			}

			if rec.BrachyType == "PDR" && len(ch.Pulses)*2 == len(ch.ControlPoints) {
				for k, pulse := range ch.Pulses {
					if len(pulse.ControlPoints) >= 2 {
						a, b := pulse.ControlPoints[0], pulse.ControlPoints[len(pulse.ControlPoints)-1]
						for m, x := range []RecordBrachyControlPoint{a, b} {
							y := ch.ControlPoints[2*k+m]
							if x.Date != y.Date || x.Time != y.Time || !semanticEqual(x.RelativePosition, y.RelativePosition) || x.ReferencedIndex.Value != nil && y.ReferencedIndex.Value != nil && *x.ReferencedIndex.Value != *y.ReferencedIndex.Value {
								r.fail(item(cp, ft(0x30080171), k), "pulse endpoints differ from channel endpoints")
							}
						}
					}
				}
			}
			if rec.BrachyType != "PDR" {
				rsCheckDwellPairs(r, ch.ControlPoints, ch.Movement, join(cp, ft(0x30080160)))
			}
			for k, pulse := range ch.Pulses {
				rsCheckDwellPairs(r, pulse.ControlPoints, ch.Movement, item(cp, ft(0x30080171), k)+"/"+ft(0x30080173).String())
			}
			v.Channels = append(v.Channels, ch)
		}
		if len(v.Channels) == 0 {
			r.fail(join(p, ft(0x30080130)), "recorded channels are missing")
		}
		rec.BrachySessions = append(rec.BrachySessions, v)
	}
	if len(rec.BrachySessions) == 0 {
		r.fail(ft(0x30080110).String(), "brachy sessions are missing")
	}
}

func rsSafe(r *reader, o *object.Object) RecordSafePosition {
	return RecordSafePosition{r.str(o, ft(0x30080162)), r.str(o, ft(0x30080164)), r.str(o, ft(0x30080166)), r.str(o, ft(0x30080168))}
}
func rsValidateSafe(r *reader, s RecordSafePosition, p string) {
	rsCheckDateTime(r, Value(s.ExitDate), Value(s.ExitTime), ft(0x30080162), ft(0x30080164), p)
	rsCheckDateTime(r, Value(s.ReturnDate), Value(s.ReturnTime), ft(0x30080166), ft(0x30080168), p)
}
func rsReadBrachyCP(r *reader, o *object.Object, seq core.Tag, p string) []RecordBrachyControlPoint {
	out := []RecordBrachyControlPoint(nil)
	for i, c := range r.sequence(o, seq) {
		pp := item(p, seq, i)
		v := RecordBrachyControlPoint{ReferencedIndex: intAttribute(r, c, ft(0x300c00f0), pp), Date: requiredText(r, c, ft(0x30080024), pp), Time: requiredText(r, c, ft(0x30080025), pp), RelativePosition: requiredFloat(r, c, ft(0x300a02d2), pp)}
		if v.ReferencedIndex.Value != nil && *v.ReferencedIndex.Value < 0 {
			r.fail(join(pp, ft(0x300c00f0)), "control point reference is negative")
		}
		rsCheckDateTime(r, Value(v.Date), Value(v.Time), ft(0x30080024), ft(0x30080025), pp)
		out = append(out, v)
	}
	return out
}

func rsReadSummary(r *reader, o *object.Object) *RecordSummary {
	s := &RecordSummary{Status: requiredText(r, o, ft(0x30080200), ""), Comment: r.str(o, ft(0x30080202)), FirstDate: rsText(r, o, ft(0x30080054)), MostRecentDate: rsText(r, o, ft(0x30080056))}
	if !oneOf(s.Status, "NOT_STARTED", "ON_TREATMENT", "ON_BREAK", "SUSPENDED", "STOPPED", "COMPLETED") {
		r.fail(ft(0x30080200).String(), "treatment status is invalid")
	}
	for _, t := range []core.Tag{ft(0x30080054), ft(0x30080056)} {
		rsRequiredPresence(r, o, t, "")
	}
	rsCheckDateTime(r, s.FirstDate, Attribute[string]{}, ft(0x30080054), ft(0x30080251), "")
	rsCheckDateTime(r, s.MostRecentDate, Attribute[string]{}, ft(0x30080056), ft(0x30080251), "")
	seen := map[int]bool{}
	for i, g := range r.sequence(o, ft(0x30080220)) {
		p := item("", ft(0x30080220), i)
		v := RecordFractionGroupSummary{ReferencedNumber: intAttribute(r, g, ft(0x300c0022), p), Type: rsText(r, g, ft(0x30080224)), Planned: intAttribute(r, g, tagNumberOfFractionsPlanned, p), Delivered: intAttribute(r, g, ft(0x3008005a), p)}
		for _, t := range []core.Tag{ft(0x30080224), tagNumberOfFractionsPlanned, ft(0x3008005a)} {
			rsRequiredPresence(r, g, t, p)
		}
		if v.Type.Value != nil && !oneOf(*v.Type.Value, "EXTERNAL_BEAM", "BRACHY") {
			r.fail(join(p, ft(0x30080224)), "fraction group type is invalid")
		}
		if v.ReferencedNumber.Value != nil {
			n := *v.ReferencedNumber.Value
			if n <= 0 || seen[n] {
				r.fail(join(p, ft(0x300c0022)), "fraction group reference is invalid or duplicated")
			}
			seen[n] = true
		}
		for _, n := range []Attribute[int]{v.Planned, v.Delivered} {
			if n.Value != nil && *n.Value < 0 {
				r.fail(p, "fraction count is negative")
			}
		}
		fractions := map[int]bool{}
		for j, f := range r.sequence(g, ft(0x30080240)) {
			fp := item(p, ft(0x30080240), j)
			fs := RecordFractionStatus{Number: r.requiredInt(f, ft(0x30080223), fp, "Referenced Fraction Number"), ClinicalNumber: intAttribute(r, f, ft(0x300a0705), fp), Date: rsText(r, f, ft(0x30080250)), Time: rsText(r, f, ft(0x30080251)), TerminationStatus: rsText(r, f, ft(0x3008002a))}
			if fs.Number <= 0 || fractions[fs.Number] {
				r.fail(join(fp, ft(0x30080223)), "fraction number is invalid or duplicated")
			}
			fractions[fs.Number] = true
			for _, t := range []core.Tag{ft(0x30080250), ft(0x30080251), ft(0x3008002a)} {
				rsRequiredPresence(r, f, t, fp)
			}
			rsCheckDateTime(r, fs.Date, fs.Time, ft(0x30080250), ft(0x30080251), fp)
			if fs.TerminationStatus.Value != nil && !oneOf(*fs.TerminationStatus.Value, "NORMAL", "OPERATOR", "MACHINE", "UNKNOWN") {
				r.fail(join(fp, ft(0x3008002a)), "termination status is invalid")
			}
			v.Fractions = append(v.Fractions, fs)
		}
		s.FractionGroups = append(s.FractionGroups, v)
	}
	s.MeasuredDoses = rsSummaryDoses(r, o, ft(0x300800e0))
	s.CalculatedDoses = rsSummaryDoses(r, o, ft(0x30080050))
	return s
}
func rsSummaryDoses(r *reader, o *object.Object, seq core.Tag) []RecordSummaryDose {
	out := []RecordSummaryDose(nil)
	seen := map[int]bool{}
	for i, d := range r.sequence(o, seq) {
		p := item("", seq, i)
		v := RecordSummaryDose{PlanNumber: intAttribute(r, d, tagReferencedDoseReferenceNumber, p), Description: r.str(d, tagDoseReferenceDescription), CumulativeDose: requiredFloat(r, d, ft(0x30080052), p)}
		if v.CumulativeDose < 0 {
			r.fail(join(p, ft(0x30080052)), "cumulative dose is negative")
		}
		if v.PlanNumber.Value != nil {
			n := *v.PlanNumber.Value
			if n <= 0 || seen[n] {
				r.fail(join(p, tagReferencedDoseReferenceNumber), "dose reference is invalid or duplicated")
			}
			seen[n] = true
		}
		out = append(out, v)
	}
	return out
}
func rsValidateDoses(r *reader, rec *TreatmentRecord) {
	measured, calculated := map[int]bool{}, map[int]bool{}
	for _, v := range rec.MeasuredDoseReferences {
		if v.Number.Value != nil {
			measured[*v.Number.Value] = true
		}
	}
	for _, v := range rec.CalculatedDoseReferences {
		if v.Number.Value != nil {
			calculated[*v.Number.Value] = true
		}
	}
	check := func(values []RecordDoseValue, local map[int]bool, p string) {
		for i, v := range values {
			if v.LocalNumber.Value != nil && !local[*v.LocalNumber.Value] {
				r.fail(p+"["+strconv.Itoa(i)+"]", "local dose reference is missing")
			}
		}
	}
	for i, v := range rec.BeamSessions {
		p := item("", ft(0x30080020), i)
		if rec.SOPClassUID == RTIonBeamsTreatmentRecordStorage {
			p = item("", ft(0x30080021), i)
		}
		check(v.MeasuredDoses, measured, join(p, ft(0x30080080)))
		check(v.CalculatedDoses, calculated, join(p, ft(0x30080090)))
		if v.ReferencedPatientSetupNumber.Value != nil {
			found := false
			for _, s := range rec.PatientSetups {
				if s.Number == *v.ReferencedPatientSetupNumber.Value {
					found = true
				}
			}
			if !found {
				r.fail(join(p, tagReferencedPatientSetupNumber), "local patient setup reference is missing")
			}
		}
	}
	for i, v := range rec.BrachySessions {
		p := item("", ft(0x30080110), i)
		check(v.MeasuredDoses, measured, join(p, ft(0x30080080)))
		check(v.CalculatedDoses, calculated, join(p, ft(0x30080090)))
		for j, c := range v.Channels {
			p := item(p, ft(0x30080130), j)
			check(c.MeasuredDoses, measured, join(p, ft(0x30080080)))
			check(c.CalculatedDoses, calculated, join(p, ft(0x30080090)))
		}
	}
}

// ValidateReferencedPlan checks identifiers against an explicitly supplied
// matching plan. It does not validate dose, machine delivery or temporal order.
func (rec *TreatmentRecord) ValidateReferencedPlan(ctx context.Context, p *Plan) error {
	if ctx == nil {
		return ErrInvalidObject
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if rec == nil || p == nil || rec.ReferencedPlan == nil {
		return pathError(ErrInvalidObject, tagReferencedRTPlanSequence.String(), "an explicit referenced plan is required")
	}
	if rec.ReferencedPlan.SOPClassUID != p.SOPClassUID || rec.ReferencedPlan.SOPInstanceUID != p.SOPInstanceUID {
		return pathError(ErrInvalidObject, tagReferencedRTPlanSequence.String(), "supplied plan identity does not match")
	}
	if err := admitModel(ctx, reflect.ValueOf(struct {
		Record *TreatmentRecord
		Plan   *Plan
	}{rec, p}), DefaultLimits()); err != nil {
		return err
	}
	r := &reader{ctx: ctx, maxProblems: DefaultLimits().MaxProblems}
	group := map[int]bool{}
	dose := map[int]bool{}
	for _, g := range p.FractionGroups {
		group[g.Number] = true
	}
	for _, d := range p.DoseReferences {
		dose[d.Number] = true
	}
	if rec.FractionGroupNumber.Value != nil && !group[*rec.FractionGroupNumber.Value] {
		r.fail(ft(0x300c0022).String(), "fraction group reference is missing in supplied plan")
	}
	for i, b := range rec.BeamSessions {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		seq := ft(0x30080020)
		limit := -1
		shifters := map[int]bool{}
		if rec.SOPClassUID == RTIonBeamsTreatmentRecordStorage {
			seq = ft(0x30080021)
			for _, pb := range p.IonBeams {
				if b.ReferencedBeamNumber.Value != nil && pb.Number == *b.ReferencedBeamNumber.Value {
					limit = len(pb.ControlPoints)
					for _, s := range pb.RangeShifters {
						shifters[s.Number] = true
					}
				}
			}
		} else {
			for _, pb := range p.Beams {
				if b.ReferencedBeamNumber.Value != nil && pb.Number == *b.ReferencedBeamNumber.Value {
					limit = len(pb.ControlPoints)
				}
			}
		}
		path := item("", seq, i)
		if b.ReferencedBeamNumber.Value != nil && limit < 0 {
			r.fail(join(path, tagReferencedBeamNumber), "beam reference is missing in supplied plan")
		}
		for j, c := range b.ControlPoints {
			if c.ReferencedIndex.Value != nil && limit >= 0 && *c.ReferencedIndex.Value >= limit {
				r.fail(item(path, ft(0x30080040), j)+"/"+ft(0x300c00f0).String(), "control point reference is missing in supplied plan")
			}
		}
		for _, s := range b.RangeShifters {
			if !shifters[s.Number] {
				r.fail(join(path, ft(0x300800f2)), "range shifter reference is missing in supplied plan")
			}
		}
	}
	for i, s := range rec.BrachySessions {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		path := item("", ft(0x30080110), i)
		var setup *BrachySetup
		for j := range p.BrachySetups {
			if s.ReferencedSetupNumber.Value != nil && p.BrachySetups[j].Number == *s.ReferencedSetupNumber.Value {
				setup = &p.BrachySetups[j]
			}
		}
		if s.ReferencedSetupNumber.Value != nil && setup == nil {
			r.fail(join(path, tagReferencedBrachySetupNumber), "setup reference is missing in supplied plan")
		}
		for j, c := range s.Channels {
			if setup != nil && c.ReferencedChannelNumber.Value != nil {
				var channel *BrachyChannel
				for k := range setup.Channels {
					if setup.Channels[k].Number == *c.ReferencedChannelNumber.Value {
						channel = &setup.Channels[k]
					}
				}
				cp := item(path, ft(0x30080130), j)
				if channel == nil {
					r.fail(join(cp, ft(0x00741406)), "channel reference is missing in supplied plan")
				} else {
					check := func(points []RecordBrachyControlPoint) {
						for _, v := range points {
							if v.ReferencedIndex.Value != nil && *v.ReferencedIndex.Value >= len(channel.ControlPoints) {
								r.fail(join(cp, ft(0x300c00f0)), "control point reference is missing in supplied plan")
							}
						}
					}
					check(c.ControlPoints)
					for _, pulse := range c.Pulses {
						check(pulse.ControlPoints)
					}
				}
			}
		}
	}
	check := func(a Attribute[int], path string) {
		if a.Value != nil && !dose[*a.Value] {
			r.fail(path, "dose reference is missing in supplied plan")
		}
	}
	for _, v := range rec.MeasuredDoseReferences {
		check(v.PlanNumber, ft(0x30080010).String())
	}
	for _, v := range rec.CalculatedDoseReferences {
		check(v.PlanNumber, ft(0x30080070).String())
	}
	for _, b := range rec.BeamSessions {
		for _, values := range [][]RecordDoseValue{b.MeasuredDoses, b.CalculatedDoses} {
			for _, v := range values {
				check(v.PlanNumber, tagReferencedDoseReferenceNumber.String())
			}
		}
	}
	for _, s := range rec.BrachySessions {
		for _, values := range [][]RecordDoseValue{s.MeasuredDoses, s.CalculatedDoses} {
			for _, v := range values {
				check(v.PlanNumber, tagReferencedDoseReferenceNumber.String())
			}
		}
		for _, c := range s.Channels {
			for _, values := range [][]RecordDoseValue{c.MeasuredDoses, c.CalculatedDoses} {
				for _, v := range values {
					check(v.PlanNumber, tagReferencedDoseReferenceNumber.String())
				}
			}
		}
	}
	if rec.Summary != nil {
		for _, g := range rec.Summary.FractionGroups {
			if g.ReferencedNumber.Value != nil && !group[*g.ReferencedNumber.Value] {
				r.fail(ft(0x30080220).String(), "fraction group reference is missing in supplied plan")
			}
		}
		for _, values := range [][]RecordSummaryDose{rec.Summary.MeasuredDoses, rec.Summary.CalculatedDoses} {
			for _, v := range values {
				check(v.PlanNumber, tagReferencedDoseReferenceNumber.String())
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(r.errors) > 0 {
		return &ValidationError{Problems: r.errors}
	}
	return nil
}

// C.8.8.21.2 describes stored metersets, including clamping at an interrupted
// session's start/end. This relation uses recorded values only, not a dose or
// delivery calculation. The tolerance covers floating subtraction of DS values.
func rsValidateMetersets(r *reader, b *RecordBeamSession, path string, seq core.Tag) {
	if len(b.ControlPoints) < 2 {
		return
	}
	start := b.ControlPoints[0].DeliveredMeterset
	end := b.ControlPoints[len(b.ControlPoints)-1].DeliveredMeterset
	close := func(a, b, scale float64) bool { return math.Abs(a-b) <= math.Ldexp(math.Abs(a)+math.Abs(b)+scale, -48) }
	if v := b.DeliveredPrimaryMeterset.Value; v != nil && !close(*v, end-start, math.Abs(start)+math.Abs(end)) {
		r.fail(join(path, ft(0x30080036)), "beam meterset differs from recorded control point increment")
	}
	var prev *float64
	for i, cp := range b.ControlPoints {
		if err := r.ctx.Err(); err != nil {
			return
		}
		if cp.SpecifiedMeterset.Value == nil {
			continue
		}
		v := *cp.SpecifiedMeterset.Value
		p := item(path, seq, i)
		if prev != nil && v < *prev {
			r.fail(join(p, ft(0x30080042)), "specified meterset decreases")
		}
		prev = cp.SpecifiedMeterset.Value
		expected := math.Max(start, math.Min(v, end))
		if !close(cp.DeliveredMeterset, expected, math.Abs(start)+math.Abs(end)+math.Abs(v)) {
			r.fail(join(p, ft(0x30080044)), "delivered meterset is inconsistent with recorded partial session")
		}
	}
}

func rsCheckDwellPairs(r *reader, points []RecordBrachyControlPoint, movement, path string) {
	if movement == "STEPWISE" {
		if len(points)%2 != 0 {
			r.fail(path, "stepwise delivery requires dwell pairs")
		}
		for k := 0; k+1 < len(points); k += 2 {
			if points[k].RelativePosition != points[k+1].RelativePosition {
				r.fail(path+"["+strconv.Itoa(k+1)+"]", "delivered dwell pair positions differ")
			}
		}
	}
}
