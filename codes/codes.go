// Package codes is the terminology of DICOM PS3.16, generated from its
// DocBook by cmd/terminologygen: the DICOM Controlled Terminology of
// Annex D, the codes of other schemes that the Context Groups and templates
// use, and the Context Groups (CIDs) of Annex B with their members,
// inclusions and extensibility.
//
// The subpackages dcm, sct, ucum, umls and ln name the codes of those
// schemes (dcm.ImagingMeasurementReport, sct.FindingSite, ...); Lookup and
// ContextGroupByID cover every scheme and group. The tables are the
// committed terms.tsv and cid.tsv, embedded and parsed on first use.
package codes

//go:generate go run ../cmd/terminologygen -dir .

import (
	"bufio"
	_ "embed"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// Code is a coded concept: Code Value, Coding Scheme Designator and Code
// Meaning.
type Code struct {
	Value, Scheme, Meaning string
}

// String formats the code as PS3.16 writes it: (value, scheme, "meaning").
func (c Code) String() string {
	return fmt.Sprintf("(%s, %s, %q)", c.Value, c.Scheme, c.Meaning)
}

// Equal reports whether a and b are the same concept: the same value and
// scheme, whatever their meanings.
func (c Code) Equal(other Code) bool {
	return c.Value == other.Value && c.Scheme == other.Scheme
}

// Term is a Code with its PS3.16 status.
type Term struct {
	Code
	// Retired codes are kept for recognition but must not be used in new
	// objects.
	Retired bool
}

// ContextGroup is one Context Group of PS3.16 Annex B.
type ContextGroup struct {
	ID      int
	Keyword string
	Name    string
	// Extensible groups may be extended with other codes (PS3.16 7.2.2).
	Extensible bool
	Version    string // Context Group Version, YYYYMMDD
	UID        string // Context Group UID
	Retired    bool
	// DefinedByReference groups have no member table: their codes are
	// those of an external resource (every UCUM unit, ISO 639 languages,
	// ...), so Contains cannot decide membership.
	DefinedByReference bool
	// Codes are the group's own members; Includes the groups it includes.
	Codes    []Term
	Includes []int
}

//go:embed terms.tsv
var termsTSV string

//go:embed cid.tsv
var cidTSV string

type tables struct {
	edition, sha256 string
	terms           map[string]Term
	groups          map[int]*ContextGroup
	keywords        map[string]int
	err             error
}

var (
	loadOnce sync.Once
	loaded   tables
)

func load() *tables {
	loadOnce.Do(func() { loaded = parse(termsTSV, cidTSV) })
	return &loaded
}

func key(scheme, value string) string { return scheme + "\x00" + value }

// Source is the PS3.16 edition and the SHA-256 of the part16.xml the
// tables were generated from.
func Source() (edition, sha256 string) {
	t := load()
	return t.edition, t.sha256
}

// Lookup returns the PS3.16 term for a Coding Scheme Designator and Code
// Value: every DCM code of Annex D, and the codes of other schemes that the
// Context Groups and templates use.
func Lookup(scheme, value string) (Term, bool) {
	term, ok := load().terms[key(scheme, value)]
	return term, ok
}

// ContextGroupByID returns CID id.
func ContextGroupByID(id int) (ContextGroup, bool) {
	group, ok := load().groups[id]
	if !ok {
		return ContextGroup{}, false
	}
	return *group, true
}

// ContextGroupByKeyword returns the CID with the keyword (as
// "GeneralRegionOfInterestMeasurementModifier").
func ContextGroupByKeyword(keyword string) (ContextGroup, bool) {
	id, ok := load().keywords[keyword]
	if !ok {
		return ContextGroup{}, false
	}
	return ContextGroupByID(id)
}

// Members returns the group's codes with those of the groups it includes,
// recursively, each code once.
func (g ContextGroup) Members() []Term {
	var out []Term
	seen := map[string]bool{}
	visited := map[int]bool{}
	var walk func(ContextGroup)
	walk = func(group ContextGroup) {
		if visited[group.ID] {
			return
		}
		visited[group.ID] = true
		for _, term := range group.Codes {
			if k := key(term.Scheme, term.Value); !seen[k] {
				seen[k] = true
				out = append(out, term)
			}
		}
		for _, id := range group.Includes {
			if included, ok := ContextGroupByID(id); ok {
				walk(included)
			}
		}
	}
	walk(g)
	return out
}

// Member returns the group's term for the code (by value and scheme), from
// the group or a group it includes, with the Code Meaning that group gives
// it: PS3.16 spells some codes differently in different groups (Long Axis
// in CID 7470, Long axis in CID 6), and the named constants carry one of
// them.
func (g ContextGroup) Member(c Code) (Term, bool) {
	for _, term := range g.Members() {
		if term.Value == c.Value && term.Scheme == c.Scheme {
			return term, true
		}
	}
	return Term{}, false
}

// Contains reports whether the code (by value and scheme) is a member of
// the group or of a group it includes. decided is false for a group, or an
// included group, defined by reference to an external resource.
func (g ContextGroup) Contains(c Code) (member, decided bool) {
	decided = true
	visited := map[int]bool{}
	var walk func(ContextGroup) bool
	walk = func(group ContextGroup) bool {
		if visited[group.ID] {
			return false
		}
		visited[group.ID] = true
		if group.DefinedByReference {
			decided = false
		}
		for _, term := range group.Codes {
			if term.Value == c.Value && term.Scheme == c.Scheme {
				return true
			}
		}
		for _, id := range group.Includes {
			if included, ok := ContextGroupByID(id); ok && walk(included) {
				return true
			}
		}
		return false
	}
	if walk(g) {
		return true, true
	}
	return false, decided
}

func parse(terms, groups string) tables {
	t := tables{terms: map[string]Term{}, groups: map[int]*ContextGroup{}, keywords: map[string]int{}}
	fail := func(format string, args ...any) tables {
		t.err = fmt.Errorf("codes: "+format, args...)
		return t
	}
	lines := bufio.NewScanner(strings.NewReader(terms))
	for n := 0; lines.Scan(); n++ {
		line := lines.Text()
		if n == 0 {
			rest, ok := strings.CutPrefix(line, "# DICOM ")
			if !ok {
				return fail("terms.tsv has no provenance line")
			}
			t.edition, t.sha256, _ = strings.Cut(rest, ", part16.xml sha256 ")
			continue
		}
		if line == "" || line[0] == '#' {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 4 {
			return fail("terms.tsv line %d", n+1)
		}
		t.terms[key(fields[0], fields[1])] = Term{Code: Code{Value: fields[1], Scheme: fields[0], Meaning: fields[2]}, Retired: fields[3] == "retired"}
	}
	var current *ContextGroup
	lines = bufio.NewScanner(strings.NewReader(groups))
	for n := 0; lines.Scan(); n++ {
		line := lines.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		fields := strings.Split(line, "\t")
		switch {
		case fields[0] == "CID" && len(fields) == 8:
			id, err := strconv.Atoi(fields[1])
			if err != nil {
				return fail("cid.tsv line %d: %v", n+1, err)
			}
			current = &ContextGroup{
				ID: id, Keyword: fields[2], Extensible: fields[3] == "extensible", Version: fields[4], UID: fields[5],
				Retired: strings.Contains(fields[6], "retired"), DefinedByReference: strings.Contains(fields[6], "no-table"), Name: fields[7],
			}
			t.groups[id] = current
			if current.Keyword != "" {
				t.keywords[current.Keyword] = id
			}
		case fields[0] == "include" && len(fields) == 2 && current != nil:
			id, err := strconv.Atoi(fields[1])
			if err != nil {
				return fail("cid.tsv line %d: %v", n+1, err)
			}
			current.Includes = append(current.Includes, id)
		case fields[0] == "code" && len(fields) == 5 && current != nil:
			current.Codes = append(current.Codes, Term{Code: Code{Value: fields[2], Scheme: fields[1], Meaning: fields[3]}, Retired: fields[4] == "retired"})
		default:
			return fail("cid.tsv line %d", n+1)
		}
	}
	return t
}

// Err reports a malformed embedded table; nil for the committed tables,
// which the package tests parse.
func Err() error { return load().err }
