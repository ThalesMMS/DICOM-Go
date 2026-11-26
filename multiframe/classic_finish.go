package multiframe

import (
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dcmtime"
	"github.com/ThalesMMS/dicom-go/dictionary"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/iodmodules"
	"github.com/ThalesMMS/dicom-go/object"
	iodrules "github.com/ThalesMMS/dicom-go/validation/iod"
)

// classicIODs names the PS3.3 IOD of each classic SOP Class.
var classicIODs = map[string]string{
	CTImageStorageUID:  "CT Image",
	MRImageStorageUID:  "MR Image",
	PETImageStorageUID: "Positron Emission Tomography Image",
}

var (
	tagSeriesDate                = core.NewTag(0x0008, 0x0021)
	tagSeriesTime                = core.NewTag(0x0008, 0x0031)
	tagFrameReferenceDateTime    = core.NewTag(0x0018, 0x9151)
	tagFrameAcquisitionDateTime  = core.NewTag(0x0018, 0x9074)
	tagFrameAcquisitionDuration  = core.NewTag(0x0018, 0x9220)
	tagActualFrameDuration       = core.NewTag(0x0018, 0x1242)
	tagDimensionIndexValues      = core.NewTag(0x0020, 0x9157)
	tagTemporalPositionIndex     = core.NewTag(0x0020, 0x9128)
	tagStackID                   = core.NewTag(0x0020, 0x9056)
	tagInStackPositionNumber     = core.NewTag(0x0020, 0x9057)
	tagAcquisitionDuration       = core.NewTag(0x0018, 0x9073)
	tagCorrectedImage            = core.NewTag(0x0028, 0x0051)
	tagSeriesType                = core.NewTag(0x0054, 0x1000)
	tagCountsSource              = core.NewTag(0x0054, 0x1002)
	tagDecayCorrection           = core.NewTag(0x0054, 0x1102)
	tagDecayCorrected            = core.NewTag(0x0018, 0x9758)
	tagDecayCorrectionDateTime   = core.NewTag(0x0018, 0x9701)
	tagPETFrameCorrectionFactors = core.NewTag(0x0018, 0x9736)
	tagNumberOfSlices            = core.NewTag(0x0054, 0x0081)
	tagNumberOfTimeSlices        = core.NewTag(0x0054, 0x0101)
	tagFrameReferenceTime        = core.NewTag(0x0054, 0x1300)
	tagImageIndex                = core.NewTag(0x0054, 0x1330)
	tagDecayFactor               = core.NewTag(0x0054, 0x1321)
	tagCardiacSynchronization    = core.NewTag(0x0018, 0x9118)
	tagRespiratorySynchronizaton = core.NewTag(0x0020, 0x9253)
)

// enhancedOnlyTags are leaves of Enhanced Functional Groups and Modules
// that a classic image has no place for; they are dropped unless the
// classic IOD lists them.
var enhancedOnlyTags = []core.Tag{
	tagPixelPresentation, tagVolumetricProperties, tagVolumeBasedCalculation, tagComplexImageComponent, tagAcquisitionContrast,
	tagFrameLaterality, tagFrameAcquisitionNumber, tagFrameReferenceDateTime, tagFrameAcquisitionDateTime, tagFrameAcquisitionDuration,
	tagDimensionIndexValues, tagTemporalPositionIndex, tagStackID, tagInStackPositionNumber,
	tagContentQualification, tagPresentationLUTShape, tagAcquisitionContextSequence, tagAcquisitionDuration,
	tagRescaleType,
}

