// Package rtplan provides bounded owned models for conventional RT Plan,
// finite RT Ion Plan and brachy profiles, four Treatment Records and classic
// native RT Image. Control points retain declared versus inherited state.
// It does not calculate dose, infer source trajectories or verify treatment.
// See docs/RT_FAMILY_MODELS.md for the exact admitted profiles and limitations.
package rtplan

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ThalesMMS/dicom-go/object"
)

// SOP Class UIDs of the RT plan family.
const (
	RTPlanStorage                    = "1.2.840.10008.5.1.4.1.1.481.5"
	RTIonPlanStorage                 = "1.2.840.10008.5.1.4.1.1.481.8"
	RTBeamsTreatmentRecordStorage    = "1.2.840.10008.5.1.4.1.1.481.4"
	RTBrachyTreatmentRecordStorage   = "1.2.840.10008.5.1.4.1.1.481.6"
	RTTreatmentSummaryRecordStorage  = "1.2.840.10008.5.1.4.1.1.481.7"
	RTIonBeamsTreatmentRecordStorage = "1.2.840.10008.5.1.4.1.1.481.9"
)

// Defined terms used by the model (PS3.3 C.8.8.9 and C.8.8.14).
const (
	GeometryPatient         = "PATIENT"
	GeometryTreatmentDevice = "TREATMENT_DEVICE"

	BeamTypeStatic  = "STATIC"
	BeamTypeDynamic = "DYNAMIC"

	RotationClockwise        = "CW"
	RotationCounterClockwise = "CC"
	RotationNone             = "NONE"

	DeviceX     = "X"
	DeviceY     = "Y"
	DeviceAsymX = "ASYMX"
	DeviceAsymY = "ASYMY"
	DeviceMLCX  = "MLCX"
	DeviceMLCY  = "MLCY"
)

var (
	ErrUnsupportedSOPClass = errors.New("dicom/rtplan: unsupported SOP class")
	ErrInvalidObject       = errors.New("dicom/rtplan: invalid object")
)

// Problem locates one requirement the plan does not meet. Path uses the
// attribute path notation of package dicominspect, with zero-based item
// indexes, for example "(300A,00B0)[0]/(300A,0111)[2]/(300A,011C)". Message
// is fixed library text plus numbers from the plan; it never quotes
// free-text attribute values.
type Problem struct {
	Path    string
	Message string
}

func (p Problem) String() string {
	if p.Path == "" {
		return p.Message
	}
	return p.Path + ": " + p.Message
}

// ValidationError lists every structural problem that made Read refuse a
// plan. errors.Is(err, ErrInvalidObject) holds.
type ValidationError struct {
	Problems []Problem
}

func (e *ValidationError) Error() string {
	if e == nil || len(e.Problems) == 0 {
		return ErrInvalidObject.Error()
	}
	parts := make([]string, 0, len(e.Problems))
	for _, p := range e.Problems {
		parts = append(parts, p.String())
	}
	return fmt.Sprintf("%v: %s", ErrInvalidObject, strings.Join(parts, "; "))
}

func (e *ValidationError) Unwrap() error { return ErrInvalidObject }

// Reference identifies a referenced SOP Instance.
type Reference struct {
	SOPClassUID    string
	SOPInstanceUID string
}

// PlanReference is an item of the Referenced RT Plan Sequence.
type PlanReference struct {
	Reference
	Relationship string
}

// Plan is a validated conventional, Ion or detailed brachy RT Plan.
type Plan struct {
	// ModelBytes is the conservative logical admission used by a reader.
	// It is not a measurement of RSS or exact Go heap retention.
	ModelBytes          int64
	SOPClassUID         string
	SOPInstanceUID      string
	StudyInstanceUID    string
	SeriesInstanceUID   string
	FrameOfReferenceUID string

	Label       string
	Name        string
	Description string
	Date        string
	Time        string
	Intent      string
	// Geometry is PATIENT or TREATMENT_DEVICE.
	Geometry       string
	ApprovalStatus string

	ReferencedStructureSet *Reference
	ReferencedDoses        []Reference
	ReferencedPlans        []PlanReference

	PrescriptionDescription string
	DoseReferences          []DoseReference
	ToleranceTables         []ToleranceTable
	PatientSetups           []PatientSetup
	FractionGroups          []FractionGroup
	Beams                   []Beam
	IonBeams                []IonBeam
	Sources                 []BrachySource

	BrachyTreatmentTechnique string
	BrachyTreatmentType      string
	BrachySetups             []BrachySetup

	// Warnings are departures from PS3.3 that leave the model unambiguous.
	Warnings []Problem
}

