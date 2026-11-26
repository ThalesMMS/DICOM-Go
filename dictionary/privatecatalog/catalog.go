// Package privatecatalog provides complete, pinned upstream private dictionary
// DATA as an explicitly constructed, immutable provider. It does not register
// definitions globally. Conflicting or contextual private VRs remain UN.
package privatecatalog

import (
	"errors"
	"fmt"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary"
)

// Source identifies imported DATA, not an implementation used by this package.
type Source string

const (
	DCMTK Source = "DCMTK"
	GDCM  Source = "GDCM"
)

var (
	ErrOptions       = errors.New("privatecatalog: invalid options")
	ErrLimitExceeded = errors.New("privatecatalog: resource limit exceeded")
	ErrSource        = errors.New("privatecatalog: invalid pinned source")
)

// Limits bound construction before allocation. Positive values may lower the
// built-in ceilings; zero uses the ceiling. Negative or larger values are invalid.
type Limits struct {
	MaxSourceBytes, MaxNormalizedBytes, MaxLineBytes int
	MaxRecords, MaxPatterns, MaxPartitions           int
}

// DefaultLimits returns finite ceilings, including duplicate record visits.
func DefaultLimits() Limits {
	return Limits{4 << 20, 8 << 20, 4096, 65536, 4096, 64}
}

// Options selects source DATA and construction limits. An empty Sources slice
// selects both sources. Source order affects neither VR conflict handling nor
// presentation metadata. Duplicate or unknown source identifiers are errors.
type Options struct {
	Sources []Source
	Limits  Limits
}

// SourceInfo identifies one complete DATA file and its distribution notices.
// Slice fields returned by Sources are caller-owned.
type SourceInfo struct {
	ID          Source   `json:"id"`
	Version     string   `json:"version"`
	Commit      string   `json:"commit"`
	Path        string   `json:"path"`
	DataFile    string   `json:"data_file"`
	SHA256      string   `json:"sha256"`
	Rows        int      `json:"rows"`
	SourceURL   string   `json:"source_url"`
	Licenses    []string `json:"licenses"`
	NoticeFiles []string `json:"notice_files"`
	LicenseURLs []string `json:"license_urls"`
}

// Disposition describes a source row or the outcome of a scoped lookup.
type Disposition string

const (
	Resolved            Disposition = "resolved"
	Opaque              Disposition = "opaque"
	Contextual          Disposition = "contextual-vr"
	VRConflict          Disposition = "vr-conflict"
	KnownCorpusConflict Disposition = "known-corpus-vr-conflict"
	InvalidDefinition   Disposition = "invalid-definition"
	UnsupportedPattern  Disposition = "unsupported-pattern"
	Missing             Disposition = "missing"
	InvalidRequest      Disposition = "invalid-request"
)

// Record preserves a literal upstream row, including unusable and duplicate
// definitions. Source resolves to Sources metadata. AllowedVRs is detached.
// Disposition is the row's status; conflicts with other rows are in Lookup.
type Record struct {
	Source                  Source
	Line                    int
	Row, RowSHA256          string
	Creator, Group, Element string
	VR, VM, Name            string
	AllowedVRs              []core.VR
	Disposition             Disposition
	Reason                  string
}

// Resolution retains every matching candidate. Keyword is always empty. VM is
// empty when candidates disagree on VM; that fact is independent of VR status.
type Resolution struct {
	Entry       dictionary.Entry
	Disposition Disposition
	VMConflict  bool
	Candidates  []Record
}

// Summary accounts for all upstream rows, not just effective definitions.
type Summary struct {
	Records       int
	BySource      map[Source]int
	ByDisposition map[Disposition]int
}

type candidate struct {
	record Record
	match  dictionary.DICEntry
	usable bool
}

// Catalog is immutable after construction and safe for concurrent lookups.
// Only verified dataset/item reservation scopes may provide its creator input.
type Catalog struct {
	sources   []SourceInfo
	rows      []candidate
	byCreator map[string][]int
	summary   Summary
}

var _ dictionary.PrivateTagDictionary = (*Catalog)(nil)

// New loads selected, embedded pinned DATA. Construction failures return nil;
// expected unusable upstream rows are inventoried rather than discarded.
func New(opts Options) (*Catalog, error) {
	limits, selected, err := normalizeOptions(opts)
	if err != nil {
		return nil, err
	}
	inputs, err := pinnedInputs(selected, limits)
	if err != nil {
		return nil, err
	}
	return build(inputs, limits)
}

func normalizeOptions(opts Options) (Limits, []Source, error) {
	l, ceiling := opts.Limits, DefaultLimits()
	for _, field := range []struct {
		value *int
		max   int
	}{
		{&l.MaxSourceBytes, ceiling.MaxSourceBytes}, {&l.MaxNormalizedBytes, ceiling.MaxNormalizedBytes},
		{&l.MaxLineBytes, ceiling.MaxLineBytes}, {&l.MaxRecords, ceiling.MaxRecords},
		{&l.MaxPatterns, ceiling.MaxPatterns}, {&l.MaxPartitions, ceiling.MaxPartitions},
	} {
		if *field.value < 0 || *field.value > field.max {
			return Limits{}, nil, ErrOptions
		}
		if *field.value == 0 {
			*field.value = field.max
		}
	}
	if len(opts.Sources) > 2 {
		return Limits{}, nil, ErrOptions
	}
	selected := append([]Source(nil), opts.Sources...)
	if len(selected) == 0 {
		selected = []Source{DCMTK, GDCM}
	}
	if len(selected) > 2 {
		return Limits{}, nil, ErrOptions
	}
	seen := map[Source]bool{}
	for _, id := range selected {
		if (id != DCMTK && id != GDCM) || seen[id] {
			return Limits{}, nil, ErrOptions
		}
		seen[id] = true
	}
	// Stable ordering is for reproducible presentation; it never resolves VRs.
	if len(selected) == 2 {
		selected = []Source{DCMTK, GDCM}
	}
	return l, selected, nil
}

