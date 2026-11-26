package rtplan

// BrachySource describes a declared source, not a calibrated dose model.
type BrachySource struct {
	Number                                           int
	Type, SerialNumber, IsotopeName, StrengthUnits   string
	IsotopeHalfLife, ReferenceAirKermaRate, Strength Attribute[float64]
	StrengthReferenceDate, StrengthReferenceTime     string
}

// BrachyChannel positions are relative to the distal-most possible source
// position, in mm. Optional CP 3D positions use patient coordinates in mm.
type BrachyChannel struct {
	Number, SourceNumber                          int
	Movement                                      string
	Length, TotalTime, StepSize, PulseInterval    Attribute[float64]
	NumberOfPulses, TransferTubeNumber            Attribute[int]
	TransferTubeLength, FinalCumulativeTimeWeight Attribute[float64]
	ControlPoints                                 []BrachyControlPoint
}
type BrachyControlPoint struct {
	Index                                 int
	CumulativeTimeWeight                  Attribute[float64]
	RelativePosition                      float64
	Position3D, Orientation               []float64
	Position3DPresent, OrientationPresent bool
}
