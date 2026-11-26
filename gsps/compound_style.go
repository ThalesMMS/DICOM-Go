package gsps

import (
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
)

var (
	tagCompoundFontName      = core.NewTag(0x0070, 0x0227)
	tagCompoundFontNameType  = core.NewTag(0x0070, 0x0228)
	tagCompoundCSSFont       = core.NewTag(0x0070, 0x0229)
	tagCompoundTextStyle     = core.NewTag(0x0070, 0x0231)
	tagCompoundLineStyle     = core.NewTag(0x0070, 0x0232)
	tagCompoundFillStyle     = core.NewTag(0x0070, 0x0233)
	tagCompoundTextColor     = core.NewTag(0x0070, 0x0241)
	tagCompoundHorizontal    = core.NewTag(0x0070, 0x0242)
	tagCompoundVertical      = core.NewTag(0x0070, 0x0243)
	tagCompoundShadowStyle   = core.NewTag(0x0070, 0x0244)
	tagCompoundShadowOffsetX = core.NewTag(0x0070, 0x0245)
	tagCompoundShadowOffsetY = core.NewTag(0x0070, 0x0246)
	tagCompoundShadowColor   = core.NewTag(0x0070, 0x0247)
	tagCompoundUnderlined    = core.NewTag(0x0070, 0x0248)
	tagCompoundBold          = core.NewTag(0x0070, 0x0249)
	tagCompoundItalic        = core.NewTag(0x0070, 0x0250)
	tagCompoundPatternOn     = core.NewTag(0x0070, 0x0251)
	tagCompoundPatternOff    = core.NewTag(0x0070, 0x0252)
	tagCompoundLineThickness = core.NewTag(0x0070, 0x0253)
	tagCompoundLineDashing   = core.NewTag(0x0070, 0x0254)
	tagCompoundLinePattern   = core.NewTag(0x0070, 0x0255)
	tagCompoundFillPattern   = core.NewTag(0x0070, 0x0256)
	tagCompoundFillMode      = core.NewTag(0x0070, 0x0257)
	tagCompoundShadowOpacity = core.NewTag(0x0070, 0x0258)
	tagCompoundOnOpacity     = core.NewTag(0x0070, 0x0284)
	tagCompoundOffOpacity    = core.NewTag(0x0070, 0x0285)
)

const compoundFillPatternBytes = 128

// CompoundLineStyle describes the foreground, dashing and shadow of a
// compound graphic. Thickness and shadow offsets use the graphic's units.
type CompoundLineStyle struct {
	OnColor, OffColor, ShadowColor              *[3]uint16
	OnOpacity, OffOpacity, Thickness            *float64
	ShadowOffsetX, ShadowOffsetY, ShadowOpacity *float64
	Dashing, Shadow                             string
	Pattern                                     *uint32
}

// CompoundFillStyle describes a solid or stippled fill. Pattern is a 32×32
// bitmap in display pixels, with the most significant bit at the left.
type CompoundFillStyle struct {
	OnColor, OffColor     *[3]uint16
	OnOpacity, OffOpacity *float64
	Mode                  string
	Pattern               []byte
}

// CompoundTextStyle describes the text style for a compound graphic.
type CompoundTextStyle struct {
	FontName, FontNameType, CSSFontName         string
	HorizontalAlignment, VerticalAlignment      string
	Shadow                                      string
	Color, ShadowColor                          *[3]uint16
	ShadowOffsetX, ShadowOffsetY, ShadowOpacity *float64
	Underlined, Bold, Italic                    *bool
}

// compoundStyleReader retains fields whose wire encoding was valid so that
// inspection of the compound is still possible when a fallback is selected.
type compoundStyleReader struct {
	item *object.Object
	ok   bool
}

func readCompoundStyleItem(obj *object.Object, tag core.Tag) (compoundStyleReader, bool) {
	element, present := obj.Get(tag)
	if !present {
		return compoundStyleReader{}, true
	}
	if element.VR() != core.VRSQ || obj.HasDuplicateParsedTag(tag) {
		return compoundStyleReader{}, false
	}
	seq, valid := element.Value.(core.SequenceValue)
	if !valid || len(seq.Items) != 1 {
		return compoundStyleReader{}, false
	}
	items, _ := obj.GetSequence(tag)
	return compoundStyleReader{item: items[0], ok: true}, true
}

