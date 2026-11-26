package dictionary

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
)

// Resource defaults applied by ParseDIC and LoadDICFile when the matching
// DICOptions field is zero.
const (
	DefaultDICMaxFileSize   int64 = 16 << 20
	DefaultDICMaxLineLength       = 4096
	DefaultDICMaxEntries          = 65536
	DefaultDICMaxRepeating        = 4096
	DefaultDICMaxSkipped          = 1024
)

var (
	// ErrDICSyntax classifies a malformed .dic line: field count, tag, VM,
	// keyword, version or private creator.
	ErrDICSyntax = errors.New("dicom: invalid .dic dictionary entry")
	// ErrDICVR classifies a VR token that is not a standard VR or one of the
	// supported DCMTK dictionary aliases.
	ErrDICVR = errors.New("dicom: unsupported .dic dictionary VR")
	// ErrDICResourceLimit is returned when a file, line, entry, repeating-entry
	// or skipped-line limit is exceeded. It is fatal in tolerant mode too.
	ErrDICResourceLimit = errors.New("dicom: .dic dictionary resource limit exceeded")
	// ErrDICOptions reports negative limits.
	ErrDICOptions = errors.New("dicom: invalid .dic dictionary options")
)

// DICOptions configures ParseDIC and LoadDICFile. Zero limits select the
// DefaultDIC* values; negative limits are rejected.
type DICOptions struct {
	// Tolerant skips malformed lines and reports each one in
	// DICReport.Skipped instead of failing on the first one. Resource limits
	// remain fatal.
	Tolerant bool
	// MaxFileSize bounds the bytes read from the source.
	MaxFileSize int64
	// MaxLineLength bounds one physical line, excluding its terminator.
	MaxLineLength int
	// MaxEntries bounds accepted definitions, counting overridden ones.
	MaxEntries int
	// MaxRepeating bounds accepted tag-range definitions, which are scanned
	// linearly during lookup.
	MaxRepeating int
	// MaxSkipped bounds the malformed lines tolerated in tolerant mode.
	MaxSkipped int
}

func (o DICOptions) normalized() (DICOptions, error) {
	if o.MaxFileSize < 0 || o.MaxLineLength < 0 || o.MaxEntries < 0 || o.MaxRepeating < 0 || o.MaxSkipped < 0 {
		return o, ErrDICOptions
	}
	if o.MaxFileSize == 0 {
		o.MaxFileSize = DefaultDICMaxFileSize
	}
	if o.MaxLineLength == 0 {
		o.MaxLineLength = DefaultDICMaxLineLength
	}
	if o.MaxEntries == 0 {
		o.MaxEntries = DefaultDICMaxEntries
	}
	if o.MaxRepeating == 0 {
		o.MaxRepeating = DefaultDICMaxRepeating
	}
	if o.MaxSkipped == 0 {
		o.MaxSkipped = DefaultDICMaxSkipped
	}
	return o, nil
}

// DICLineError locates a problem in a .dic source. Field is "line", "tag",
// "vr", "keyword", "vm" or "version". Err is ErrDICSyntax, ErrDICVR or
// ErrDICResourceLimit, so errors.Is works on the returned error.
type DICLineError struct {
	Line   int
	Field  string
	Reason string
	Err    error
}

func (e *DICLineError) Error() string {
	return fmt.Sprintf("%v: line %d: %s: %s", e.Err, e.Line, e.Field, e.Reason)
}

func (e *DICLineError) Unwrap() error { return e.Err }

// DICReport summarizes a load. Skipped is only populated in tolerant mode.
type DICReport struct {
	Lines      int
	Entries    int
	Overridden int
	Skipped    []DICLineError
}

type dicParity uint8

const (
	dicAnyParity dicParity = iota
	dicEven
	dicOdd
)

// dicPart matches one tag component: an inclusive range restricted by parity,
// or a nibble pattern (mask selects the literal hex digits of lo).
type dicPart struct {
	lo, hi uint16
	mask   uint16
	parity dicParity
}

