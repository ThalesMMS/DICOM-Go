package main

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// The extraction is committed as two TSV files next to the codes package,
// which embeds them: terms.tsv (scheme, value, meaning, flags) and cid.tsv
// (a "CID" line per group followed by its "include" and "code" lines).
// Both start with a provenance comment naming the edition and the SHA-256
// of the part16.xml they were extracted from.

func provenance(x Extraction) string {
	return fmt.Sprintf("# DICOM %s, part16.xml sha256 %s\n", x.Edition, x.SHA256)
}

func writeTerms(w io.Writer, x Extraction) error {
	b := bufio.NewWriter(w)
	fmt.Fprint(b, provenance(x))
	fmt.Fprint(b, "# scheme\tvalue\tmeaning\tflags\n")
	for _, terms := range [][]Term{x.DCM, x.Others} {
		for _, term := range terms {
			fmt.Fprintf(b, "%s\t%s\t%s\t%s\n", term.Scheme, term.Value, term.Meaning, flagIf(term.Retired, "retired"))
		}
	}
	return b.Flush()
}

func writeContextGroups(w io.Writer, x Extraction) error {
	b := bufio.NewWriter(w)
	fmt.Fprint(b, provenance(x))
	fmt.Fprint(b, "# CID\tid\tkeyword\ttype\tversion\tuid\tflags\tname\n# include\tid\n# code\tscheme\tvalue\tmeaning\tflags\n")
	for _, group := range x.ContextGroups {
		kind := "non-extensible"
		if group.Extensible {
			kind = "extensible"
		}
		var flags []string
		if group.Retired {
			flags = append(flags, "retired")
		}
		if group.WithoutMemberTables {
			flags = append(flags, "no-table")
		}
		fmt.Fprintf(b, "CID\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n", group.ID, group.Keyword, kind, group.Version, group.UID, strings.Join(flags, ","), group.Name)
		for _, include := range group.Includes {
			fmt.Fprintf(b, "include\t%d\n", include)
		}
		for _, term := range group.Codes {
			fmt.Fprintf(b, "code\t%s\t%s\t%s\t%s\n", term.Scheme, term.Value, term.Meaning, flagIf(term.Retired, "retired"))
		}
	}
	return b.Flush()
}

func flagIf(set bool, name string) string {
	if set {
		return name
	}
	return ""
}

// readProvenance returns the edition and SHA-256 of a TSV header line.
func readProvenance(line string) (string, string, error) {
	rest, ok := strings.CutPrefix(line, "# DICOM ")
	if !ok {
		return "", "", fmt.Errorf("missing provenance line: %q", line)
	}
	edition, sum, ok := strings.Cut(rest, ", part16.xml sha256 ")
	if !ok || len(sum) != 64 {
		return "", "", fmt.Errorf("malformed provenance line: %q", line)
	}
	return edition, sum, nil
}

// readTSV reads both files back into an Extraction.
func readTSV(terms, groups io.Reader) (Extraction, error) {
	var x Extraction
	lines := bufio.NewScanner(terms)
	lines.Buffer(make([]byte, 1<<20), 1<<20)
	for n := 0; lines.Scan(); n++ {
		line := lines.Text()
		if n == 0 {
			var err error
			if x.Edition, x.SHA256, err = readProvenance(line); err != nil {
				return x, err
			}
			continue
		}
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 4 {
			return x, fmt.Errorf("terms.tsv line %d: %d fields", n+1, len(fields))
		}
		term := Term{Scheme: fields[0], Value: fields[1], Meaning: fields[2], Retired: fields[3] == "retired"}
		if term.Scheme == "DCM" {
			x.DCM = append(x.DCM, term)
		} else {
			x.Others = append(x.Others, term)
		}
	}
	if err := lines.Err(); err != nil {
		return x, err
	}
	lines = bufio.NewScanner(groups)
	lines.Buffer(make([]byte, 1<<20), 1<<20)
	for n := 0; lines.Scan(); n++ {
		line := lines.Text()
		if n == 0 {
			edition, sum, err := readProvenance(line)
			if err != nil {
				return x, err
			}
			if edition != x.Edition || sum != x.SHA256 {
				return x, fmt.Errorf("cid.tsv and terms.tsv come from different sources")
			}
			continue
		}
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		switch fields[0] {
		case "CID":
			if len(fields) != 8 {
				return x, fmt.Errorf("cid.tsv line %d: %d fields", n+1, len(fields))
			}
			id, err := strconv.Atoi(fields[1])
			if err != nil {
				return x, fmt.Errorf("cid.tsv line %d: %w", n+1, err)
			}
			x.ContextGroups = append(x.ContextGroups, ContextGroup{
				ID: id, Keyword: fields[2], Extensible: fields[3] == "extensible", Version: fields[4], UID: fields[5],
				Retired: strings.Contains(fields[6], "retired"), WithoutMemberTables: strings.Contains(fields[6], "no-table"), Name: fields[7],
			})
		case "include", "code":
			if len(x.ContextGroups) == 0 {
				return x, fmt.Errorf("cid.tsv line %d: member before any CID", n+1)
			}
			group := &x.ContextGroups[len(x.ContextGroups)-1]
			if fields[0] == "include" && len(fields) == 2 {
				id, err := strconv.Atoi(fields[1])
				if err != nil {
					return x, fmt.Errorf("cid.tsv line %d: %w", n+1, err)
				}
				group.Includes = append(group.Includes, id)
				continue
			}
			if len(fields) != 5 {
				return x, fmt.Errorf("cid.tsv line %d: %d fields", n+1, len(fields))
			}
			group.Codes = append(group.Codes, Term{Scheme: fields[1], Value: fields[2], Meaning: fields[3], Retired: fields[4] == "retired"})
		default:
			return x, fmt.Errorf("cid.tsv line %d: unknown record %q", n+1, fields[0])
		}
	}
	return x, lines.Err()
}