// pet correction flags of Enhanced PET and the classic Corrected Image
// terms they stand for.
var petCorrections = []struct {
	flag core.Tag
	term string
}{
	{core.NewTag(0x0018, 0x9758), "DECY"}, {core.NewTag(0x0018, 0x9759), "ATTN"}, {core.NewTag(0x0018, 0x9760), "SCAT"},
	{core.NewTag(0x0018, 0x9761), "DTIM"}, {core.NewTag(0x0018, 0x9762), "MOTN"}, {core.NewTag(0x0018, 0x9763), "PMOT"},
	{core.NewTag(0x0018, 0x9764), "CLN"}, {core.NewTag(0x0018, 0x9765), "RAN"}, {core.NewTag(0x0018, 0x9766), "RADL"},
	{core.NewTag(0x0018, 0x9767), "DCAL"}, {core.NewTag(0x0018, 0x9768), "NORM"},
}

// petUnits are the Defined Terms of Units (0054,1001).
var petUnits = map[string]bool{
	"CNTS": true, "NONE": true, "CM2": true, "CM2ML": true, "PCNT": true, "CPS": true, "BQML": true, "MGMINML": true,
	"UMOLMINML": true, "MLMING": true, "MLG": true, "1CM": true, "UMOLML": true, "PROPCNTS": true, "PROPCPS": true,
	"MLMINML": true, "MLML": true, "GML": true, "STDDEV": true,
}

// finishClassic completes a flattened classic image: the PET Series and PET
// Image attributes an Enhanced PET image keeps elsewhere, the Enhanced-only
// leaves dropped, and the Type 2 Attributes of the classic IOD's mandatory
// Modules written empty when missing.
func finishClassic(dataset, source *object.Object, frameIndex, frames int, targetClassUID string) error {
	if targetClassUID == PETImageStorageUID {
		if err := finishClassicPET(dataset, source, frameIndex, frames); err != nil {
			return err
		}
	}
	iod := classicIODs[targetClassUID]
	top, _ := iodmodules.TopLevelTags(iod)
	drop := append([]core.Tag(nil), enhancedOnlyTags...)
	for _, c := range petCorrections {
		drop = append(drop, c.flag)
	}
	for _, tag := range drop {
		if _, keep := top[tag]; !keep {
			dataset.Remove(tag)
		}
	}
	emitType2(dataset, iod)
	return nil
}