func (p dicPart) exact() bool { return p.lo == p.hi }

func (p dicPart) matches(v uint16) bool {
	if v < p.lo || v > p.hi || v&p.mask != p.lo&p.mask {
		return false
	}
	switch p.parity {
	case dicEven:
		return v&1 == 0
	case dicOdd:
		return v&1 == 1
	}
	return true
}

// first returns the smallest matching value; count reports the number of
// matching values. Both are exact for ranges and nibble patterns.
func (p dicPart) first() (uint16, bool) {
	v := p.lo
	if !p.matches(v) {
		v++ // only the parity of the lowest value can fail
	}
	if v < p.lo || !p.matches(v) {
		return 0, false
	}
	return v, true
}

func (p dicPart) count() uint64 {
	if _, ok := p.first(); !ok {
		return 0
	}
	if p.mask != 0 || p.lo == p.hi {
		wild := 0
		for shift := 0; shift < 16; shift += 4 {
			if (p.mask>>shift)&0xf == 0 && (p.hi>>shift)&0xf == 0xf && (p.lo>>shift)&0xf == 0 {
				wild++
			}
		}
		n := uint64(1) << (4 * wild)
		if p.parity != dicAnyParity && p.mask&0xf == 0 && p.lo != p.hi {
			n /= 2
		}
		return n
	}
	first, _ := p.first()
	span := uint64(p.hi - first)
	if p.parity == dicAnyParity {
		return span + 1
	}
	return span/2 + 1
}

// DICEntry is one effective definition loaded from a .dic source, in file
// order. Entry.Tag is the first matching tag of an absolute definition, or the
// block-relative tag (group, offset) of a private definition. VMMax is -1 for
// an unbounded multiplicity.
type DICEntry struct {
	Line      int
	TagField  string
	Creator   string
	Fixed     bool // private definition bound to one element (eeee >= 1000)
	Repeating bool
	VRToken   string
	VRSpec    VRSpec
	Version   string
	VMMin     int
	VMMax     int
	VMStep    int
	Entry     Entry

	group, element dicPart
	specificity    uint64
}

// Matches reports whether tag has the shape of this definition. For private
// definitions it ignores the creator, which only a dataset scope can supply.
func (e DICEntry) Matches(tag core.Tag) bool {
	if !e.group.matches(tag.Group) {
		return false
	}
	if e.Creator == "" || e.Fixed {
		return e.element.matches(tag.Element)
	}
	return IsPrivateDataTag(tag) && e.element.matches(tag.Element&0xff)
}

type dicKey struct {
	creator        string
	fixed          bool
	group, element dicPart
}

type dicPrivateKey struct {
	creator        string
	fixed          bool
	group, element uint16
}

// DICDictionary is an immutable dictionary loaded from DCMTK's text format.
// It implements DataDictionary, VRSpecDictionary and PrivateDataDictionary and
// is safe for concurrent lookups. Compose it with Chain: put it before
// std.Dictionary to override standard definitions, after it to only extend.
type DICDictionary struct {
	entries      []DICEntry
	exact        map[core.Tag]int
	groupRanges  map[uint16][]int
	wideRanges   []int
	keywords     map[string]int
	private      map[dicPrivateKey]int
	fixedRanges  []int
	offsetRanges []int
}

var (
	_ VRSpecDictionary      = (*DICDictionary)(nil)
	_ PrivateDataDictionary = (*DICDictionary)(nil)
	_ PrivateTagDictionary  = (*DICDictionary)(nil)
)