func (*Catalog) ByTag(core.Tag) (dictionary.Entry, bool)   { return dictionary.Entry{}, false }
func (*Catalog) ByKeyword(string) (dictionary.Entry, bool) { return dictionary.Entry{}, false }
func (c *Catalog) HasPrivateDefinitions() bool             { return c != nil && len(c.byCreator) != 0 }

// ByPrivate resolves only block-relative records. Fixed records require the
// actual full element and are available through ByPrivateTag instead.
func (c *Catalog) ByPrivate(creator string, group uint16, offset uint8) (dictionary.Entry, bool) {
	r, ok := c.resolve(creator, core.NewTag(group, 0x1000|uint16(offset)), true, false)
	r.Entry.Tag = core.NewTag(group, uint16(offset))
	return r.Entry, ok
}
func (c *Catalog) ByPrivateTag(creator string, tag core.Tag) (dictionary.Entry, bool) {
	r, ok := c.resolve(creator, tag, false, false)
	return r.Entry, ok
}

// Lookup returns the conservative resolution and all matching candidate rows.
// Invalid inputs and missing definitions have no candidates and return UN.
func (c *Catalog) Lookup(creator string, tag core.Tag) Resolution {
	r, _ := c.resolve(creator, tag, false, true)
	return r
}

func (c *Catalog) resolve(creator string, tag core.Tag, relativeOnly, detail bool) (Resolution, bool) {
	r := Resolution{Entry: dictionary.Entry{Tag: tag, VR: core.VRUN}, Disposition: Missing}
	canonical, err := dictionary.PrivateCreatorID(creator)
	if err != nil || !dictionary.IsPrivateDataTag(tag) {
		r.Disposition = InvalidRequest
		return r, false
	}
	if c == nil {
		return r, false
	}
	found, conflict, contextual, known := false, false, false, false
	var vr core.VR
	for _, index := range c.byCreator[canonical] {
		v := &c.rows[index]
		if !v.usable || (relativeOnly && v.match.Fixed) || !v.match.Matches(tag) {
			continue
		}
		if !found {
			vr, r.Entry.Name, r.Entry.VM = v.match.Entry.VR, v.record.Name, v.record.VM
			found = true
		} else {
			conflict = conflict || vr != v.match.Entry.VR
			if r.Entry.VM != v.record.VM {
				r.VMConflict = true
			}
		}
		contextual = contextual || v.record.Disposition == Contextual
		known = known || v.record.Disposition == KnownCorpusConflict
		if detail {
			r.Candidates = append(r.Candidates, cloneRecord(v.record))
		}
	}
	if !found {
		return r, false
	}
	if r.VMConflict {
		r.Entry.VM = ""
	}
	switch {
	case known:
		r.Disposition = KnownCorpusConflict
	case contextual:
		r.Disposition = Contextual
	case conflict:
		r.Disposition = VRConflict
	case vr == core.VRUN:
		r.Disposition = Opaque
	default:
		r.Disposition, r.Entry.VR = Resolved, vr
	}
	return r, true
}

func cloneRecord(r Record) Record {
	r.AllowedVRs = append([]core.VR(nil), r.AllowedVRs...)
	return r
}
func cloneSource(s SourceInfo) SourceInfo {
	s.Licenses = append([]string(nil), s.Licenses...)
	s.NoticeFiles = append([]string(nil), s.NoticeFiles...)
	s.LicenseURLs = append([]string(nil), s.LicenseURLs...)
	return s
}
func (c *Catalog) Records() []Record {
	if c == nil {
		return nil
	}
	out := make([]Record, len(c.rows))
	for i := range c.rows {
		out[i] = cloneRecord(c.rows[i].record)
	}
	return out
}
func (c *Catalog) Sources() []SourceInfo {
	if c == nil {
		return nil
	}
	out := make([]SourceInfo, len(c.sources))
	for i := range c.sources {
		out[i] = cloneSource(c.sources[i])
	}
	return out
}
func (c *Catalog) Summary() Summary {
	s := Summary{BySource: map[Source]int{}, ByDisposition: map[Disposition]int{}}
	if c == nil {
		return s
	}
	s.Records = c.summary.Records
	for k, v := range c.summary.BySource {
		s.BySource[k] = v
	}
	for k, v := range c.summary.ByDisposition {
		s.ByDisposition[k] = v
	}
	return s
}

func knownContradiction(creator, group, element string) bool {
	group, element = strings.ToUpper(group), strings.ToUpper(element)
	// DCMTK also describes these legacy keys as exact fixed 10NN elements.
	// This identifies the row's corpus key only: its DIC matcher remains fixed
	// to the actual full element and never follows it into another block.
	if len(element) == 4 && strings.HasPrefix(element, "10") {
		element = element[2:]
	}
	if creator == "SIEMENS MED" && group == "0021" && element == "11" {
		return true
	}
	if creator == "SIEMENS CT VA0  COAD" && group == "0019" {
		return element == "90" || element == "92" || element == "93" || element == "96"
	}
	return creator == "PHILIPS MR/PART" && ((group == "0019" && element == "00") ||
		(group == "0021" && (element == "00" || element == "10")))
}

func limit(what string) error { return fmt.Errorf("%w: %s", ErrLimitExceeded, what) }
