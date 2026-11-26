package main

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Class is one Storage SOP Class of the catalog.
type Class struct {
	UID      string
	Name     string
	Keyword  string // from PS3.6 Table A-1; empty when that edition lacks it
	Category string
	Modality string // the Modality (0008,0060) the IOD fixes; empty otherwise
	Source   string // the table it comes from
	Retired  bool
}

// Table is the vendored catalog.
type Table struct {
	Edition string // PS3.4 edition of the extracted pages, such as 2026d
	Date    string // extraction date
	Classes []Class
}

// Sources, in the order the extractor consults them.
const (
	sourceB5   = "PS3.4 B.5-1"
	sourceB6   = "PS3.4 B.6-1"
	sourceGG3  = "PS3.4 GG.3-1"
	sourcePS36 = "PS3.6 A-1"
)

var (
	knownSources  = map[string]bool{sourceB5: true, sourceB6: true, sourceGG3: true, sourcePS36: true}
	uidPattern    = regexp.MustCompile(`^1\.2\.840\.10008(\.(0|[1-9][0-9]*))+$`)
	modalityValue = regexp.MustCompile(`^[A-Z0-9]{0,16}$`)
	editionValue  = regexp.MustCompile(`^\d{4}[a-z]$`)
	dateValue     = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

func (table Table) validate() error {
	if !editionValue.MatchString(table.Edition) || !dateValue.MatchString(table.Date) {
		return fmt.Errorf("edition %q or date %q is malformed", table.Edition, table.Date)
	}
	if len(table.Classes) == 0 {
		return fmt.Errorf("no SOP Classes")
	}
	seen := map[string]bool{}
	for _, class := range table.Classes {
		switch {
		case !uidPattern.MatchString(class.UID):
			return fmt.Errorf("UID %q is not a DICOM UID", class.UID)
		case seen[class.UID]:
			return fmt.Errorf("UID %s is listed twice", class.UID)
		case strings.TrimSpace(class.Name) == "" || strings.ContainsAny(class.Name, "\t\n\""):
			return fmt.Errorf("%s: name %q", class.UID, class.Name)
		case !knownCategories[class.Category]:
			return fmt.Errorf("%s: unknown category %q", class.UID, class.Category)
		case !modalityValue.MatchString(class.Modality):
			return fmt.Errorf("%s: modality %q", class.UID, class.Modality)
		case !knownSources[class.Source]:
			return fmt.Errorf("%s: unknown source %q", class.UID, class.Source)
		case strings.ContainsAny(class.Keyword, " \t\""):
			return fmt.Errorf("%s: keyword %q", class.UID, class.Keyword)
		}
		seen[class.UID] = true
	}
	return nil
}

// uidLess orders UIDs component by component.
func uidLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		if pa[i] == pb[i] {
			continue
		}
		na, _ := strconv.ParseUint(pa[i], 10, 64)
		nb, _ := strconv.ParseUint(pb[i], 10, 64)
		return na < nb
	}
	return len(pa) < len(pb)
}

func sortClasses(classes []Class) {
	sort.Slice(classes, func(i, j int) bool { return uidLess(classes[i].UID, classes[j].UID) })
}

const tsvHeader = "UID\tName\tKeyword\tCategory\tModality\tSource\tRetired"

func formatTSV(table Table, date string) []byte {
	if date == "" {
		date = table.Date
	}
	var b bytes.Buffer
	b.WriteString("# Storage SOP Classes (#1046)\n")
	fmt.Fprintf(&b, "# Source: DICOM PS3.4 %s Tables B.5-1, B.6-1 and GG.3-1, extracted from the chtml pages on %s,\n", table.Edition, date)
	b.WriteString("# with the Storage SOP Classes of the vendored PS3.6 Table A-1 (uids.tsv) that PS3.4 does not list.\n")
	b.WriteString("# Category and Modality are assigned by the rules of cmd/storageclassgen; regenerate, do not edit.\n")
	b.WriteString(tsvHeader + "\n")
	for _, class := range table.Classes {
		fmt.Fprintf(&b, "%s\t%s\t%s\t%s\t%s\t%s\t%t\n", class.UID, class.Name, class.Keyword, class.Category, class.Modality, class.Source, class.Retired)
	}
	return b.Bytes()
}

var headerPattern = regexp.MustCompile(`# Source: DICOM PS3\.4 (\S+) Tables B\.5-1, B\.6-1 and GG\.3-1, extracted from the chtml pages on (\S+),`)

func parseTSV(data []byte) (Table, error) {
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	match := headerPattern.FindSubmatch(data)
	if match == nil {
		return Table{}, fmt.Errorf("the TSV header has no edition and date")
	}
	table := Table{Edition: string(match[1]), Date: string(match[2])}
	sawHeader := false
	for number, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		if !sawHeader {
			if line != tsvHeader {
				return Table{}, fmt.Errorf("line %d: header %q", number+1, line)
			}
			sawHeader = true
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 7 {
			return Table{}, fmt.Errorf("line %d: %d fields", number+1, len(fields))
		}
		retired, err := strconv.ParseBool(fields[6])
		if err != nil {
			return Table{}, fmt.Errorf("line %d: retired %q", number+1, fields[6])
		}
		table.Classes = append(table.Classes, Class{UID: fields[0], Name: fields[1], Keyword: fields[2], Category: fields[3], Modality: fields[4], Source: fields[5], Retired: retired})
	}
	if err := table.validate(); err != nil {
		return Table{}, err
	}
	sorted := append([]Class(nil), table.Classes...)
	sortClasses(sorted)
	for i := range sorted {
		if sorted[i].UID != table.Classes[i].UID {
			return Table{}, fmt.Errorf("the TSV is not sorted by UID at %s", table.Classes[i].UID)
		}
	}
	return table, nil
}