// LoadDICFile opens a regular file and parses it with ParseDIC. The file size
// is checked against MaxFileSize before any line is read.
func LoadDICFile(path string, opts DICOptions) (*DICDictionary, DICReport, error) {
	norm, err := opts.normalized()
	if err != nil {
		return nil, DICReport{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, DICReport{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, DICReport{}, err
	}
	if !info.Mode().IsRegular() {
		return nil, DICReport{}, fmt.Errorf("dicom: .dic dictionary %s is not a regular file", path)
	}
	if info.Size() > norm.MaxFileSize {
		return nil, DICReport{}, fmt.Errorf("%w: file has %d bytes, limit %d", ErrDICResourceLimit, info.Size(), norm.MaxFileSize)
	}
	return ParseDIC(f, opts)
}

// dicSizeReader delivers at most max bytes. When the source is longer it
// records the overrun; ParseDIC then stops before interpreting a truncated
// final line.
type dicSizeReader struct {
	r         io.Reader
	read, max int64
	exceeded  bool
}

func (s *dicSizeReader) err() error {
	return fmt.Errorf("%w: source exceeds %d bytes", ErrDICResourceLimit, s.max)
}

func (s *dicSizeReader) Read(p []byte) (int, error) {
	if s.exceeded {
		return 0, s.err()
	}
	if len(p) == 0 {
		return 0, nil
	}
	if s.read == s.max {
		var probe [1]byte
		n, err := io.ReadAtLeast(s.r, probe[:], 1)
		if n > 0 {
			s.exceeded = true
			return 0, s.err()
		}
		if err == io.ErrUnexpectedEOF {
			err = io.EOF
		}
		return 0, err
	}
	if room := s.max - s.read; int64(len(p)) > room {
		p = p[:room]
	}
	n, err := s.r.Read(p)
	s.read += int64(n)
	return n, err
}

// ParseDIC reads DCMTK's tab-separated data dictionary format: Tag, VR, Name
// (keyword), VM and Version, one definition per line; '#' starts a comment
// line. See docs/PRIVATE_CREATORS.md for the accepted grammar. In strict mode
// (the default) the first malformed line fails the load with a
// *DICLineError; with Tolerant, malformed lines are skipped and reported.
// Later definitions of the same tag or range override earlier ones.
func ParseDIC(r io.Reader, opts DICOptions) (*DICDictionary, DICReport, error) {
	var report DICReport
	opts, err := opts.normalized()
	if err != nil {
		return nil, report, err
	}
	src := &dicSizeReader{r: r, max: opts.MaxFileSize}
	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 0, min(4096, opts.MaxLineLength+2)), opts.MaxLineLength+2)
	var defs []DICEntry
	repeating := 0
	for scanner.Scan() {
		if src.exceeded {
			return nil, report, src.err()
		}
		report.Lines++
		raw := scanner.Text()
		if len(raw) > opts.MaxLineLength {
			return nil, report, &DICLineError{Line: report.Lines, Field: "line", Reason: fmt.Sprintf("longer than %d bytes", opts.MaxLineLength), Err: ErrDICResourceLimit}
		}
		line := strings.TrimRight(raw, " \t\r")
		body := strings.TrimLeft(line, " \t")
		if body == "" || body[0] == '#' {
			continue
		}
		def, lineErr := parseDICLine(line)
		if lineErr != nil {
			lineErr.Line = report.Lines
			if !opts.Tolerant {
				return nil, report, lineErr
			}
			if len(report.Skipped) >= opts.MaxSkipped {
				return nil, report, &DICLineError{Line: report.Lines, Field: "line", Reason: fmt.Sprintf("more than %d malformed lines", opts.MaxSkipped), Err: ErrDICResourceLimit}
			}
			report.Skipped = append(report.Skipped, *lineErr)
			continue
		}
		if len(defs) >= opts.MaxEntries {
			return nil, report, &DICLineError{Line: report.Lines, Field: "line", Reason: fmt.Sprintf("more than %d entries", opts.MaxEntries), Err: ErrDICResourceLimit}
		}
		if def.Repeating {
			if repeating >= opts.MaxRepeating {
				return nil, report, &DICLineError{Line: report.Lines, Field: "tag", Reason: fmt.Sprintf("more than %d repeating entries", opts.MaxRepeating), Err: ErrDICResourceLimit}
			}
			repeating++
		}
		def.Line = report.Lines
		defs = append(defs, def)
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return nil, report, &DICLineError{Line: report.Lines + 1, Field: "line", Reason: fmt.Sprintf("longer than %d bytes", opts.MaxLineLength), Err: ErrDICResourceLimit}
		}
		return nil, report, err
	}
	report.Entries = len(defs)
	d, overridden := buildDICDictionary(defs)
	report.Overridden = overridden
	return d, report, nil
}

