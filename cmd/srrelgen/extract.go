package main

import (
	"fmt"
	"html"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// sopClasses maps the IOD names of the A.35 table captions to their SOP
// Classes (PS3.4 B.5, PS3.6 Table A-1). The Rendition Selection Document has
// no Storage SOP Class; it is exchanged as Real-Time Communication.
var sopClasses = map[string]string{
	"Basic Text SR":                             "1.2.840.10008.5.1.4.1.1.88.11",
	"Enhanced SR":                               "1.2.840.10008.5.1.4.1.1.88.22",
	"Comprehensive SR":                          "1.2.840.10008.5.1.4.1.1.88.33",
	"Comprehensive 3D SR":                       "1.2.840.10008.5.1.4.1.1.88.34",
	"Extensible SR":                             "1.2.840.10008.5.1.4.1.1.88.35",
	"Procedure Log":                             "1.2.840.10008.5.1.4.1.1.88.40",
	"Mammography CAD SR":                        "1.2.840.10008.5.1.4.1.1.88.50",
	"Key Object Selection Document":             "1.2.840.10008.5.1.4.1.1.88.59",
	"Chest CAD SR":                              "1.2.840.10008.5.1.4.1.1.88.65",
	"X-Ray Radiation Dose SR":                   "1.2.840.10008.5.1.4.1.1.88.67",
	"Radiopharmaceutical Radiation Dose SR":     "1.2.840.10008.5.1.4.1.1.88.68",
	"Colon CAD SR":                              "1.2.840.10008.5.1.4.1.1.88.69",
	"Implantation Plan SR Document":             "1.2.840.10008.5.1.4.1.1.88.70",
	"Acquisition Context SR":                    "1.2.840.10008.5.1.4.1.1.88.71",
	"Simplified Adult Echo SR":                  "1.2.840.10008.5.1.4.1.1.88.72",
	"Patient Radiation Dose SR":                 "1.2.840.10008.5.1.4.1.1.88.73",
	"Planned Imaging Agent Administration SR":   "1.2.840.10008.5.1.4.1.1.88.74",
	"Performed Imaging Agent Administration SR": "1.2.840.10008.5.1.4.1.1.88.75",
	"Enhanced X-Ray Radiation Dose SR":          "1.2.840.10008.5.1.4.1.1.88.76",
	"Waveform Annotation SR":                    "1.2.840.10008.5.1.4.1.1.88.77",
	"Spectacle Prescription Report":             "1.2.840.10008.5.1.4.1.1.78.6",
	"Macular Grid Thickness and Volume Report":  "1.2.840.10008.5.1.4.1.1.79.1",
	"Rendition Selection Document":              "1.2.840.10008.10.4",
}

var (
	editionPattern = regexp.MustCompile(`DICOM PS3\.3 (\d{4}[a-z])`)
	tablePattern   = regexp.MustCompile(`(?s)<strong>Table[\s\x{00a0}]A\.35\.(\d+)-2\.[\s\x{00a0}]*Relationship Content Constraints for ([^<]+?) IOD</strong>\s*</p>\s*<div class="table-contents">(.*?)</table>`)
	rowPattern     = regexp.MustCompile(`(?s)<tr[^>]*>(.*?)</tr>`)
	cellPattern    = regexp.MustCompile(`(?s)<td[^>]*>(.*?)</td>`)
	tagPattern     = regexp.MustCompile(`<[^>]+>`)
	spacePattern   = regexp.MustCompile(`\s+`)
	sentence       = regexp.MustCompile(`[^.]*\.`)
	byReferenceRel = regexp.MustCompile(`by-reference ((?:[A-Z]+ ?)+)`)
	notByReference = regexp.MustCompile(`The ((?:[A-Z]+ ?)+) and ((?:[A-Z]+ ?)+) relationships shall not be conveyed by-reference`)
)

func plainText(fragment string) string {
	text := html.UnescapeString(tagPattern.ReplaceAllString(fragment, " "))
	return strings.TrimSpace(spacePattern.ReplaceAllString(text, " "))
}

// valueTypeTerm normalizes a Source or Target Value Type cell.
func valueTypeTerm(cell string) (string, error) {
	cell = strings.TrimSpace(strings.TrimSuffix(cell, "(see below)"))
	switch {
	case cell == "any type":
		return "*", nil
	case strings.HasPrefix(cell, "any type (except ") && strings.HasSuffix(cell, ")"):
		return "*-" + strings.TrimSuffix(strings.TrimPrefix(cell, "any type (except "), ")"), nil
	}
	var terms []string
	for _, term := range strings.Split(cell, ",") {
		term = strings.TrimSpace(term)
		if !valueTypes[term] {
			return "", fmt.Errorf("value type cell %q: unknown term %q", cell, term)
		}
		terms = append(terms, term)
	}
	return strings.Join(terms, ","), nil
}

func relationshipTerm(cell string) (string, error) {
	if except, ok := strings.CutPrefix(cell, "any type other than "); ok && relationships[except] {
		return "*-" + except, nil
	}
	if !relationships[cell] {
		return "", fmt.Errorf("unknown relationship %q", cell)
	}
	return cell, nil
}

// byReferencePolicy reads the "Relationship Constraints" text of an IOD.
func byReferencePolicy(text string) (string, error) {
	for _, s := range sentence.FindAllString(text, -1) {
		if !strings.Contains(s, "makes use of by-reference") && !strings.Contains(s, "makes extensive use of by-reference") && !strings.Contains(s, "allows for by-reference") {
			continue
		}
		var list []string
		for _, match := range byReferenceRel.FindAllStringSubmatch(s, -1) {
			relationship := strings.TrimSpace(match[1])
			if !relationships[relationship] {
				return "", fmt.Errorf("by-reference sentence %q: unknown relationship %q", s, relationship)
			}
			list = append(list, relationship)
		}
		return "only:" + strings.Join(list, ","), nil
	}
	if strings.Contains(text, "may be conveyed either by-value or by-reference") {
		if match := notByReference.FindStringSubmatch(text); match != nil {
			return "all-except:" + strings.TrimSpace(match[1]) + "," + strings.TrimSpace(match[2]), nil
		}
		return "all", nil
	}
	for _, byValue := range []string{"shall be conveyed in the by-value mode", "shall be conveyed by-value", "may be conveyed by-value"} {
		if strings.Contains(text, byValue) {
			return "none", nil
		}
	}
	return "", fmt.Errorf("no recognizable by-reference rule")
}

// extractPage reads the relationship tables of one chtml page.
func extractPage(page string) ([]IOD, error) {
	var iods []IOD
	for _, match := range tablePattern.FindAllStringSubmatch(page, -1) {
		name := plainText(match[2])
		uid, ok := sopClasses[name]
		if !ok {
			return nil, fmt.Errorf("Table A.35.%s-2: no SOP Class for IOD %q", match[1], name)
		}
		iod := IOD{SOPClassUID: uid, Table: "A.35." + match[1] + "-2", Name: name}
		for _, row := range rowPattern.FindAllStringSubmatch(match[3], -1) {
			cells := cellPattern.FindAllStringSubmatch(row[1], -1)
			if len(cells) == 0 {
				continue // header row
			}
			if len(cells) != 3 {
				return nil, fmt.Errorf("%s: row with %d cells", iod.Table, len(cells))
			}
			sources, err := valueTypeTerm(plainText(cells[0][1]))
			if err != nil {
				return nil, fmt.Errorf("%s: %w", iod.Table, err)
			}
			relationship, err := relationshipTerm(plainText(cells[1][1]))
			if err != nil {
				return nil, fmt.Errorf("%s: %w", iod.Table, err)
			}
			targets, err := valueTypeTerm(plainText(cells[2][1]))
			if err != nil {
				return nil, fmt.Errorf("%s: %w", iod.Table, err)
			}
			iod.Rules = append(iod.Rules, Rule{Sources: sources, Relationship: relationship, Targets: targets})
		}
		policy, err := byReferencePolicy(plainText(page))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", iod.Table, err)
		}
		iod.ByReference = policy
		iods = append(iods, iod)
	}
	return iods, nil
}

// extractDir reads every sect_A.35*.html page of dir.
func extractDir(dir, edition string) (Table, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "sect_A.35*.html"))
	if err != nil {
		return Table{}, err
	}
	table := Table{Edition: edition}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return Table{}, err
		}
		page := string(data)
		if match := editionPattern.FindStringSubmatch(page); match != nil {
			if table.Edition == "" {
				table.Edition = match[1]
			} else if table.Edition != match[1] {
				return Table{}, fmt.Errorf("%s: edition %s, want %s", filepath.Base(path), match[1], table.Edition)
			}
		}
		iods, err := extractPage(page)
		if err != nil {
			return Table{}, fmt.Errorf("%s: %w", filepath.Base(path), err)
		}
		table.IODs = append(table.IODs, iods...)
	}
	sortIODs(table.IODs)
	if err := table.validate(); err != nil {
		return Table{}, err
	}
	if len(table.IODs) != len(sopClasses) {
		return Table{}, fmt.Errorf("found %d relationship tables, want %d", len(table.IODs), len(sopClasses))
	}
	return table, nil
}
