package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/ThalesMMS/dicom-go/dictionary"
)

// The GDCM private dictionary is read only from a local copy of the pinned
// file; the importer refuses any other content.
const (
	gdcmProject      = "GDCM"
	gdcmVersion      = "GDCM-3.2.11-68-g2cd05d13"
	gdcmCommit       = "2cd05d13222b175e791b5ae3c08feeabcc1ed1f0"
	gdcmSourceFile   = "Source/DataDictionary/privatedicts.xml"
	gdcmSourceSHA256 = "d3744ac3678d4c089742d4cfc30616301f59da0ad528708f2a51e4332ef8fea4"
	gdcmRestrictions = "Retain the GDCM (Mathieu Malaterre, CREATIS) and dicom3tools (David A. Clunie DBA PixelMed Publishing) copyright notices, conditions and disclaimers in source/binary distributions; no endorsement. Definition is not permission to retain private data during de-identification."
	// maxUpstreamBytes bounds the upstream dictionaries read for import or
	// verification (privatedicts.xml is about 1 MiB).
	maxUpstreamBytes = 4 << 20
)

var gdcmExclusions = []string{
	"Only block-relative definitions (element xxNN or 00NN of a fixed odd group) with a single privatedicts.xml row are imported; repeating groups and HarvestedPrivate.xml (unnamed, mostly UN) are not.",
	`Uncertain names ("?" anywhere), names without a keyword (leading digit), UN, SQ, OB and OW definitions are refused.`,
	"Only the creator/group/offset keys listed in gdcm_selection.tsv are imported; each was unresolved in at least two gdcmData files without a contradicting Explicit VR (docs/CURATED_PRIVATE_CATALOG.md).",
}

func gdcmBaseURL(commit string) string {
	return "https://github.com/malaterre/GDCM/blob/" + commit
}

// gdcmEntry is one <entry/> of privatedicts.xml.
type gdcmEntry struct {
	XMLName xml.Name `xml:"entry"`
	Owner   string   `xml:"owner,attr"`
	Group   string   `xml:"group,attr"`
	Element string   `xml:"element,attr"`
	VR      string   `xml:"vr,attr"`
	VM      string   `xml:"vm,attr"`
	Name    string   `xml:"name,attr"`
}

// gdcmDefinition is a block-relative privatedicts.xml row.
type gdcmDefinition struct {
	entry         gdcmEntry
	group, offset string // canonical upper-case hex
}

var gdcmElement = regexp.MustCompile(`^(?:xx|00)([0-9A-Fa-f]{2})$`)

// parseGDCMRow parses one privatedicts.xml line holding a single
// block-relative <entry/>. GDCM writes the reserved-block offset either as
// xxNN or as 00NN; repeating groups (60xx) and other elements are not
// block-relative and are refused.
func parseGDCMRow(line string) (gdcmDefinition, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "<entry ") || !strings.HasSuffix(trimmed, "/>") || strings.Count(trimmed, "<") != 1 {
		return gdcmDefinition{}, false
	}
	var e gdcmEntry
	if err := xml.Unmarshal([]byte(trimmed), &e); err != nil {
		return gdcmDefinition{}, false
	}
	m := gdcmElement.FindStringSubmatch(e.Element)
	if m == nil || !hexGroup.MatchString(strings.ToUpper(e.Group)) || e.Owner == "" {
		return gdcmDefinition{}, false
	}
	return gdcmDefinition{entry: e, group: strings.ToUpper(e.Group), offset: strings.ToUpper(m[1])}, true
}