// finishClassicPET maps Enhanced PET to the Type 1 attributes of the PET
// Series and PET Image Modules, keeping any the image already has (a Legacy
// Converted source carries the classic ones).
func finishClassicPET(dataset, source *object.Object, frameIndex, frames int) error {
	has := func(tag core.Tag) bool { v, ok := dataset.GetString(tag); return ok && cleanText(v) != "" }
	put := func(tag core.Tag, vr core.VR, value string) {
		if !has(tag) {
			dataset.Put(str(tag, vr, value))
		}
	}
	offset := stringOf(dataset, core.NewTag(0x0008, 0x0201))
	if !has(tagSeriesDate) || !has(tagSeriesTime) {
		seriesTime := enhancedDateTime(dataset, offset, tagAcquisitionDateTime, tagFrameAcquisitionDateTime, tagFrameReferenceDateTime)
		if seriesTime.IsZero() {
			seriesTime = enhancedDateTime(dataset, offset, tagContentDate)
		}
		if !seriesTime.IsZero() {
			if offset == "" {
				offset = seriesTime.Format("-0700")
			} else if local, err := parseDICOMDateTime(seriesTime.Format("20060102") + offset); err == nil {
				seriesTime = seriesTime.In(local.Location())
			}
			put(tagSeriesDate, core.VRDA, seriesTime.Format("20060102"))
			put(tagSeriesTime, core.VRTM, seriesTime.Format("150405.999999"))
		}
	}
	reference := enhancedDateTime(dataset, offset, tagFrameReferenceDateTime, tagFrameAcquisitionDateTime, tagAcquisitionDateTime)
	units, _ := dataset.GetString(tagRescaleType)
	if units = cleanText(units); !petUnits[units] {
		units = "NONE"
	}
	put(tagUnits, core.VRCS, units)
	put(tagCountsSource, core.VRCS, "EMISSION")
	temporal := temporalPositions(source)
	if !has(tagSeriesType) {
		kind := "STATIC"
		switch {
		case source.Has(tagCardiacSynchronization) || source.Has(tagRespiratorySynchronizaton):
			kind = "GATED"
		case temporal > 1:
			kind = "DYNAMIC"
		}
		dataset.Put(stringsElement(tagSeriesType, core.VRCS, []string{kind, "IMAGE"}))
	}
	if values, _ := dataset.GetStrings(tagSeriesType); len(values) > 0 && cleanText(values[0]) == "DYNAMIC" {
		putUS16(dataset, tagNumberOfTimeSlices, temporal)
	}
	if !dataset.Has(tagNumberOfSlices) {
		putUS16(dataset, tagNumberOfSlices, max(1, frames/max(1, temporal)))
	}
	if !dataset.Has(tagCorrectedImage) {
		var terms []string
		for _, c := range petCorrections {
			if v, _ := dataset.GetString(c.flag); cleanText(v) == "YES" {
				terms = append(terms, c.term)
			}
		}
		if len(terms) > 0 {
			dataset.Put(stringsElement(tagCorrectedImage, core.VRCS, terms))
		}
	}
	if stringOf(dataset, tagDecayCorrected) == "YES" && (!has(tagDecayCorrection) || stringOf(dataset, tagDecayCorrection) == "NONE") {
		decayValue := stringOf(dataset, tagDecayCorrectionDateTime)
		if !strings.ContainsAny(decayValue, "+-") {
			decayValue += offset
		}
		decay, decayErr := parseDICOMDateTime(decayValue)
		start, startErr := parseDICOMDateTime(stringOf(dataset, tagSeriesDate) + stringOf(dataset, tagSeriesTime) + offset)
		if decayErr != nil || startErr != nil || !has(tagSeriesDate) || !has(tagSeriesTime) {
			return fmt.Errorf("%w: cannot derive Decay Correction from Decay Correction DateTime and series start", ErrNonconformant)
		}
		correction := "ADMIN"
		if decay.Equal(start) {
			correction = "START"
		}
		dataset.Put(str(tagDecayCorrection, core.VRCS, correction))
	} else {
		put(tagDecayCorrection, core.VRCS, "NONE")
	}
	if !has(tagFrameReferenceTime) {
		elapsed := 0.0
		if seriesDate, seriesTime := stringOf(dataset, tagSeriesDate), stringOf(dataset, tagSeriesTime); !reference.IsZero() && seriesDate != "" {
			if start, err := parseDICOMDateTime(seriesDate + seriesTime + offset); err == nil {
				elapsed = float64(reference.Sub(start).Milliseconds())
			}
		}
		if err := dataset.SetDS(tagFrameReferenceTime, math.Max(elapsed, 0)); err != nil {
			dataset.Put(str(tagFrameReferenceTime, core.VRDS, "0"))
		}
	}
	if !dataset.Has(tagImageIndex) {
		putUS16(dataset, tagImageIndex, frameIndex+1)
	}
	if acquired := enhancedDateTime(dataset, offset, tagFrameAcquisitionDateTime); !acquired.IsZero() {
		put(core.NewTag(0x0008, 0x0022), core.VRDA, acquired.Format("20060102"))
		put(core.NewTag(0x0008, 0x0032), core.VRTM, acquired.Format("150405"))
	}
	if ms, ok := numberValue(dataset, tagFrameAcquisitionDuration); ok {
		put(tagActualFrameDuration, core.VRIS, strconv.Itoa(int(math.Round(ms))))
	}
	if correction := stringOf(dataset, tagDecayCorrection); correction != "NONE" {
		// Functional-group flattening carries the frame's PET Frame
		// Correction Factors macro into this data set. Never invent a factor.
		if !has(tagDecayFactor) {
			return fmt.Errorf("%w: Decay Factor is required for decay-corrected PET", ErrNonconformant)
		}
	}
	return nil
}

