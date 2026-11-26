package printmgmt

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
)

// ErrInvalidAttributes reports an attribute list outside the supported
// subset.
var ErrInvalidAttributes = errors.New("printmgmt: invalid attribute list")

// StatusError carries a DIMSE status and the attributes involved (tags only).
type StatusError struct {
	Status       uint16
	Tags         []core.Tag
	ErrorComment string
	Err          error
}

func (e *StatusError) Error() string {
	var tags []string
	for _, tag := range e.Tags {
		tags = append(tags, tag.String())
	}
	return fmt.Sprintf("%v (status %04X %s)", e.Err, e.Status, strings.Join(tags, " "))
}

func (e *StatusError) Unwrap() error { return e.Err }

func invalid(tags ...core.Tag) error {
	return &StatusError{Status: StatusInvalidAttributeValue, Tags: tags, Err: ErrInvalidAttributes}
}

// attributeReader reads attributes and collects the tags it did not use, so
// unsupported optional attributes can be reported with warning 0107H.
type attributeReader struct {
	ds   *object.Object
	used map[core.Tag]bool
}

func newReader(ds *object.Object) *attributeReader {
	return &attributeReader{ds: ds, used: map[core.Tag]bool{}}
}

func (r *attributeReader) has(tag core.Tag) bool {
	_, ok := r.ds.Get(tag)
	return ok
}