// DoseReference is an item of the Dose Reference Sequence (RT Prescription).
type DoseReference struct {
	Number int
	UID    string
	// StructureType is POINT, VOLUME, COORDINATES or SITE.
	StructureType string
	Description   string
	// Type is TARGET or ORGAN_AT_RISK.
	Type                string
	ReferencedROINumber *int
	// PointCoordinates are the three patient coordinates (mm) of a
	// COORDINATES reference.
	PointCoordinates       []float64
	DeliveryMaximumDose    *float64
	TargetMinimumDose      *float64
	TargetPrescriptionDose *float64
	TargetMaximumDose      *float64
	OrganAtRiskMaximumDose *float64
}

// DeviceTolerance is a Beam Limiting Device Tolerance Sequence item.
type DeviceTolerance struct {
	DeviceType string
	Position   float64
}

// ToleranceTable is an item of the Tolerance Table Sequence.
type ToleranceTable struct {
	Number                          int
	Label                           string
	GantryAngle                     *float64
	BeamLimitingDeviceAngle         *float64
	PatientSupportAngle             *float64
	TableTopEccentricAngle          *float64
	TableTopVerticalPosition        *float64
	TableTopLongitudinalPosition    *float64
	TableTopLateralPosition         *float64
	BeamLimitingDevicePositionLimit []DeviceTolerance
}

// PatientSetup is an item of the Patient Setup Sequence.
type PatientSetup struct {
	Number                    int
	Label                     string
	PatientPosition           string
	PatientAdditionalPosition string
	SetupTechnique            string
}

// ReferencedBeam is an item of a fraction group's Referenced Beam Sequence.
type ReferencedBeam struct {
	BeamNumber int
	// Dose is the Beam Dose (Gy) per fraction.
	Dose *float64
	// Meterset is the Beam Meterset per fraction, in the beam's Primary
	// Dosimeter Unit (usually MU).
	Meterset *float64
}

// ReferencedBrachySetup is an item of the Referenced Brachy Application
// Setup Sequence.
type ReferencedBrachySetup struct {
	SetupNumber int
	Dose        *float64
}

// ReferencedDoseReference is an item of a fraction group's Referenced Dose
// Reference Sequence.
type ReferencedDoseReference struct {
	Number                 int
	ConstraintWeight       *float64
	DeliveryMaximumDose    *float64
	TargetPrescriptionDose *float64
}

// FractionGroup is an item of the Fraction Group Sequence.
type FractionGroup struct {
	Number      int
	Description string
	// FractionsPlanned is nil when Number of Fractions Planned is empty.
	FractionsPlanned          *int
	FractionPatternDigitsDay  *int
	RepeatFractionCycleLength *int
	FractionPattern           string
	BeamDoseMeaning           string

	NumberOfBeams           int
	NumberOfBrachySetups    int
	ReferencedBeams         []ReferencedBeam
	ReferencedBrachySetups  []ReferencedBrachySetup
	ReferencedDoseReference []ReferencedDoseReference
}

// BeamLimitingDevice is an item of a beam's Beam Limiting Device Sequence.
type BeamLimitingDevice struct {
	// Type is X, Y, ASYMX, ASYMY, MLCX or MLCY.
	Type           string
	SourceDistance *float64
	Pairs          int
	// LeafBoundaries holds the Pairs+1 leaf position boundaries (mm) of an
	// MLC, along the axis orthogonal to leaf travel; nil for jaws.
	LeafBoundaries []float64
}

// IsMLC reports whether the device is a multileaf collimator.
func (d BeamLimitingDevice) IsMLC() bool { return d.Type == DeviceMLCX || d.Type == DeviceMLCY }

// Wedge is an item of the Wedge Sequence.
type Wedge struct {
	Number      int
	Type        string
	ID          string
	Angle       float64
	Factor      *float64
	Orientation float64
}

// Block is an item of the conventional Block Sequence. Contour is the ordered
// closed polygon in IEC BEAM LIMITING DEVICE millimetres, projected onto the
// machine isocentric plane. Empty Type 2 data remain unavailable, not inferred.
type Block struct {
	Number         int
	Type           string
	Name           string
	Divergence     string
	NumberOfPoints int
	Contour        [][2]float64
	MaterialID     string
	Thickness      *float64
	Transmission   *float64
}

// Applicator is an item of the Applicator Sequence.
type Applicator struct {
	ID   string
	Type string
}

// DevicePosition is the position of one beam limiting device at a control
// point: 2N values in IEC leaf subscript order (bank 1 first, then bank 2).
type DevicePosition struct {
	DeviceType string
	Positions  []float64
}

// WedgePosition is an item of a control point's Wedge Position Sequence.
type WedgePosition struct {
	WedgeNumber int
	Position    string
}

// ControlPointDoseReference is an item of a control point's Referenced Dose
// Reference Sequence.
type ControlPointDoseReference struct {
	Number      int
	Coefficient *float64
}

// Field names a control point attribute that may be inherited.
type Field uint32