// gdcmKeyword derives a keyword from a GDCM display name: ASCII letters and
// digits are kept, every other character separates words, and each word
// starts with an upper-case letter ("Table Speed in mm/rot" gives
// "TableSpeedInMmRot"). It returns "" when the name is not a reviewable
// ASCII name or yields no valid keyword.
func gdcmKeyword(name string) string {
	// A name must start with a letter: GDCM marks placeholders with "?" or
	// "<internal", and a leading digit leaves no keyword.
	if name == "" || len(name) > 128 || strings.ContainsAny(name, "?<>") || strings.EqualFold(name, "Unknown") ||
		!(name[0] >= 'A' && name[0] <= 'Z' || name[0] >= 'a' && name[0] <= 'z') {
		return ""
	}
	var b strings.Builder
	upper := true
	for _, r := range name {
		switch {
		case r < 0x20 || r > 0x7e:
			return ""
		case r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9':
			if upper && r >= 'a' && r <= 'z' {
				r -= 'a' - 'A'
			}
			b.WriteRune(r)
			upper = false
		default:
			upper = true
		}
	}
	if !keyword.MatchString(b.String()) {
		return ""
	}
	return b.String()
}

// validGDCMRecord checks a definition reviewed from GDCM's privatedicts.xml:
// the row is the exact upstream line, the name is the upstream display name
// and the keyword is derived from it. It returns the failing field.
func validGDCMRecord(r sourceRecord) (string, bool) {
	o := r.Origin
	if !strings.HasPrefix(o.Version, "GDCM-") {
		return "pinned provenance", false
	}
	base := gdcmBaseURL(o.Commit)
	if o.SourceURL != base+"/"+gdcmSourceFile || o.LicenseURL != base+"/Copyright.txt" {
		return "source/license/review", false
	}
	if r.Keyword != gdcmKeyword(r.Name) {
		return "reviewed name", false
	}
	d, ok := parseGDCMRow(o.SourceRow)
	if !ok {
		return "upstream row", false
	}
	if d.entry.Owner != r.Creator || d.group != r.Group || d.offset != r.Offset || d.entry.VR != r.VR || d.entry.VM != r.VM || d.entry.Name != r.Name {
		return "definition contradicts source row", false
	}
	return "", true
}

func gdcmLines(data []byte) []string {
	return strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
}

// verifyGDCM checks the GDCM records of s against a local copy of the pinned
// privatedicts.xml: file hash, line and row, and the absence of any other
// definition of the same block-relative key.
func verifyGDCM(s source, data []byte) error {
	if len(data) > maxUpstreamBytes {
		return errSource
	}
	hash := sha256.Sum256(data)
	digest := hex.EncodeToString(hash[:])
	lines := gdcmLines(data)
	definitions := map[string]int{}
	for _, line := range lines {
		if d, ok := parseGDCMRow(line); ok {
			definitions[d.entry.Owner+"/"+d.group+"/"+d.offset]++
		}
	}
	for i, r := range s.Records {
		if r.Origin.Project != gdcmProject {
			continue
		}
		if r.Origin.SourceSHA256 != digest || r.Origin.SourceLine < 1 || r.Origin.SourceLine > len(lines) || lines[r.Origin.SourceLine-1] != r.Origin.SourceRow {
			return fmt.Errorf("%w: GDCM record %d differs", errSource, i)
		}
		if definitions[r.Creator+"/"+r.Group+"/"+r.Offset] != 1 {
			return fmt.Errorf("%w: GDCM conflict %d", errSource, i)
		}
	}
	return nil
}

// parseSelection reads the reviewed creator<TAB>group<TAB>offset keys; '#'
// lines are comments.
func parseSelection(data []byte) ([][3]string, error) {
	var keys [][3]string
	seen := map[[3]string]bool{}
	for n, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 3 {
			return nil, fmt.Errorf("%w: selection line %d", errSource, n+1)
		}
		k := [3]string{fields[0], fields[1], fields[2]}
		creator, err := dictionary.PrivateCreatorID(k[0])
		if err != nil || creator != k[0] || !hexGroup.MatchString(k[1]) || !hexOffset.MatchString(k[2]) || seen[k] {
			return nil, fmt.Errorf("%w: selection line %d", errSource, n+1)
		}
		seen[k] = true
		keys = append(keys, k)
	}
	if len(keys) == 0 || len(keys) > 4096 {
		return nil, fmt.Errorf("%w: empty selection", errSource)
	}
	return keys, nil
}

