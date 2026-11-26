package sr

import (
	"io"
	"strconv"
	"strings"
)

// RenderItemsWithMetadataSnapshot resolves short, long and URN code identities
// using one structure-checked metadata snapshot. A nil snapshot deliberately
// selects legacy RenderItems behavior. Non-nil stale context is always rejected,
// even when ShowCodes is false or MaxItems would hide the changed item.
func RenderItemsWithMetadataSnapshot(doc *Document, snapshot *TemplateMetadataSnapshot, opts RenderOptions) (RenderedDocument, error) {
	if snapshot == nil {
		return RenderItems(doc, opts)
	}
	if err := snapshot.Check(doc); err != nil {
		return RenderedDocument{}, err
	}
	return renderItems(doc, opts, snapshot.index.code)
}

// RenderTextWithMetadataSnapshot writes the checked presentation through the
// same control-character escaping as RenderText. Failure before rendering leaves
// the supplied writer untouched; ordinary writer errors retain io.Writer rules.
func RenderTextWithMetadataSnapshot(w io.Writer, doc *Document, snapshot *TemplateMetadataSnapshot, opts RenderOptions) error {
	rendered, err := RenderItemsWithMetadataSnapshot(doc, snapshot, opts)
	if err != nil {
		return err
	}
	return renderText(w, rendered)
}

// RenderHTMLWithMetadataSnapshot HTML-escapes all code data. URNs and URLs are
// inert text; they never become links, attributes or network requests.
func RenderHTMLWithMetadataSnapshot(w io.Writer, doc *Document, snapshot *TemplateMetadataSnapshot, opts RenderOptions) error {
	rendered, err := RenderItemsWithMetadataSnapshot(doc, snapshot, opts)
	if err != nil {
		return err
	}
	return renderHTML(w, rendered)
}

func contextCode(code CodedEntry, context CodeContext, show bool) string {
	value := code.CodeValue
	if context.LongCodeValue != "" {
		value = context.LongCodeValue
	}
	if context.URNCodeValue != "" {
		value = context.URNCodeValue
	}
	meaning := strings.TrimSpace(code.CodeMeaning)
	if meaning == "" {
		meaning = value
	}
	if !show || value == "" {
		return meaning
	}
	parts := []string{value}
	if code.CodingSchemeDesignator != "" {
		parts = append(parts, code.CodingSchemeDesignator)
	}
	if context.CodingSchemeVersion != "" {
		parts = append(parts, context.CodingSchemeVersion)
	}
	return meaning + " (" + strings.Join(parts, ", ") + ")"
}

func (r *renderer) contextValue(item ContentItem, contexts ContentItemCodeContexts) string {
	switch item.ValueType {
	case ValueCode:
		return contextCode(item.Code, contexts.Value, r.opts.ShowCodes)
	case ValueNum:
		if item.Measurement == nil {
			if item.NumericValueQualifier.Empty() {
				return "no value"
			}
			return contextCode(item.NumericValueQualifier, contexts.NumericValueQualifier, r.opts.ShowCodes)
		}
		units := item.Measurement.Units
		unit := contextCode(units, contexts.MeasurementUnits, r.opts.ShowCodes)
		if !r.opts.ShowCodes && strings.EqualFold(units.CodingSchemeDesignator, "UCUM") && units.CodeValue != "" {
			unit = units.CodeValue
			if unit == "1" || unit == "{ratio}" {
				unit = ""
			}
		}
		value := strings.TrimSpace(strconv.FormatFloat(item.Measurement.Value, 'g', -1, 64) + " " + unit)
		if r.opts.ShowCodes && !item.NumericValueQualifier.Empty() {
			value += " [qualifier: " + contextCode(item.NumericValueQualifier, contexts.NumericValueQualifier, true) + "]"
		}
		return value
	default:
		return r.value(item)
	}
}