func buildDICDictionary(defs []DICEntry) (*DICDictionary, int) {
	latest := make(map[dicKey]int, len(defs))
	for i, def := range defs {
		latest[dicKey{def.Creator, def.Fixed, def.group, def.element}] = i
	}
	d := &DICDictionary{
		exact:       make(map[core.Tag]int),
		groupRanges: make(map[uint16][]int),
		keywords:    make(map[string]int),
		private:     make(map[dicPrivateKey]int),
	}
	for i, def := range defs {
		if latest[dicKey{def.Creator, def.Fixed, def.group, def.element}] == i {
			d.entries = append(d.entries, def)
		}
	}
	for i, def := range d.entries {
		switch {
		case def.Creator != "" && !def.Repeating:
			d.private[dicPrivateKey{def.Creator, def.Fixed, def.group.lo, def.element.lo}] = i
		case def.Creator != "" && def.Fixed:
			d.fixedRanges = append(d.fixedRanges, i)
		case def.Creator != "":
			d.offsetRanges = append(d.offsetRanges, i)
		case !def.Repeating:
			d.exact[def.Entry.Tag] = i
		case def.group.exact():
			d.groupRanges[def.group.lo] = append(d.groupRanges[def.group.lo], i)
		default:
			d.wideRanges = append(d.wideRanges, i)
		}
		if def.Creator == "" {
			key := strings.ToLower(def.Entry.Keyword)
			if prev, ok := d.keywords[key]; !ok || def.Repeating == d.entries[prev].Repeating || d.entries[prev].Repeating {
				d.keywords[key] = i
			}
		}
	}
	// Narrower ranges win; among equally narrow ones the later line wins.
	order := func(list []int) {
		sort.SliceStable(list, func(a, b int) bool {
			ea, eb := d.entries[list[a]], d.entries[list[b]]
			if ea.specificity != eb.specificity {
				return ea.specificity < eb.specificity
			}
			return ea.Line > eb.Line
		})
	}
	for _, list := range d.groupRanges {
		order(list)
	}
	order(d.wideRanges)
	order(d.fixedRanges)
	order(d.offsetRanges)
	return d, len(defs) - len(d.entries)
}

// Len returns the number of effective definitions.
func (d *DICDictionary) Len() int {
	if d == nil {
		return 0
	}
	return len(d.entries)
}

// Entries returns a caller-owned copy of the effective definitions in file
// order; definitions overridden by a later identical tag field are omitted.
func (d *DICDictionary) Entries() []DICEntry {
	if d == nil {
		return nil
	}
	return append([]DICEntry(nil), d.entries...)
}

// HasPrivateDefinitions reports whether any creator-bound definition was
// loaded. HasPrivateDictionary uses it so that a file with only absolute
// definitions does not activate private-creator resolution.
func (d *DICDictionary) HasPrivateDefinitions() bool {
	return d != nil && (len(d.private) > 0 || len(d.fixedRanges) > 0 || len(d.offsetRanges) > 0)
}

func (d *DICDictionary) lookupAbsolute(tag core.Tag) (int, bool) {
	if d == nil {
		return 0, false
	}
	if i, ok := d.exact[tag]; ok {
		return i, true
	}
	best := -1
	pick := func(list []int) {
		for _, i := range list {
			if d.entries[i].Matches(tag) {
				if best < 0 || d.better(i, best) {
					best = i
				}
				return
			}
		}
	}
	pick(d.groupRanges[tag.Group])
	pick(d.wideRanges)
	return best, best >= 0
}