func (r *compoundStyleReader) text(tag core.Tag, vr core.VR) string {
	// Leave room for ISO 2022 escapes around a 64-character LO.
	maxBytes := core.Length(512)
	if vr == core.VRCS {
		maxBytes = 16
	}
	if elem, present := r.item.Get(tag); present && elem.Length() > maxBytes {
		r.ok = false
		return ""
	}
	if tag == tagCompoundFontName {
		// The optional Font Name can be explicitly empty.
		if elem, present := r.item.Get(tag); present && elem.Length() == 0 &&
			elem.VR() == vr && !r.item.HasDuplicateParsedTag(tag) {
			return ""
		}
	}
	value, ok := readCompoundString(r.item, tag, vr)
	r.ok = r.ok && ok
	return value
}

func (r *compoundStyleReader) color(tag core.Tag) *[3]uint16 {
	element, present := r.item.Get(tag)
	if !present {
		return nil
	}
	if element.VR() != core.VRUS || r.item.HasDuplicateParsedTag(tag) || element.Length() != 6 {
		r.ok = false
		return nil
	}
	values, err := r.item.GetInts(tag)
	if err != nil || len(values) != 3 {
		r.ok = false
		return nil
	}
	return &[3]uint16{uint16(values[0]), uint16(values[1]), uint16(values[2])}
}

func (r *compoundStyleReader) float(tag core.Tag) *float64 {
	value, ok := readCompoundFloat(r.item, tag, core.VRFL)
	r.ok = r.ok && ok
	return value
}

func (r *compoundStyleReader) word(tag core.Tag) *uint32 {
	value, ok := readCompoundUint32(r.item, tag)
	r.ok = r.ok && ok
	return value
}

func (r *compoundStyleReader) yn(tag core.Tag) *bool {
	value, ok := readCompoundBool(r.item, tag)
	r.ok = r.ok && ok
	return value
}

func (r *compoundStyleReader) pattern() []byte {
	element, present := r.item.Get(tagCompoundFillPattern)
	if !present {
		return nil
	}
	raw, ok := element.RawBytes()
	if element.VR() != core.VROB || r.item.HasDuplicateParsedTag(tagCompoundFillPattern) || !ok || len(raw) != compoundFillPatternBytes {
		r.ok = false
		return nil
	}
	return append([]byte(nil), raw...)
}

func readCompoundStyles(item *object.Object, c *CompoundGraphic) bool {
	line, lineOK := readCompoundStyleItem(item, tagCompoundLineStyle)
	if line.item != nil {
		c.LineStyle = &CompoundLineStyle{
			OnColor: line.color(tagCompoundPatternOn), OffColor: line.color(tagCompoundPatternOff),
			OnOpacity: line.float(tagCompoundOnOpacity), OffOpacity: line.float(tagCompoundOffOpacity),
			Thickness: line.float(tagCompoundLineThickness), Dashing: line.text(tagCompoundLineDashing, core.VRCS),
			Pattern: line.word(tagCompoundLinePattern), Shadow: line.text(tagCompoundShadowStyle, core.VRCS),
			ShadowColor: line.color(tagCompoundShadowColor), ShadowOpacity: line.float(tagCompoundShadowOpacity),
			ShadowOffsetX: line.float(tagCompoundShadowOffsetX), ShadowOffsetY: line.float(tagCompoundShadowOffsetY),
		}
		lineOK = line.ok
	}
	fill, fillOK := readCompoundStyleItem(item, tagCompoundFillStyle)
	if fill.item != nil {
		c.FillStyle = &CompoundFillStyle{
			OnColor: fill.color(tagCompoundPatternOn), OffColor: fill.color(tagCompoundPatternOff),
			OnOpacity: fill.float(tagCompoundOnOpacity), OffOpacity: fill.float(tagCompoundOffOpacity),
			Mode: fill.text(tagCompoundFillMode, core.VRCS), Pattern: fill.pattern(),
		}
		fillOK = fill.ok
	}
	text, textOK := readCompoundStyleItem(item, tagCompoundTextStyle)
	if text.item != nil {
		c.TextStyle = &CompoundTextStyle{
			FontName: text.text(tagCompoundFontName, core.VRLO), FontNameType: text.text(tagCompoundFontNameType, core.VRCS),
			CSSFontName: text.text(tagCompoundCSSFont, core.VRLO), Color: text.color(tagCompoundTextColor),
			HorizontalAlignment: text.text(tagCompoundHorizontal, core.VRCS), VerticalAlignment: text.text(tagCompoundVertical, core.VRCS),
			Shadow: text.text(tagCompoundShadowStyle, core.VRCS), ShadowColor: text.color(tagCompoundShadowColor),
			ShadowOffsetX: text.float(tagCompoundShadowOffsetX), ShadowOffsetY: text.float(tagCompoundShadowOffsetY),
			ShadowOpacity: text.float(tagCompoundShadowOpacity),
			Underlined:    text.yn(tagCompoundUnderlined), Bold: text.yn(tagCompoundBold), Italic: text.yn(tagCompoundItalic),
		}
		textOK = text.ok
	}
	return lineOK && fillOK && textOK && validateCompoundStyles(c) == nil
}