// temporalPositions counts the distinct Temporal Position Index values of
// an Enhanced source (1 without them).
func temporalPositions(source *object.Object) int {
	seen := map[string]bool{}
	items, _ := source.GetSequence(tagPerFrameFunctionalGroupsSequence)
	for _, item := range items {
		contents, _ := item.GetSequence(tagFrameContentSequence)
		for _, content := range contents {
			if element, ok := content.Get(tagTemporalPositionIndex); ok {
				if raw, ok := element.RawBytes(); ok {
					seen[string(raw)] = true
				} else if v, ok := content.GetString(tagTemporalPositionIndex); ok {
					seen[cleanText(v)] = true
				}
			}
		}
	}
	return max(1, len(seen))
}

// numberValue reads a single FD, FL or decimal-string value.
func numberValue(dataset *object.Object, tag core.Tag) (float64, bool) {
	element, ok := dataset.Get(tag)
	if !ok {
		return 0, false
	}
	if raw, ok := element.RawBytes(); ok {
		switch {
		case element.VR() == core.VRFD && len(raw) == 8:
			return math.Float64frombits(binary.LittleEndian.Uint64(raw)), true
		case element.VR() == core.VRFL && len(raw) == 4:
			return float64(math.Float32frombits(binary.LittleEndian.Uint32(raw))), true
		}
	}
	values, err := dataset.GetFloats(tag)
	if err != nil || len(values) != 1 {
		return 0, false
	}
	return values[0], true
}

func putUS16(dataset *object.Object, tag core.Tag, value int) {
	dataset.SetUS(tag, uint16(min(max(value, 0), math.MaxUint16)))
}

func stringOf(dataset *object.Object, tag core.Tag) string {
	v, _ := dataset.GetString(tag)
	return cleanText(v)
}

// enhancedDateTime is the first of the DT (or DA) attributes that parses,
// using offset when the value does not carry its own UTC offset.
func enhancedDateTime(dataset *object.Object, offset string, tags ...core.Tag) time.Time {
	for _, tag := range tags {
		value := stringOf(dataset, tag)
		if !strings.ContainsAny(value, "+-") {
			value += offset
		}
		if t, err := parseDICOMDateTime(value); err == nil {
			return t
		}
	}
	return time.Time{}
}

// parseDICOMDateTime reads a DT value (or a DA date), keeping fractions and offsets.
func parseDICOMDateTime(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if len(value) < 8 {
		return time.Time{}, strconv.ErrSyntax
	}
	parsed, err := dcmtime.ParseDatetime(value)
	return parsed.Time, err
}

// emitType2 writes, empty, the top-level Attributes the data set lacks that
// are Type 2 in the IOD's mandatory Modules, or Type 2C with a condition
// validation/iod evaluates true (Repetition Time of an MR Image that is not
// echo planar).
func emitType2(dataset *object.Object, iod string) {
	modules, _ := iodmodules.Modules(iod)
	for _, use := range modules {
		if use.Usage != "M" {
			continue
		}
		attributes, _ := iodmodules.Attributes(use.Module)
		for _, attribute := range attributes {
			if attribute.Type == "2" {
				putEmpty(dataset, core.NewTag(uint16(attribute.Tag>>16), uint16(attribute.Tag)))
			}
		}
	}
	report, err := iodrules.Validate(dataset, iodrules.Options{IOD: iod})
	if err != nil {
		return
	}
	for _, f := range report.Errors() {
		if f.Kind == iodrules.MissingType2C && f.Path == f.Tag.String() {
			putEmpty(dataset, f.Tag)
		}
	}
}

// putEmpty writes tag with no value (an empty Sequence for SQ) unless the
// data set has it.
func putEmpty(dataset *object.Object, tag core.Tag) {
	if dataset.Has(tag) {
		return
	}
	if vr := dictionary.LookupVR(std.Dictionary, tag); vr == core.VRSQ {
		dataset.Put(seq(tag))
	} else {
		dataset.Put(core.NewRawElement(tag, vr, nil))
	}
}