func (d *DICDictionary) better(a, b int) bool {
	ea, eb := d.entries[a], d.entries[b]
	if ea.specificity != eb.specificity {
		return ea.specificity < eb.specificity
	}
	return ea.Line > eb.Line
}

// ByTag resolves absolute definitions: exact tags first, then the narrowest
// matching repeating range. Creator-bound definitions never match here.
func (d *DICDictionary) ByTag(tag core.Tag) (Entry, bool) {
	i, ok := d.lookupAbsolute(tag)
	if !ok {
		return Entry{}, false
	}
	e := d.entries[i].Entry
	e.Tag = tag
	return e, true
}

// ByKeyword resolves absolute definitions case-insensitively. A repeating
// definition reports its first matching tag.
func (d *DICDictionary) ByKeyword(keyword string) (Entry, bool) {
	if d == nil {
		return Entry{}, false
	}
	i, ok := d.keywords[strings.ToLower(keyword)]
	if !ok {
		return Entry{}, false
	}
	return d.entries[i].Entry, true
}

// VRSpecByTag preserves the alternatives of DCMTK's contextual VR tokens.
func (d *DICDictionary) VRSpecByTag(tag core.Tag) (VRSpec, bool) {
	i, ok := d.lookupAbsolute(tag)
	if !ok || d.entries[i].VRSpec.count == 0 {
		return VRSpec{}, false
	}
	return d.entries[i].VRSpec, true
}

// ByPrivate resolves block-relative private definitions ((gggg,"CREATOR",ee)).
// Fixed-element definitions need the element and resolve via ByPrivateTag.
func (d *DICDictionary) ByPrivate(creator string, group uint16, offset uint8) (Entry, bool) {
	if d == nil {
		return Entry{}, false
	}
	creator, err := PrivateCreatorID(creator)
	if err != nil {
		return Entry{}, false
	}
	i, ok := d.private[dicPrivateKey{creator, false, group, uint16(offset)}]
	if !ok {
		i, ok = d.firstPrivate(d.offsetRanges, creator, group, uint16(offset))
	}
	if !ok {
		return Entry{}, false
	}
	e := d.entries[i].Entry
	e.Tag = core.NewTag(group, uint16(offset))
	return e, true
}

// ByPrivateTag resolves a private data element whose creator the caller
// already verified. Fixed-element definitions precede block-relative ones.
func (d *DICDictionary) ByPrivateTag(creator string, tag core.Tag) (Entry, bool) {
	if d == nil || !IsPrivateDataTag(tag) {
		return Entry{}, false
	}
	canonical, err := PrivateCreatorID(creator)
	if err != nil {
		return Entry{}, false
	}
	i, ok := d.private[dicPrivateKey{canonical, true, tag.Group, tag.Element}]
	if !ok {
		i, ok = d.firstPrivate(d.fixedRanges, canonical, tag.Group, tag.Element)
	}
	if !ok {
		e, found := d.ByPrivate(canonical, tag.Group, uint8(tag.Element))
		if found {
			e.Tag = tag
		}
		return e, found
	}
	e := d.entries[i].Entry
	e.Tag = tag
	return e, true
}

func (d *DICDictionary) firstPrivate(list []int, creator string, group, element uint16) (int, bool) {
	for _, i := range list {
		def := d.entries[i]
		if def.Creator == creator && def.group.matches(group) && def.element.matches(element) {
			return i, true
		}
	}
	return 0, false
}

func dicErr(field string, err error, format string, args ...any) *DICLineError {
	return &DICLineError{Field: field, Reason: fmt.Sprintf(format, args...), Err: err}
}

// dicQuote bounds attacker-controlled text echoed in diagnostics.
func dicQuote(s string) string {
	if len(s) > 64 {
		s = s[:64] + "..."
	}
	return strconv.Quote(s)
}