// importGDCMSource builds the reviewed JSON source of the selected keys from
// the pinned privatedicts.xml. Every selected key must have exactly one
// upstream row, and the resulting records must pass the reviewed-source
// validation; the importer never repairs or guesses a definition.
func importGDCMSource(xmlData, selection []byte) ([]byte, error) {
	if len(xmlData) > maxUpstreamBytes {
		return nil, errSource
	}
	hash := sha256.Sum256(xmlData)
	if digest := hex.EncodeToString(hash[:]); digest != gdcmSourceSHA256 {
		return nil, fmt.Errorf("%w: privatedicts.xml is not the pinned GDCM %s file (SHA-256 %s, want %s)", errSource, gdcmCommit[:8], digest, gdcmSourceSHA256)
	}
	return buildGDCMSource(xmlData, selection)
}

// refusedGDCMVR lists VRs that are never imported: a dictionary sequence
// would change how implicit VR data is parsed, and opaque or unknown VRs add
// nothing over the UN fallback.
var refusedGDCMVR = map[string]bool{"SQ": true, "OB": true, "OW": true, "UN": true}

func buildGDCMSource(xmlData, selection []byte) ([]byte, error) {
	hash := sha256.Sum256(xmlData)
	digest := hex.EncodeToString(hash[:])
	keys, err := parseSelection(selection)
	if err != nil {
		return nil, err
	}
	type row struct {
		line int
		text string
		def  gdcmDefinition
	}
	rows := map[[3]string][]row{}
	for n, line := range gdcmLines(xmlData) {
		if d, ok := parseGDCMRow(line); ok {
			k := [3]string{d.entry.Owner, d.group, d.offset}
			rows[k] = append(rows[k], row{n + 1, line, d})
		}
	}
	base := gdcmBaseURL(gdcmCommit)
	s := source{Schema: 1, Exclusions: gdcmExclusions}
	for _, k := range keys {
		found := rows[k]
		if len(found) != 1 {
			return nil, fmt.Errorf("%w: selected %q (%s,xx%s) has %d GDCM rows", errSource, k[0], k[1], k[2], len(found))
		}
		e := found[0].def.entry
		if refusedGDCMVR[e.VR] {
			return nil, fmt.Errorf("%w: selected %q (%s,xx%s) has refused VR %s", errSource, k[0], k[1], k[2], e.VR)
		}
		if gdcmKeyword(e.Name) == "" {
			return nil, fmt.Errorf("%w: selected %q (%s,xx%s) has no reviewable name", errSource, k[0], k[1], k[2])
		}
		s.Records = append(s.Records, sourceRecord{
			Creator: k[0], Group: k[1], Offset: k[2], VR: e.VR, VM: e.VM, Name: e.Name, Keyword: gdcmKeyword(e.Name),
			Origin: sourceOrigin{
				Project: gdcmProject, Version: gdcmVersion, Commit: gdcmCommit,
				SourceURL: base + "/" + gdcmSourceFile, SourceSHA256: digest,
				SourceLine: found[0].line, SourceRow: found[0].text,
				License: "BSD-3-Clause", LicenseURL: base + "/Copyright.txt",
				Restrictions: gdcmRestrictions, Verification: "reviewed-upstream",
			},
		})
	}
	if _, err := validatedRecords(s); err != nil {
		return nil, err
	}
	if err := verifyGDCM(s, xmlData); err != nil {
		return nil, err
	}
	slices.SortFunc(s.Records, func(a, b sourceRecord) int {
		if c := strings.Compare(a.Creator, b.Creator); c != 0 {
			return c
		}
		if c := strings.Compare(a.Group, b.Group); c != 0 {
			return c
		}
		return strings.Compare(a.Offset, b.Offset)
	})
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(s); err != nil {
		return nil, errSource
	}
	if b.Len() > maxSourceBytes {
		return nil, errSource
	}
	return b.Bytes(), nil
}
