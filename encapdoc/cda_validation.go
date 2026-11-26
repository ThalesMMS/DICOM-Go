package encapdoc

import (
	"context"
	"errors"
)

// CDAValidationProfile selects the header checks applied by ValidateCDA.
type CDAValidationProfile string

const (
	// CDAProfileBaseHeader checks selected CDA R2 header cardinalities and
	// the optional document revision identifier pair.
	CDAProfileBaseHeader CDAValidationProfile = "base"
	// CDAProfileCCDHeader adds the selected CCD document template and
	// LOINC document code checks to the base profile.
	CDAProfileCCDHeader CDAValidationProfile = "ccd"
)

// ErrUnsupportedCDAValidationProfile reports an unrecognized profile option.
var ErrUnsupportedCDAValidationProfile = errors.New("encapdoc: unsupported CDA validation profile")

// CDAValidationOptions configures an opt-in CDA validation. The zero profile
// selects CDAProfileBaseHeader. Limits bound the bytes, depth and XML tokens.
type CDAValidationOptions struct {
	Profile CDAValidationProfile
	Limits  PayloadLimits
}

// CDAValidationFinding reports a fixed rule code, its structural path and
// the number of occurrences. It contains no document values.
type CDAValidationFinding struct {
	Code  string
	Path  string
	Count int
}

// CDAValidationReport records the selected profile and its failed rules.
// A report with no findings has passed these specific checks.
type CDAValidationReport struct {
	Profile  CDAValidationProfile
	Findings []CDAValidationFinding
}

// HasErrors reports whether the validation found any failures.
func (r CDAValidationReport) HasErrors() bool { return len(r.Findings) != 0 }

// ValidateCDA checks the selected document header and local references in the
// structured body. It never fetches resources, expands links or changes the
// source. XML, resource and context failures return an error and no report.
// The report covers these checks only and does not establish CDA conformance.
func ValidateCDA(ctx context.Context, data []byte, opts CDAValidationOptions) (CDAValidationReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return CDAValidationReport{}, err
	}
	if opts.Profile == "" {
		opts.Profile = CDAProfileBaseHeader
	}
	if opts.Profile != CDAProfileBaseHeader && opts.Profile != CDAProfileCCDHeader {
		return CDAValidationReport{}, ErrUnsupportedCDAValidationProfile
	}
	limits, err := normalizeCDANarrativeLimits(CDANarrativeLimits{
		MaxBytes: opts.Limits.MaxBytes, MaxDepth: opts.Limits.MaxDepth, MaxTokens: opts.Limits.MaxRecords,
	})
	if err != nil {
		return CDAValidationReport{}, err
	}
	if _, err := InspectPayload(ctx, MIMETypeCDA, data, PayloadLimits{
		MaxBytes: limits.MaxBytes, MaxDepth: limits.MaxDepth, MaxRecords: limits.MaxTokens,
	}); err != nil {
		return CDAValidationReport{}, err
	}
	root, err := readCDANarrativeXML(ctx, data, limits, true)
	if err != nil {
		return CDAValidationReport{}, err
	}
	report := CDAValidationReport{Profile: opts.Profile}
	body := report.validateHeader(root)
	if body != nil {
		if err := report.validateNarrativeLinks(ctx, body); err != nil {
			return CDAValidationReport{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return CDAValidationReport{}, err
	}
	return report, nil
}

const (
	cdaHeaderPath = "/ClinicalDocument"
	cdaBodyPath   = cdaHeaderPath + "/component/structuredBody"
)

func (r *CDAValidationReport) add(code, path string) {
	// Only a fixed set of rules and paths can be added to a report.
	for i, finding := range r.Findings {
		if finding.Code == code && finding.Path == path {
			r.Findings[i].Count++
			return
		}
	}
	r.Findings = append(r.Findings, CDAValidationFinding{Code: code, Path: path, Count: 1})
}

func (r *CDAValidationReport) validateHeader(root *cdaXMLNode) *cdaXMLNode {
	header := cdaChildren(root)
	for _, name := range []string{"code", "effectiveTime", "confidentialityCode", "custodian", "component"} {
		if len(header[name]) != 1 {
			r.add("header-cardinality", cdaHeaderPath+"/"+name)
		}
	}
	for _, name := range []string{"recordTarget", "author"} {
		if len(header[name]) == 0 {
			r.add("header-cardinality", cdaHeaderPath+"/"+name)
		}
	}
	for _, name := range []string{"typeId", "title", "setId", "versionNumber"} {
		if len(header[name]) > 1 {
			r.add("header-cardinality", cdaHeaderPath+"/"+name)
		}
	}
	for _, attr := range []struct{ name, expected string }{{"classCode", "DOCCLIN"}, {"moodCode", "EVN"}} {
		if value := root.attribute(attr.name); value != "" && value != attr.expected {
			r.add("header-fixed-value", cdaHeaderPath+"/@"+attr.name)
		}
	}
	if ids := header["typeId"]; len(ids) == 1 {
		if ids[0].attribute("root") != "2.16.840.1.113883.1.3" || ids[0].attribute("extension") != "POCD_HD000040" {
			r.add("header-fixed-value", cdaHeaderPath+"/typeId")
		}
	}
	if (len(header["setId"]) == 0) != (len(header["versionNumber"]) == 0) {
		r.add("revision-pair", cdaHeaderPath)
	}
	if r.Profile == CDAProfileCCDHeader {
		r.validateCCDHeader(header)
	}
	if len(header["component"]) != 1 {
		return nil
	}
	body := cdaChildren(header["component"][0])
	if len(body["structuredBody"])+len(body["nonXMLBody"]) != 1 {
		r.add("body-choice", cdaHeaderPath+"/component")
		return nil
	}
	if len(body["structuredBody"]) == 1 {
		return body["structuredBody"][0]
	}
	return nil
}

func (r *CDAValidationReport) validateCCDHeader(header map[string][]*cdaXMLNode) {
	const ccdRoot = "2.16.840.1.113883.10.20.22.1.2"
	declared := false
	for _, template := range header["templateId"] {
		if template.attribute("root") == ccdRoot && template.attribute("nullFlavor") == "" {
			declared = true
			break
		}
	}
	if !declared {
		r.add("ccd-template", cdaHeaderPath+"/templateId")
	}
	if codes := header["code"]; len(codes) == 1 {
		code := codes[0]
		if code.attribute("code") != "34133-9" || code.attribute("codeSystem") != "2.16.840.1.113883.6.1" ||
			code.attribute("nullFlavor") != "" {
			r.add("ccd-document-code", cdaHeaderPath+"/code")
		}
	}
}

func cdaChildren(node *cdaXMLNode) map[string][]*cdaXMLNode {
	children := make(map[string][]*cdaXMLNode)
	for _, part := range node.parts {
		if child := part.node; child != nil && child.name.Space == cdaNamespace {
			children[child.name.Local] = append(children[child.name.Local], child)
		}
	}
	return children
}