func parseDICLine(line string) (DICEntry, *DICLineError) {
	for i := 0; i < len(line); i++ {
		if c := line[i]; (c < 0x20 && c != '\t') || c > 0x7e {
			return DICEntry{}, dicErr("line", ErrDICSyntax, "byte 0x%02X at column %d is not printable ASCII", c, i+1)
		}
	}
	var fields []string
	for _, f := range strings.Split(line, "\t") {
		if f = strings.Trim(f, " "); f != "" {
			fields = append(fields, f)
		}
	}
	if len(fields) < 3 || len(fields) > 5 {
		return DICEntry{}, dicErr("line", ErrDICSyntax, "%d tab-separated fields, want Tag, VR, Name[, VM[, Version]]", len(fields))
	}
	def := DICEntry{TagField: fields[0], VRToken: fields[1], Version: "DICOM"}
	if err := parseDICTag(&def); err != nil {
		return DICEntry{}, err
	}
	vr, spec, err := parseDICVR(fields[1], def)
	if err != nil {
		return DICEntry{}, err
	}
	keyword := fields[2]
	retired := false
	if rest, ok := strings.CutPrefix(keyword, "RETIRED_"); ok && rest != "" {
		keyword, retired = rest, true
	}
	vm := "1"
	if len(fields) > 3 {
		vm = fields[3]
	}
	if def.VMMin, def.VMMax, def.VMStep, err = parseDICVM(vm); err != nil {
		return DICEntry{}, err
	}
	if len(fields) > 4 {
		def.Version = fields[4]
	}
	if strings.Contains(strings.ToLower(def.Version), "retired") {
		retired = true
	}
	def.VRSpec = spec
	def.Entry.VR = vr
	def.Entry.Keyword = keyword
	def.Entry.Name = dicKeywordToName(keyword)
	def.Entry.VM = vm
	def.Entry.Retired = retired
	return def, nil
}

func parseDICTag(def *DICEntry) *DICLineError {
	s := def.TagField
	if len(s) < 2 || s[0] != '(' || s[len(s)-1] != ')' {
		return dicErr("tag", ErrDICSyntax, "%s is not enclosed in parentheses", dicQuote(s))
	}
	body := s[1 : len(s)-1]
	comma := strings.IndexByte(body, ',')
	if comma < 0 {
		return dicErr("tag", ErrDICSyntax, "%s has no element part", dicQuote(s))
	}
	groupText, rest := body[:comma], strings.TrimLeft(body[comma+1:], " ")
	creatorText, private := "", false
	if strings.HasPrefix(rest, "\"") {
		end := strings.IndexByte(rest[1:], '"')
		if end < 0 {
			return dicErr("tag", ErrDICSyntax, "%s has an unterminated private creator", dicQuote(s))
		}
		creatorText, private = rest[1:1+end], true
		rest = strings.TrimLeft(rest[2+end:], " ")
		if !strings.HasPrefix(rest, ",") {
			return dicErr("tag", ErrDICSyntax, "%s has no element after the private creator", dicQuote(s))
		}
		rest = rest[1:]
	}
	if strings.ContainsAny(rest, "(),\"") || strings.ContainsAny(groupText, "()\"") {
		return dicErr("tag", ErrDICSyntax, "%s is malformed", dicQuote(s))
	}
	group, gWidth, reason := parseDICPart(groupText, true)
	if reason != "" {
		return dicErr("tag", ErrDICSyntax, "group of %s: %s", dicQuote(s), reason)
	}
	if gWidth != 4 {
		return dicErr("tag", ErrDICSyntax, "group of %s must have 4 hex digits", dicQuote(s))
	}
	element, eWidth, reason := parseDICPart(rest, false)
	if reason != "" {
		return dicErr("tag", ErrDICSyntax, "element of %s: %s", dicQuote(s), reason)
	}
	def.group, def.element = group, element
	def.Repeating = !group.exact() || !element.exact()
	if !private {
		if eWidth != 4 {
			return dicErr("tag", ErrDICSyntax, "element of %s must have 4 hex digits", dicQuote(s))
		}
	} else {
		creator, err := PrivateCreatorID(creatorText)
		if err != nil {
			return dicErr("tag", ErrDICSyntax, "private creator %s is not a valid LO identifier", dicQuote(creatorText))
		}
		def.Creator = creator
		if first, ok := group.first(); !ok || (!group.exact() && group.parity != dicOdd) || !IsPrivateGroup(first) {
			return dicErr("tag", ErrDICSyntax, "%s: private definitions need odd groups from 0009 (use gggg-o-gggg for ranges)", dicQuote(s))
		}
		switch {
		case element.hi <= 0x00ff:
		case element.lo >= 0x1000:
			def.Fixed = true
		default:
			return dicErr("tag", ErrDICSyntax, "%s: private element must be an offset 00-FF or a data element 1000-FFFF", dicQuote(s))
		}
	}
	gFirst, gOK := group.first()
	eFirst, eOK := element.first()
	if !gOK || !eOK {
		return dicErr("tag", ErrDICSyntax, "%s matches no tag", dicQuote(s))
	}
	def.specificity = group.count() * element.count()
	def.Entry.Tag = core.NewTag(gFirst, eFirst)
	return nil
}

