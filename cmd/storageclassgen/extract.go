package main

import (
	"bufio"
	"bytes"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	editionPattern = regexp.MustCompile(`DICOM PS3\.4 (\d{4}[a-z])`)
	rowPattern     = regexp.MustCompile(`(?s)<tr[^>]*>(.*?)</tr>`)
	cellPattern    = regexp.MustCompile(`(?s)<td[^>]*>(.*?)</td>`)
	tagPattern     = regexp.MustCompile(`<[^>]+>`)
	spacePattern   = regexp.MustCompile(`\s+`)
)

// pages are the PS3.4 chtml pages and the tables read from each.
var pages = []struct {
	file, table, source string
	retired             bool
}{
	{"sect_B.5.html", "B.5-1", sourceB5, false},
	{"sect_B.6.html", "B.6-1", sourceB6, true},
	{"sect_GG.3.html", "GG.3-1", sourceGG3, false},
}

// plainText is the text of an HTML fragment without the zero-width spaces
// the chtml pages put inside long UIDs.
func plainText(fragment string) string {
	text := html.UnescapeString(tagPattern.ReplaceAllString(fragment, " "))
	text = strings.NewReplacer("​", "", " ", " ").Replace(text)
	return strings.TrimSpace(spacePattern.ReplaceAllString(text, " "))
}

// tableRows returns the Name and UID cells of the rows of the table titled
// "Table <number>." in page.
func tableRows(page, number string) ([][2]string, error) {
	title := regexp.MustCompile(`<strong>Table[\s\x{00a0}]` + regexp.QuoteMeta(number) + `\.`)
	loc := title.FindStringIndex(page)
	if loc == nil {
		return nil, fmt.Errorf("table %s not found", number)
	}
	rest := page[loc[1]:]
	end := strings.Index(rest, "</table>")
	if end < 0 {
		return nil, fmt.Errorf("table %s is not closed", number)
	}
	var rows [][2]string
	for _, row := range rowPattern.FindAllStringSubmatch(rest[:end], -1) {
		cells := cellPattern.FindAllStringSubmatch(row[1], -1)
		if len(cells) < 2 {
			continue // the heading row uses <th>
		}
		name, uid := plainText(cells[0][1]), plainText(cells[1][1])
		if !uidPattern.MatchString(uid) {
			return nil, fmt.Errorf("table %s: %q has UID %q", number, name, uid)
		}
		rows = append(rows, [2]string{name, uid})
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("table %s has no rows", number)
	}
	return rows, nil
}

type ps36Entry struct {
	name, keyword string
	retired       bool
}

// readPS36 returns the SOP Classes of the vendored PS3.6 Table A-1.
func readPS36(data []byte) (map[string]ps36Entry, error) {
	out := map[string]ps36Entry{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 5 {
			return nil, fmt.Errorf("uids.tsv: malformed line %q", line)
		}
		if fields[3] == "SOP Class" {
			out[fields[0]] = ps36Entry{name: fields[1], keyword: fields[2], retired: fields[4] == "true"}
		}
	}
	return out, scanner.Err()
}

// ps36Storage reports a PS3.6 SOP Class that is a Storage SOP Class: its
// name says Storage, and it is not Storage Commitment or the Media Storage
// Directory (DICOMDIR), which is not exchanged with C-STORE.
func ps36Storage(uid string, entry ps36Entry) bool {
	return strings.Contains(entry.name, "Storage") && !strings.Contains(entry.name, "Commitment") && uid != "1.2.840.10008.1.3.10"
}

func extractDir(dir string, uids []byte) (Table, error) {
	ps36, err := readPS36(uids)
	if err != nil {
		return Table{}, err
	}
	table := Table{}
	seen := map[string]bool{}
	add := func(name, uid, source string, retired bool) error {
		if seen[uid] {
			return nil
		}
		seen[uid] = true
		category, modality, err := classify(name)
		if err != nil {
			return err
		}
		table.Classes = append(table.Classes, Class{UID: uid, Name: name, Keyword: ps36[uid].keyword, Category: category, Modality: modality, Source: source, Retired: retired})
		return nil
	}
	for _, p := range pages {
		data, err := os.ReadFile(filepath.Join(dir, p.file))
		if err != nil {
			return Table{}, err
		}
		page := string(data)
		match := editionPattern.FindStringSubmatch(page)
		if match == nil {
			return Table{}, fmt.Errorf("%s: no PS3.4 edition", p.file)
		}
		if table.Edition == "" {
			table.Edition = match[1]
		} else if table.Edition != match[1] {
			return Table{}, fmt.Errorf("%s is PS3.4 %s, not %s", p.file, match[1], table.Edition)
		}
		rows, err := tableRows(page, p.table)
		if err != nil {
			return Table{}, fmt.Errorf("%s: %w", p.file, err)
		}
		for _, row := range rows {
			if err := add(row[0], row[1], p.source, p.retired); err != nil {
				return Table{}, err
			}
		}
	}
	for uid, entry := range ps36 {
		if !seen[uid] && ps36Storage(uid, entry) {
			name := strings.TrimSpace(strings.TrimSuffix(entry.name, "(Retired)"))
			if err := add(name, uid, sourcePS36, entry.retired); err != nil {
				return Table{}, err
			}
		}
	}
	sortClasses(table.Classes)
	table.Date = "0000-00-00"
	return table, nil
}