const (
	FieldNominalBeamEnergy Field = 1 << iota
	FieldDoseRateSet
	FieldGantryAngle
	FieldGantryRotationDirection
	FieldBeamLimitingDeviceAngle
	FieldBeamLimitingDeviceRotationDirection
	FieldPatientSupportAngle
	FieldPatientSupportRotationDirection
	FieldTableTopEccentricAngle
	FieldTableTopEccentricRotationDirection
	FieldTableTopVerticalPosition
	FieldTableTopLongitudinalPosition
	FieldTableTopLateralPosition
	FieldIsocenterPosition
	FieldDevicePositions
	FieldTableTopPitchAngle
	FieldTableTopRollAngle
)

// ControlPoint is one resolved control point. Every machine-state field
// holds the effective value at this control point: the value encoded in
// this item, or else the one inherited from the previous control point.
// Encoded records which fields this item encodes itself.
type ControlPoint struct {
	Index int
	// CumulativeMetersetWeight is nil when the plan leaves the weights empty.
	CumulativeMetersetWeight *float64

	NominalBeamEnergy *float64
	DoseRateSet       *float64

	GantryAngle                         float64
	GantryRotationDirection             string
	BeamLimitingDeviceAngle             float64
	BeamLimitingDeviceRotationDirection string
	PatientSupportAngle                 float64
	PatientSupportRotationDirection     string
	TableTopEccentricAngle              float64
	TableTopEccentricRotationDirection  string
	TableTopPitchAngle                  *float64
	TableTopRollAngle                   *float64

	TableTopVerticalPosition     *float64
	TableTopLongitudinalPosition *float64
	TableTopLateralPosition      *float64
	// IsocenterPosition holds three patient coordinates (mm), or nil.
	IsocenterPosition []float64
	// SourceToSurfaceDistance is not inherited.
	SourceToSurfaceDistance *float64

	// DevicePositions holds one entry per beam limiting device of the beam,
	// in the order of the Beam Limiting Device Sequence.
	DevicePositions []DevicePosition
	// EncodedDeviceTypes identifies devices explicitly positioned in this item;
	// other effective positions were inherited without interpolation.
	EncodedDeviceTypes []string
	WedgePositions     []WedgePosition
	DoseReferences     []ControlPointDoseReference

	Encoded Field
}

// Encodes reports whether this control point item encodes field itself.
func (cp ControlPoint) Encodes(field Field) bool { return cp.Encoded&field != 0 }

// Device returns the position of the device of the given type.
func (cp ControlPoint) Device(deviceType string) (DevicePosition, bool) {
	for _, d := range cp.DevicePositions {
		if d.DeviceType == deviceType {
			return d, true
		}
	}
	return DevicePosition{}, false
}

// Beam is an item of the Beam Sequence.
type Beam struct {
	Number                int
	Name                  string
	Description           string
	Type                  string
	RadiationType         string
	HighDoseTechniqueType string
	TreatmentMachineName  string
	Manufacturer          string
	PrimaryDosimeterUnit  string
	SourceAxisDistance    *float64
	TreatmentDeliveryType string

	ReferencedPatientSetupNumber   *int
	ReferencedToleranceTableNumber *int

	Devices []BeamLimitingDevice

	NumberOfWedges       int
	NumberOfCompensators int
	NumberOfBoli         int
	NumberOfBlocks       int
	Wedges               []Wedge
	Blocks               []Block
	Applicators          []Applicator

	FinalCumulativeMetersetWeight *float64
	ControlPoints                 []ControlPoint
}

// Device returns the beam limiting device of the given type.
func (b *Beam) Device(deviceType string) (BeamLimitingDevice, bool) {
	if b == nil {
		return BeamLimitingDevice{}, false
	}
	for _, d := range b.Devices {
		if d.Type == deviceType {
			return d, true
		}
	}
	return BeamLimitingDevice{}, false
}

// MLC returns the multileaf collimator of the beam, if it has one.
func (b *Beam) MLC() (BeamLimitingDevice, bool) {
	if b == nil {
		return BeamLimitingDevice{}, false
	}
	for _, d := range b.Devices {
		if d.IsMLC() {
			return d, true
		}
	}
	return BeamLimitingDevice{}, false
}

// BrachySetup owns the admitted Application Setup and its detailed channels.
type BrachySetup struct {
	Number                  int
	Type                    string
	Name                    string
	TotalReferenceAirKerma  *float64
	NumberOfChannels        int
	ReferencedSourceNumbers []int
	Channels                []BrachyChannel
}

// Beam returns the beam with the given Beam Number.
func (p *Plan) Beam(number int) (*Beam, bool) {
	if p == nil {
		return nil, false
	}
	for i := range p.Beams {
		if p.Beams[i].Number == number {
			return &p.Beams[i], true
		}
	}
	return nil, false
}

// Read parses and validates the RT Plan of a Part 10 file.
func Read(file *object.File) (*Plan, error) {
	return ReadWithOptions(context.Background(), file, ReadOptions{})
}

// ReadDataset reads an admitted conventional, brachy or Ion plan.
func ReadDataset(obj *object.Object) (*Plan, error) {
	return ReadDatasetWithOptions(context.Background(), obj, ReadOptions{})
}