// parseDICPart parses "gggg", "gggg-hhhh" (even values only), "gggg-o-hhhh",
// "gggg-e-hhhh", "gggg-u-hhhh" or a PS3.6-style nibble pattern such as "60xx"
// (groups: even values only). Private offsets may use two hex digits.
func parseDICPart(text string, isGroup bool) (dicPart, int, string) {
	text = strings.Trim(text, " ")
	parts := strings.Split(text, "-")
	switch len(parts) {
	case 1:
		if strings.ContainsAny(text, "xX") {
			if len(text) != 4 {
				return dicPart{}, 0, fmt.Sprintf("pattern %s must have 4 digits", dicQuote(text))
			}
			var p dicPart
			for i := 0; i < 4; i++ {
				shift := uint(12 - 4*i)
				if c := text[i]; c == 'x' || c == 'X' {
					p.hi |= 0xf << shift
					continue
				}
				n, ok := dicHexDigit(text[i])
				if !ok {
					return dicPart{}, 0, fmt.Sprintf("%s is not hexadecimal", dicQuote(text))
				}
				p.lo |= n << shift
				p.hi |= n << shift
				p.mask |= 0xf << shift
			}
			if isGroup {
				p.parity = dicEven
			}
			return p, 4, ""
		}
		v, width, ok := dicHex(text)
		if !ok {
			return dicPart{}, 0, fmt.Sprintf("%s is not 2 or 4 hex digits", dicQuote(text))
		}
		return dicPart{lo: v, hi: v}, width, ""
	case 2, 3:
		p := dicPart{parity: dicEven}
		if len(parts) == 3 {
			switch strings.ToLower(parts[1]) {
			case "o":
				p.parity = dicOdd
			case "e":
			case "u":
				p.parity = dicAnyParity
			default:
				return dicPart{}, 0, fmt.Sprintf("unknown range restriction %s (want o, e or u)", dicQuote(parts[1]))
			}
		}
		lo, lw, ok1 := dicHex(strings.Trim(parts[0], " "))
		hi, hw, ok2 := dicHex(strings.Trim(parts[len(parts)-1], " "))
		if !ok1 || !ok2 || lw != hw {
			return dicPart{}, 0, fmt.Sprintf("range %s needs two hex bounds of equal width", dicQuote(text))
		}
		if lo > hi {
			return dicPart{}, 0, fmt.Sprintf("range %s is descending", dicQuote(text))
		}
		p.lo, p.hi = lo, hi
		return p, lw, ""
	}
	return dicPart{}, 0, fmt.Sprintf("%s is malformed", dicQuote(text))
}

func dicHexDigit(c byte) (uint16, bool) {
	switch {
	case c >= '0' && c <= '9':
		return uint16(c - '0'), true
	case c >= 'a' && c <= 'f':
		return uint16(c-'a') + 10, true
	case c >= 'A' && c <= 'F':
		return uint16(c-'A') + 10, true
	}
	return 0, false
}