func validateCompoundStyles(c *CompoundGraphic) error {
	if line := c.LineStyle; line != nil {
		if line.Thickness == nil || !validStyleFloat(line.Thickness) || *line.Thickness <= 0 || float32(*line.Thickness) == 0 {
			return fmt.Errorf("%w: line style requires a positive FL thickness", ErrInvalidObject)
		}
		switch line.Dashing {
		case "", "SOLID":
		case "DASHED":
			if line.Pattern == nil {
				return fmt.Errorf("%w: dashed line style requires a line pattern", ErrInvalidObject)
			}
		default:
			return fmt.Errorf("%w: unknown line dashing style", ErrInvalidObject)
		}
		if !validStyleOpacity(line.OnOpacity) || !validStyleOpacity(line.OffOpacity) ||
			!validStyleShadow(line.Shadow, line.ShadowColor, line.ShadowOffsetX, line.ShadowOffsetY, line.ShadowOpacity) {
			return fmt.Errorf("%w: invalid line style opacity or shadow", ErrInvalidObject)
		}
	}
	if fill := c.FillStyle; fill != nil {
		switch fill.Mode {
		case "", "SOLID":
		case "STIPPELED":
			if len(fill.Pattern) != compoundFillPatternBytes {
				return fmt.Errorf("%w: stippled fill requires a 128-byte fill pattern", ErrInvalidObject)
			}
		default:
			return fmt.Errorf("%w: unknown fill mode", ErrInvalidObject)
		}
		if (len(fill.Pattern) != 0 && len(fill.Pattern) != compoundFillPatternBytes) ||
			!validStyleOpacity(fill.OnOpacity) || !validStyleOpacity(fill.OffOpacity) {
			return fmt.Errorf("%w: invalid fill pattern or opacity", ErrInvalidObject)
		}
	}
	if text := c.TextStyle; text != nil {
		if !validStyleFontName(text.FontName) || !validStyleFontName(text.CSSFontName) ||
			(text.FontName != "" && text.FontNameType == "") || (text.FontNameType != "" && !validCS(text.FontNameType)) {
			return fmt.Errorf("%w: invalid text style font specification", ErrInvalidObject)
		}
		switch text.HorizontalAlignment {
		case "", "LEFT", "CENTER", "RIGHT":
		default:
			return fmt.Errorf("%w: invalid text style horizontal alignment", ErrInvalidObject)
		}
		switch text.VerticalAlignment {
		case "", "TOP", "CENTER", "BOTTOM":
		default:
			return fmt.Errorf("%w: invalid text style vertical alignment", ErrInvalidObject)
		}
		if !validStyleShadow(text.Shadow, text.ShadowColor, text.ShadowOffsetX, text.ShadowOffsetY, text.ShadowOpacity) {
			return fmt.Errorf("%w: invalid text style shadow", ErrInvalidObject)
		}
	}
	return nil
}

func validStyleFontName(value string) bool {
	return utf8.ValidString(value) && utf8.RuneCountInString(value) <= 64 &&
		strings.IndexFunc(value, func(r rune) bool { return r == '\\' || r < ' ' || r == 0x7f }) < 0
}

func validStyleFloat(value *float64) bool {
	return value == nil || (!math.IsNaN(*value) && math.Abs(*value) <= math.MaxFloat32)
}

func validStyleOpacity(value *float64) bool {
	return value == nil || unitInterval(*value)
}

func validStyleShadow(style string, color *[3]uint16, x, y, opacity *float64) bool {
	switch style {
	case "", "OFF":
	case "NORMAL", "OUTLINED":
		if color == nil || x == nil || y == nil || opacity == nil {
			return false
		}
	default:
		return false
	}
	return validStyleFloat(x) && validStyleFloat(y) && validStyleOpacity(opacity)
}