func (r *attributeReader) text(tag core.Tag) (string, bool, error) {
	r.used[tag] = true
	if !r.has(tag) {
		return "", false, nil
	}
	value, err := r.ds.LookupString(tag)
	if err != nil {
		values, strErr := r.ds.LookupStrings(tag)
		if strErr != nil {
			return "", true, invalid(tag)
		}
		value = strings.Join(values, `\`)
	}
	return strings.TrimSpace(value), true, nil
}

func (r *attributeReader) enum(tag core.Tag, allowed ...string) (string, error) {
	value, ok, err := r.text(tag)
	if err != nil || !ok || value == "" {
		return value, err
	}
	for _, candidate := range allowed {
		if value == candidate {
			return value, nil
		}
	}
	return "", invalid(tag)
}

func uint16Of(ds *object.Object, tag core.Tag) (int, bool, error) {
	element, ok := ds.Get(tag)
	if !ok {
		return 0, false, nil
	}
	switch value := element.Value.(type) {
	case core.Uint16Value:
		if len(value) == 1 {
			return int(value[0]), true, nil
		}
	case core.RawValue:
		if len(value) == 2 {
			return int(ds.ValueByteOrder().Uint16(value)), true, nil
		}
		if len(value) == 0 {
			return 0, false, nil
		}
	case nil:
		return 0, false, nil
	}
	return 0, true, invalid(tag)
}

func (r *attributeReader) number(tag core.Tag) (int, bool, error) {
	r.used[tag] = true
	return uint16Of(r.ds, tag)
}

// ignored lists attributes present but not used by the subset.
func (r *attributeReader) ignored() []core.Tag {
	var tags []core.Tag
	for _, element := range r.ds.Elements() {
		if !r.used[element.Tag()] {
			tags = append(tags, element.Tag())
		}
	}
	return tags
}

var (
	filmSizes = []string{"8INX10IN", "8_5INX11IN", "10INX12IN", "10INX14IN", "11INX14IN", "11INX17IN", "14INX14IN", "14INX17IN", "24CMX24CM", "24CMX30CM", "A4", "A3"}
	mediums   = []string{"PAPER", "CLEAR FILM", "BLUE FILM", "MAMMO CLEAR FILM", "MAMMO BLUE FILM"}
)

// parseFilmSession reads a Basic Film Session N-CREATE or N-SET list
// (Table H.4-2). Memory Allocation is reported for warning B600H.
func parseFilmSession(ds *object.Object, current FilmSessionPresentation, maxCopies int) (FilmSessionPresentation, bool, []core.Tag, error) {
	out := current
	if ds == nil {
		return out, false, nil, nil
	}
	r := newReader(ds)
	r.used[core.NewTag(0x0008, 0x0005)] = true
	if copies, ok, err := r.text(TagNumberOfCopies); err != nil {
		return out, false, nil, err
	} else if ok && copies != "" {
		n, convErr := strconv.Atoi(copies)
		if convErr != nil || n < 1 || n > maxCopies {
			return out, false, nil, invalid(TagNumberOfCopies)
		}
		out.NumberOfCopies = n
	}
	var err error
	if value, e := r.enum(TagPrintPriority, "HIGH", "MED", "LOW"); e != nil {
		return out, false, nil, e
	} else if value != "" {
		out.PrintPriority = value
	}
	if value, e := r.enum(TagMediumType, mediums...); e != nil {
		return out, false, nil, e
	} else if value != "" {
		out.MediumType = value
	}
	destination, ok, err := r.text(TagFilmDestination)
	if err != nil {
		return out, false, nil, err
	}
	if ok && destination != "" {
		if destination != "MAGAZINE" && destination != "PROCESSOR" && !strings.HasPrefix(destination, "BIN_") {
			return out, false, nil, invalid(TagFilmDestination)
		}
		out.FilmDestination = destination
	}
	if label, ok, err := r.text(TagFilmSessionLabel); err != nil {
		return out, false, nil, err
	} else if ok {
		out.Label = label
	}
	if owner, ok, err := r.text(TagOwnerID); err != nil {
		return out, false, nil, err
	} else if ok {
		out.OwnerID = owner
	}
	memory := r.has(TagMemoryAllocation)
	r.used[TagMemoryAllocation] = true
	return out, memory, r.ignored(), nil
}

// parseDisplayFormat accepts STANDARD\C,R only. Columns and rows are
// unsigned decimal integers, each bounded by maxBoxes before their product is
// formed, so no layout can wrap the product (for example 2^62 x 4 on 64-bit
// or 65536 x 65536 on 32-bit) past the image box limit.
func parseDisplayFormat(value string, maxBoxes int) (int, int, error) {
	kind, layout, ok := strings.Cut(value, `\`)
	if !ok || kind != "STANDARD" {
		return 0, 0, invalid(TagImageDisplayFormat)
	}
	columnsText, rowsText, ok := strings.Cut(layout, ",")
	if !ok {
		return 0, 0, invalid(TagImageDisplayFormat)
	}
	columns, okC := boundedCount(columnsText, maxBoxes)
	rows, okR := boundedCount(rowsText, maxBoxes)
	if !okC || !okR || !boxCountWithin(columns, rows, maxBoxes) {
		return 0, 0, invalid(TagImageDisplayFormat)
	}
	return columns, rows, nil
}

// boundedCount parses a decimal count in [1, limit]. Signs, empty values and
// non-digits are refused; digits are accumulated with an overflow check, so
// arbitrarily long numbers are refused without wrapping.
func boundedCount(text string, limit int) (int, bool) {
	text = strings.TrimSpace(text)
	if text == "" || limit < 1 {
		return 0, false
	}
	n := 0
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		digit := int(c - '0')
		if digit > limit || n > (limit-digit)/10 {
			return 0, false
		}
		n = n*10 + digit
	}
	return n, n >= 1
}

// boxCountWithin reports whether columns*rows image boxes fit in maxBoxes,
// without forming a product that could overflow int.
func boxCountWithin(columns, rows, maxBoxes int) bool {
	return columns >= 1 && rows >= 1 && maxBoxes >= 1 && columns <= maxBoxes/rows
}

// parseFilmBoxPresentation reads the presentation attributes of Tables H.4-6
// and H.4-7. Presentation LUT references are refused because the Presentation
// LUT SOP Class is not supported; an ICC Profile is refused for grayscale.
func parseFilmBoxPresentation(r *attributeReader, current FilmBoxPresentation, color bool) (FilmBoxPresentation, error) {
	return parseFilmBoxPresentationNative(r, current, color, false)
}

func parseFilmBoxPresentationNative(r *attributeReader, current FilmBoxPresentation, color, allowLUT bool) (FilmBoxPresentation, error) {
	out := current
	if r.has(TagReferencedPresentationLUT) && !allowLUT {
		return out, invalid(TagReferencedPresentationLUT)
	}
	if r.has(TagICCProfile) && !color {
		return out, invalid(TagICCProfile)
	}
	fields := []struct {
		tag     core.Tag
		target  *string
		allowed []string
	}{
		{TagFilmOrientation, &out.FilmOrientation, []string{"PORTRAIT", "LANDSCAPE"}},
		{TagFilmSizeID, &out.FilmSizeID, filmSizes},
		{TagMagnificationType, &out.MagnificationType, []string{"REPLICATE", "BILINEAR", "CUBIC", "NONE"}},
		{TagTrim, &out.Trim, []string{"YES", "NO"}},
	}
	for _, field := range fields {
		value, err := r.enum(field.tag, field.allowed...)
		if err != nil {
			return out, err
		}
		if value != "" {
			*field.target = value
		}
	}
	for _, field := range []struct {
		tag    core.Tag
		target *string
	}{{TagSmoothingType, &out.SmoothingType}, {TagBorderDensity, &out.BorderDensity}, {TagEmptyImageDensity, &out.EmptyImageDensity}, {TagConfigurationInformation, &out.Configuration}} {
		value, ok, err := r.text(field.tag)
		if err != nil {
			return out, err
		}
		if ok {
			*field.target = value
		}
	}
	for _, field := range []struct {
		tag     core.Tag
		target  *int
		present *bool
	}{{TagMinDensity, &out.MinDensity, &out.MinDensitySet}, {TagMaxDensity, &out.MaxDensity, &out.MaxDensitySet}, {TagIllumination, &out.Illumination, &out.IlluminationSet}, {TagReflectedAmbientLight, &out.ReflectedAmbientLight, &out.ReflectedAmbientLightSet}} {
		value, ok, err := r.number(field.tag)
		if err != nil {
			return out, err
		}
		if ok {
			*field.target = value
			*field.present = true
		}
	}
	if out.MinDensity != 0 && out.MaxDensity != 0 && out.MinDensity >= out.MaxDensity {
		return out, invalid(TagMinDensity, TagMaxDensity)
	}
	return out, nil
}

// parseImage validates a Basic Grayscale or Color Image Sequence item against
// the preformatted pixel constraints: grayscale MONOCHROME1/2 with 8 bits, or
// 12 bits in 16-bit containers; color RGB, 8 bits, Planar Configuration 1;
// unsigned, and Pixel Data of exactly the declared size.
func parseImage(item *object.Object, color bool, maxDimension int, maxBytes int64) (*Image, error) {
	sequence := TagBasicGrayscaleImageSequence
	if color {
		sequence = TagBasicColorImageSequence
	}
	get := func(tag core.Tag) (int, error) {
		value, ok, err := uint16Of(item, tag)
		if err != nil || !ok {
			return 0, invalid(sequence, tag)
		}
		return value, nil
	}
	samples, err := get(TagSamplesPerPixel)
	if err != nil {
		return nil, err
	}
	rows, err := get(TagRows)
	if err != nil {
		return nil, err
	}
	columns, err := get(TagColumns)
	if err != nil {
		return nil, err
	}
	allocated, err := get(TagBitsAllocated)
	if err != nil {
		return nil, err
	}
	stored, err := get(TagBitsStored)
	if err != nil {
		return nil, err
	}
	high, err := get(TagHighBit)
	if err != nil {
		return nil, err
	}
	representation, err := get(TagPixelRepresentation)
	if err != nil {
		return nil, err
	}
	photometric, err := item.LookupString(TagPhotometricInterpretation)
	if err != nil {
		return nil, invalid(sequence, TagPhotometricInterpretation)
	}
	photometric = strings.TrimSpace(photometric)
	if rows < 1 || columns < 1 || rows > maxDimension || columns > maxDimension {
		return nil, invalid(sequence, TagRows, TagColumns)
	}
	if representation != 0 || high != stored-1 {
		return nil, invalid(sequence, TagPixelRepresentation, TagHighBit)
	}
	image := &Image{Color: color, Rows: rows, Columns: columns, Bits: stored, Photometric: photometric}
	bytesPerSample := 1
	if color {
		planar, err := get(TagPlanarConfiguration)
		if err != nil {
			return nil, err
		}
		if samples != 3 || photometric != "RGB" || planar != 1 || allocated != 8 || stored != 8 {
			return nil, invalid(sequence, TagSamplesPerPixel, TagPhotometricInterpretation, TagPlanarConfiguration, TagBitsAllocated)
		}
	} else {
		if samples != 1 || (photometric != "MONOCHROME1" && photometric != "MONOCHROME2") || !(allocated == 8 && stored == 8 || allocated == 16 && stored == 12) {
			return nil, invalid(sequence, TagSamplesPerPixel, TagPhotometricInterpretation, TagBitsAllocated, TagBitsStored)
		}
		bytesPerSample = allocated / 8
	}
	if values, present, err := residentTextValues(item, TagPixelAspectRatio, core.VRIS, 2, 12); err != nil {
		return nil, invalid(sequence, TagPixelAspectRatio)
	} else if present {
		for index, v := range values {
			n, err := strconv.ParseInt(v, 10, 32)
			if err != nil || n < 1 {
				return nil, invalid(sequence, TagPixelAspectRatio)
			} else {
				image.PixelAspectRatio[index] = int(n)
			}
		}
	}
	want := int64(rows) * int64(columns) * int64(samples) * int64(bytesPerSample)
	if want > maxBytes {
		return nil, &StatusError{Status: StatusInsufficientPrinterMemory, Tags: []core.Tag{sequence, TagPixelData}, Err: ErrInvalidAttributes}
	}
	pixels, ok := item.GetRaw(TagPixelData)
	if !ok || (int64(len(pixels)) != want && !(want%2 == 1 && int64(len(pixels)) == want+1)) {
		return nil, invalid(sequence, TagPixelData)
	}
	image.PixelData = append([]byte(nil), pixels[:want]...)
	if item.ValueByteOrder() == binary.BigEndian && bytesPerSample == 2 {
		for i := 0; i+1 < len(image.PixelData); i += 2 {
			image.PixelData[i], image.PixelData[i+1] = image.PixelData[i+1], image.PixelData[i]
		}
	}
	return image, nil
}

func text(tag core.Tag, vr core.VR, value string) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: vr}, Value: core.StringValue{value}}
}

func us(tag core.Tag, value int) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: core.VRUS}, Value: core.Uint16Value{uint16(value)}}
}

func sequenceOf(tag core.Tag, items ...core.DataSet) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: core.VRSQ, Length: core.UndefinedLength, LengthSet: true}, Value: core.SequenceValue{Items: items}}
}

func referenceItem(classUID, instanceUID string) core.DataSet {
	return core.DataSet{Elements: []core.Element{text(TagReferencedSOPClassUID, core.VRUI, classUID), text(TagReferencedSOPInstanceUID, core.VRUI, instanceUID)}}
}

// BuildFilmSession returns a Basic Film Session N-CREATE or N-SET list with
// the non-empty fields of p.
func BuildFilmSession(p FilmSessionPresentation) *object.Object {
	var elements []core.Element
	if p.NumberOfCopies > 0 {
		elements = append(elements, text(TagNumberOfCopies, core.VRIS, strconv.Itoa(p.NumberOfCopies)))
	}
	for _, field := range []struct {
		tag   core.Tag
		vr    core.VR
		value string
	}{{TagSpecificCharacterSet, core.VRCS, p.SpecificCharacterSet}, {TagPrintPriority, core.VRCS, p.PrintPriority}, {TagMediumType, core.VRCS, p.MediumType}, {TagFilmDestination, core.VRCS, p.FilmDestination}, {TagFilmSessionLabel, core.VRLO, p.Label}, {TagOwnerID, core.VRSH, p.OwnerID}} {
		if field.value != "" {
			elements = append(elements, text(field.tag, field.vr, field.value))
		}
	}
	return object.FromElements(elements, std.Dictionary)
}

// BuildFilmBox returns a Basic Film Box N-CREATE list for STANDARD\C,R.
func BuildFilmBox(filmSessionUID string, columns, rows int, p FilmBoxPresentation) *object.Object {
	elements := []core.Element{
		text(TagImageDisplayFormat, core.VRST, fmt.Sprintf(`STANDARD\%d,%d`, columns, rows)),
		sequenceOf(TagReferencedFilmSessionSequence, referenceItem(FilmSessionSOPClassUID, filmSessionUID)),
	}
	for _, field := range []struct {
		tag   core.Tag
		vr    core.VR
		value string
	}{{TagFilmOrientation, core.VRCS, p.FilmOrientation}, {TagFilmSizeID, core.VRCS, p.FilmSizeID}, {TagMagnificationType, core.VRCS, p.MagnificationType}, {TagSmoothingType, core.VRCS, p.SmoothingType}, {TagBorderDensity, core.VRCS, p.BorderDensity}, {TagEmptyImageDensity, core.VRCS, p.EmptyImageDensity}, {TagTrim, core.VRCS, p.Trim}, {TagConfigurationInformation, core.VRST, p.Configuration}} {
		if field.value != "" {
			elements = append(elements, text(field.tag, field.vr, field.value))
		}
	}
	if p.AnnotationDisplayFormatID != "" {
		elements = append(elements, text(TagAnnotationDisplayFormatID, core.VRCS, p.AnnotationDisplayFormatID))
	}
	if p.PresentationLUTUID != "" {
		elements = append(elements, presentationLUTReference(p.PresentationLUTUID))
	}
	if p.MinDensitySet || p.MinDensity > 0 {
		elements = append(elements, us(TagMinDensity, p.MinDensity))
	}
	if p.MaxDensitySet || p.MaxDensity > 0 {
		elements = append(elements, us(TagMaxDensity, p.MaxDensity))
	}
	if p.IlluminationSet || p.Illumination > 0 {
		elements = append(elements, us(TagIllumination, p.Illumination))
	}
	if p.ReflectedAmbientLightSet || p.ReflectedAmbientLight > 0 {
		elements = append(elements, us(TagReflectedAmbientLight, p.ReflectedAmbientLight))
	}
	return object.FromElements(elements, std.Dictionary)
}

// BuildImageBox returns an Image Box N-SET list. A nil image erases the
// position (zero-length image sequence, PS3.4 H.4.3.1.2.1.3).
func BuildImageBox(position int, image *Image, polarity string) (*object.Object, error) {
	sequence := TagBasicGrayscaleImageSequence
	if image != nil && image.Color {
		sequence = TagBasicColorImageSequence
	}
	elements := []core.Element{us(TagImageBoxPosition, position)}
	if polarity != "" {
		elements = append(elements, text(TagPolarity, core.VRCS, polarity))
	}
	if image == nil {
		elements = append(elements, sequenceOf(sequence))
		return object.FromElements(elements, std.Dictionary), nil
	}
	samples, allocated, planar := 1, 8, -1
	if image.Color {
		samples, planar = 3, 1
	} else if image.Bits == 12 {
		allocated = 16
	}
	vr := core.VROB
	if allocated == 16 {
		vr = core.VROW
	}
	item := core.DataSet{Elements: []core.Element{
		us(TagSamplesPerPixel, samples),
		text(TagPhotometricInterpretation, core.VRCS, image.Photometric),
		us(TagRows, image.Rows), us(TagColumns, image.Columns),
		us(TagBitsAllocated, allocated), us(TagBitsStored, image.Bits), us(TagHighBit, image.Bits-1), us(TagPixelRepresentation, 0),
		core.NewRawElement(TagPixelData, vr, image.PixelData),
	}}
	if planar >= 0 {
		item.Elements = append(item.Elements[:2], append([]core.Element{us(TagPlanarConfiguration, planar)}, item.Elements[2:]...)...)
	}
	if image.PixelAspectRatio != [2]int{} && image.PixelAspectRatio != [2]int{1, 1} {
		item.Elements = append(item.Elements, core.Element{Header: core.ElementHeader{Tag: TagPixelAspectRatio, VR: core.VRIS}, Value: core.StringValue{strconv.Itoa(image.PixelAspectRatio[0]), strconv.Itoa(image.PixelAspectRatio[1])}})
	}
	elements = append(elements, sequenceOf(sequence, item))
	ds := object.FromElements(elements, std.Dictionary)
	items, _ := ds.GetSequence(sequence)
	if _, err := parseImage(items[0], image.Color, 1<<16-1, 1<<40); err != nil {
		return nil, err
	}
	return ds, nil
}