func dicHex(s string) (uint16, int, bool) {
	if len(s) != 2 && len(s) != 4 {
		return 0, 0, false
	}
	var v uint16
	for i := 0; i < len(s); i++ {
		n, ok := dicHexDigit(s[i])
		if !ok {
			return 0, 0, false
		}
		v = v<<4 | n
	}
	return v, len(s), true
}

func parseDICVR(token string, def DICEntry) (core.VR, VRSpec, *DICLineError) {
	switch token {
	case "xs", "ox", "px", "lt", "up":
		spec := NewContextualVRSpec(ContextualVR(token))
		fallback := map[string]core.VR{"xs": core.VRUS, "ox": core.VROW, "px": core.VROW, "lt": core.VROW, "up": core.VRUL}[token]
		return fallback, spec, nil
	case "na":
		// Items and delimiters have no VR; nothing else may drop it.
		if def.Creator != "" || def.group.lo != 0xfffe || def.group.hi != 0xfffe {
			return "", VRSpec{}, dicErr("vr", ErrDICVR, `"na" is only valid for group FFFE items and delimiters`)
		}
		return "", VRSpec{}, nil
	}
	vr, err := core.ParseVR(token)
	if err != nil {
		return "", VRSpec{}, dicErr("vr", ErrDICVR, "%s is not a standard VR or one of xs, ox, px, lt, up, na", dicQuote(token))
	}
	return vr, NewVRSpec(vr), nil
}

// parseDICVM accepts "n", "N", "Nn", "N-M", "N-n" and "N-Kn" (for example
// 1-n, 2-2n or 3-3n). An unbounded maximum is reported as -1 with step K.
func parseDICVM(vm string) (int, int, int, *DICLineError) {
	bad := func() (int, int, int, *DICLineError) {
		return 0, 0, 0, dicErr("vm", ErrDICSyntax, "%s is not a VM such as 1, 1-3, 1-n or 2-2n", dicQuote(vm))
	}
	num := func(s string) (int, bool) {
		if len(s) == 0 || len(s) > 6 || strings.Trim(s, "0123456789") != "" {
			return 0, false
		}
		n, err := strconv.Atoi(s)
		return n, err == nil && n > 0
	}
	unbounded := func(s string) (int, bool) {
		if !strings.HasSuffix(s, "n") && !strings.HasSuffix(s, "N") {
			return 0, false
		}
		if s = s[:len(s)-1]; s == "" {
			return 1, true
		}
		return num(s)
	}
	lo, hi, ok := strings.Cut(vm, "-")
	if !ok {
		if n, ok := num(lo); ok {
			return n, n, 1, nil
		}
		if n, ok := unbounded(lo); ok {
			if lo == "n" || lo == "N" {
				return 1, -1, 1, nil
			}
			return n, -1, 1, nil
		}
		return bad()
	}
	minVM, ok := num(lo)
	if !ok {
		return bad()
	}
	if step, ok := unbounded(hi); ok {
		return minVM, -1, step, nil
	}
	maxVM, ok := num(hi)
	if !ok || maxVM < minVM {
		return bad()
	}
	return minVM, maxVM, 1, nil
}

// dicKeywordToName derives a readable name from a CamelCase keyword, as the
// standard dictionary does. Keywords with other characters are kept verbatim.
func dicKeywordToName(keyword string) string {
	for i := 0; i < len(keyword); i++ {
		c := keyword[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return keyword
		}
	}
	isUpper := func(c byte) bool { return c >= 'A' && c <= 'Z' }
	isLower := func(c byte) bool { return c >= 'a' && c <= 'z' }
	var b strings.Builder
	for i := 0; i < len(keyword); i++ {
		c := keyword[i]
		if i > 0 && isUpper(c) {
			prev := keyword[i-1]
			if !isUpper(prev) || (i+1 < len(keyword) && isLower(keyword[i+1])) {
				b.WriteByte(' ')
			}
		}
		b.WriteByte(c)
	}
	return b.String()
}