func compoundStyleElements(c *CompoundGraphic) []core.Element {
	var out []core.Element
	if s := c.LineStyle; s != nil {
		line := compoundStyleWriter{}
		line.color(tagCompoundPatternOn, defaultStyleColor(s.OnColor))
		line.color(tagCompoundPatternOff, s.OffColor)
		line.float(tagCompoundOnOpacity, s.OnOpacity, 1)
		line.optionalFloat(tagCompoundOffOpacity, s.OffOpacity)
		line.float(tagCompoundLineThickness, s.Thickness, 0)
		line.code(tagCompoundLineDashing, s.Dashing, "SOLID")
		if s.Pattern != nil {
			line = append(line, derivedio.UL(tagCompoundLinePattern, *s.Pattern))
		}
		line.shadow(s.Shadow, s.ShadowColor, s.ShadowOffsetX, s.ShadowOffsetY, s.ShadowOpacity, true)
		out = append(out, line.sequence(tagCompoundLineStyle))
	}
	if s := c.FillStyle; s != nil {
		fill := compoundStyleWriter{}
		fill.color(tagCompoundPatternOn, defaultStyleColor(s.OnColor))
		fill.color(tagCompoundPatternOff, s.OffColor)
		fill.float(tagCompoundOnOpacity, s.OnOpacity, 1)
		fill.float(tagCompoundOffOpacity, s.OffOpacity, 0)
		fill.code(tagCompoundFillMode, s.Mode, "SOLID")
		if len(s.Pattern) != 0 {
			fill = append(fill, derivedio.Raw(tagCompoundFillPattern, core.VROB, s.Pattern))
		}
		out = append(out, fill.sequence(tagCompoundFillStyle))
	}
	if s := c.TextStyle; s != nil {
		text := compoundStyleWriter{}
		if s.FontName != "" {
			text = append(text, derivedio.LO(tagCompoundFontName, s.FontName))
		}
		text.code(tagCompoundFontNameType, s.FontNameType, "")
		css := s.CSSFontName
		if css == "" {
			css = "sans-serif"
		}
		text = append(text, derivedio.LO(tagCompoundCSSFont, css))
		text.color(tagCompoundTextColor, defaultStyleColor(s.Color))
		text.code(tagCompoundHorizontal, s.HorizontalAlignment, "")
		text.code(tagCompoundVertical, s.VerticalAlignment, "")
		text.shadow(s.Shadow, s.ShadowColor, s.ShadowOffsetX, s.ShadowOffsetY, s.ShadowOpacity, false)
		text.yn(tagCompoundUnderlined, s.Underlined)
		text.yn(tagCompoundBold, s.Bold)
		text.yn(tagCompoundItalic, s.Italic)
		out = append(out, text.sequence(tagCompoundTextStyle))
	}
	return out
}

func defaultStyleColor(value *[3]uint16) *[3]uint16 {
	if value == nil {
		return &[3]uint16{0xffff, 0x8080, 0x8080}
	}
	return value
}

type compoundStyleWriter []core.Element

func (w *compoundStyleWriter) float(tag core.Tag, value *float64, fallback float64) {
	if value != nil {
		fallback = *value
	}
	*w = append(*w, derivedio.FL(tag, fallback))
}

func (w *compoundStyleWriter) optionalFloat(tag core.Tag, value *float64) {
	if value != nil {
		w.float(tag, value, 0)
	}
}

func (w *compoundStyleWriter) code(tag core.Tag, value, fallback string) {
	if value == "" {
		value = fallback
	}
	if value != "" {
		*w = append(*w, derivedio.CS(tag, value))
	}
}

func (w *compoundStyleWriter) yn(tag core.Tag, value *bool) {
	w.code(tag, flipValue(value != nil && *value), "")
}

func (w *compoundStyleWriter) color(tag core.Tag, value *[3]uint16) {
	if value != nil {
		*w = append(*w, derivedio.US(tag, value[:]...))
	}
}

func (w *compoundStyleWriter) shadow(style string, color *[3]uint16, x, y, opacity *float64, includeDefaults bool) {
	w.code(tagCompoundShadowStyle, style, "OFF")
	if includeDefaults {
		w.float(tagCompoundShadowOffsetX, x, 0)
		w.float(tagCompoundShadowOffsetY, y, 0)
		w.float(tagCompoundShadowOpacity, opacity, 0)
		if color == nil {
			color = &[3]uint16{0, 0x8080, 0x8080}
		}
	} else {
		w.optionalFloat(tagCompoundShadowOffsetX, x)
		w.optionalFloat(tagCompoundShadowOffsetY, y)
		w.optionalFloat(tagCompoundShadowOpacity, opacity)
	}
	w.color(tagCompoundShadowColor, color)
}

func (w compoundStyleWriter) sequence(tag core.Tag) core.Element {
	return derivedio.Seq(tag, derivedio.DataSet(w...))
}
