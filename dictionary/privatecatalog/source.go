package privatecatalog

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary"
)

// Only DATA and distribution notices are embedded, never upstream code.
//
//go:embed data/private.dic data/privatedicts.xml data/sources.json LICENSE.DCMTK LICENSE.GDCM LICENSE.dicom3tools
var files embed.FS

type sourceInput struct {
	info SourceInfo
	data []byte
}

func digest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

func pinnedInputs(selected []Source, limits Limits) ([]sourceInput, error) {
	metadata, err := files.ReadFile("data/sources.json")
	if err != nil || len(metadata) > 16384 {
		return nil, fmt.Errorf("%w: metadata", ErrSource)
	}
	var manifest struct {
		Schema  int          `json:"schema_version"`
		Sources []SourceInfo `json:"sources"`
	}
	if err := json.Unmarshal(metadata, &manifest); err != nil || manifest.Schema != 1 || len(manifest.Sources) != 2 {
		return nil, fmt.Errorf("%w: metadata schema", ErrSource)
	}
	out := make([]sourceInput, 0, len(selected))
	rows := 0
	for _, id := range selected {
		var info SourceInfo
		for _, s := range manifest.Sources {
			if s.ID == id {
				info = s
			}
		}
		expectedFile := "data/private.dic"
		if id == GDCM {
			expectedFile = "data/privatedicts.xml"
		}
		if info.ID != id || info.DataFile != expectedFile || len(info.SHA256) != 64 || info.Rows <= 0 {
			return nil, fmt.Errorf("%w: metadata for %s", ErrSource, id)
		}
		if info.Rows > limits.MaxRecords-rows {
			return nil, limit("records")
		}
		rows += info.Rows
		f, err := files.Open(info.DataFile)
		if err != nil {
			return nil, fmt.Errorf("%w: %s", ErrSource, id)
		}
		stat, statErr := f.Stat()
		if statErr != nil || stat.Size() > int64(limits.MaxSourceBytes) {
			f.Close()
			if statErr != nil {
				return nil, fmt.Errorf("%w: stat", ErrSource)
			}
			return nil, limit("source bytes")
		}
		data, readErr := io.ReadAll(io.LimitReader(f, int64(limits.MaxSourceBytes)+1))
		f.Close()
		if readErr != nil {
			return nil, fmt.Errorf("%w: read", ErrSource)
		}
		if len(data) > limits.MaxSourceBytes {
			return nil, limit("source bytes")
		}
		if digest(data) != info.SHA256 {
			return nil, fmt.Errorf("%w: SHA-256 for %s", ErrSource, id)
		}
		out = append(out, sourceInput{info, data})
	}
	return out, nil
}

// LicenseNotice returns complete applicable upstream notices for distributors
// of binaries using this opt-in DATA. It does not require constructing a catalog.
func LicenseNotice() string {
	var b strings.Builder
	for _, name := range []string{"LICENSE.DCMTK", "LICENSE.GDCM", "LICENSE.dicom3tools"} {
		data, _ := files.ReadFile(name) // fixed embedded files
		b.WriteString(name + "\n")
		b.Write(data)
		b.WriteByte('\n')
	}
	return b.String()
}

type partition struct {
	lines   []string
	indices []int
}

func build(inputs []sourceInput, limits Limits) (*Catalog, error) {
	totalRows := 0
	for _, input := range inputs {
		if len(input.data) > limits.MaxSourceBytes {
			return nil, limit("source bytes")
		}
		if input.info.Rows <= 0 || input.info.Rows > limits.MaxRecords-totalRows {
			return nil, limit("records")
		}
		totalRows += input.info.Rows
	}
	// The complete inventory size is checked before allocating its storage;
	// capacity growth cannot overshoot a caller's accepted record budget.
	c := &Catalog{byCreator: map[string][]int{}, rows: make([]candidate, 0, totalRows)}
	normalizedBytes, patterns := 0, 0
	for _, input := range inputs {
		if digest(input.data) != input.info.SHA256 {
			return nil, fmt.Errorf("%w: SHA-256", ErrSource)
		}
		c.sources = append(c.sources, cloneSource(input.info))
		before := len(c.rows)
		scanner := bufio.NewScanner(bytes.NewReader(input.data))
		scanner.Buffer(make([]byte, 0, min(4096, limits.MaxLineBytes+2)), limits.MaxLineBytes+2)
		line := 0
		var partitions []partition
		occurrences := map[string]int{}
		for scanner.Scan() {
			line++
			if len(scanner.Bytes()) > limits.MaxLineBytes {
				return nil, limit("line bytes")
			}
			raw := scanner.Text()
			body := strings.TrimSpace(raw)
			if (input.info.ID == DCMTK && !strings.HasPrefix(body, "(")) ||
				(input.info.ID == GDCM && !strings.HasPrefix(body, "<entry")) {
				continue
			}
			if len(c.rows) == limits.MaxRecords {
				return nil, limit("records")
			}
			r := Record{Source: input.info.ID, Line: line, Row: raw, RowSHA256: digest([]byte(raw))}
			parseRow(&r)
			normalized, key := normalizeRow(&r)
			if patternRow(r) {
				if patterns == limits.MaxPatterns {
					return nil, limit("patterns")
				}
				patterns++
			}
			index := len(c.rows)
			c.rows = append(c.rows, candidate{record: r})
			if normalized == "" {
				continue
			}
			if len(normalized) > limits.MaxLineBytes {
				return nil, limit("normalized line bytes")
			}
			if len(normalized)+1 > limits.MaxNormalizedBytes-normalizedBytes {
				return nil, limit("normalized bytes")
			}
			normalizedBytes += len(normalized) + 1
			part := occurrences[key]
			if part >= limits.MaxPartitions {
				return nil, limit("duplicate partitions")
			}
			occurrences[key]++
			if part == len(partitions) {
				partitions = append(partitions, partition{})
			}
			partitions[part].lines = append(partitions[part].lines, normalized)
			partitions[part].indices = append(partitions[part].indices, index)
		}
		if err := scanner.Err(); err != nil {
			return nil, fmt.Errorf("%w: source line: %v", ErrLimitExceeded, err)
		}
		if len(c.rows)-before != input.info.Rows {
			return nil, fmt.Errorf("%w: row count for %s", ErrSource, input.info.ID)
		}
		for _, p := range partitions {
			if err := c.parsePartition(p, limits, &normalizedBytes); err != nil {
				return nil, err
			}
		}
	}
	c.summary = Summary{Records: len(c.rows), BySource: map[Source]int{}, ByDisposition: map[Disposition]int{}}
	for i := range c.rows {
		v := &c.rows[i]
		c.summary.BySource[v.record.Source]++
		c.summary.ByDisposition[v.record.Disposition]++
		if v.usable {
			c.byCreator[v.match.Creator] = append(c.byCreator[v.match.Creator], i)
		}
	}
	return c, nil
}

func (c *Catalog) parsePartition(p partition, limits Limits, normalizedBytes *int) error {
	text := strings.Join(p.lines, "\n") + "\n"
	opts := dictionary.DICOptions{Tolerant: true, MaxFileSize: int64(limits.MaxNormalizedBytes),
		MaxLineLength: limits.MaxLineBytes, MaxEntries: limits.MaxRecords, MaxRepeating: limits.MaxPatterns, MaxSkipped: limits.MaxRecords}
	d, report, err := dictionary.ParseDIC(strings.NewReader(text), opts)
	if err != nil {
		return fmt.Errorf("%w: normalized dictionary: %v", ErrSource, err)
	}
	seen := make([]bool, len(p.indices))
	for _, entry := range d.Entries() {
		index := entry.Line - 1
		seen[index] = true
		c.rows[p.indices[index]].match, c.rows[p.indices[index]].usable = entry, true
	}
	for _, issue := range report.Skipped {
		index := issue.Line - 1
		seen[index] = true
		r := &c.rows[p.indices[index]].record
		r.Disposition, r.Reason = InvalidDefinition, issue.Field+": "+issue.Reason
	}
	// Syntactically different equivalent ranges can still be the same DIC key.
	// Recover overridden rows separately so no candidate loses provenance/VR.
	for i, present := range seen {
		if present {
			continue
		}
		if len(p.lines[i])+1 > limits.MaxNormalizedBytes-*normalizedBytes {
			return limit("normalized recovery bytes")
		}
		*normalizedBytes += len(p.lines[i]) + 1
		d, _, err := dictionary.ParseDIC(strings.NewReader(p.lines[i]+"\n"), opts)
		if err != nil || d.Len() != 1 {
			return fmt.Errorf("%w: equivalent definition", ErrSource)
		}
		c.rows[p.indices[i]].match, c.rows[p.indices[i]].usable = d.Entries()[0], true
	}
	return nil
}

var dcmtkTag = regexp.MustCompile(`^\(([^,]+),"([^"]*)",([^)]*)\)$`)

func patternRow(r Record) bool {
	element := r.Element
	// xxNN marks a movable private block, not a wildcard offset. Only the
	// group or an actually repeating/unsupported element shape consumes this
	// budget; otherwise the complete GDCM table would exceed it needlessly.
	if r.Source == GDCM && len(element) == 4 && strings.EqualFold(element[:2], "xx") {
		if _, err := strconv.ParseUint(element[2:], 16, 8); err == nil {
			element = element[2:]
		}
	}
	return strings.ContainsAny(r.Group+element, "-xX")
}

func parseRow(r *Record) {
	r.Disposition = Resolved
	if r.Source == DCMTK {
		var fields []string
		for _, field := range strings.Split(r.Row, "\t") {
			if field = strings.Trim(field, " "); field != "" {
				fields = append(fields, field)
			}
		}
		if len(fields) < 3 || len(fields) > 5 {
			r.Disposition, r.Reason = InvalidDefinition, "invalid tab fields"
			return
		}
		tag := dcmtkTag.FindStringSubmatch(fields[0])
		if tag == nil {
			r.Disposition, r.Reason = InvalidDefinition, "invalid private tag shape"
			return
		}
		r.Group, r.Creator, r.Element, r.VR, r.Name, r.VM = tag[1], tag[2], tag[3], fields[1], fields[2], "1"
		if len(fields) > 3 {
			r.VM = fields[3]
		}
		return
	}
	decoder := xml.NewDecoder(strings.NewReader(strings.TrimSpace(r.Row)))
	token, err := decoder.Token()
	start, ok := token.(xml.StartElement)
	if err != nil || !ok || start.Name.Local != "entry" || start.Name.Space != "" {
		r.Disposition, r.Reason = InvalidDefinition, "invalid XML entry"
		return
	}
	attrs := map[string]string{}
	for _, a := range start.Attr {
		if _, duplicate := attrs[a.Name.Local]; duplicate || a.Name.Space != "" {
			r.Disposition, r.Reason = InvalidDefinition, "duplicate or qualified XML attribute"
			return
		}
		attrs[a.Name.Local] = a.Value
	}
	for _, name := range []string{"owner", "group", "element", "vr", "vm", "name"} {
		if _, ok := attrs[name]; !ok {
			r.Disposition, r.Reason = InvalidDefinition, "missing XML attribute"
			return
		}
	}
	if len(attrs) != 6 {
		r.Disposition, r.Reason = InvalidDefinition, "unknown XML attribute"
		return
	}
	r.Creator, r.Group, r.Element, r.VR, r.VM, r.Name = attrs["owner"], attrs["group"], attrs["element"], attrs["vr"], attrs["vm"], attrs["name"]
	token, err = decoder.Token()
	end, ok := token.(xml.EndElement)
	if err != nil || !ok || end.Name != start.Name {
		r.Disposition, r.Reason = InvalidDefinition, "nonempty XML entry"
		return
	}
	for {
		token, err = decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			r.Disposition, r.Reason = InvalidDefinition, "invalid XML suffix"
			return
		}
		switch t := token.(type) {
		case xml.Comment:
		case xml.CharData:
			if strings.TrimSpace(string(t)) != "" {
				r.Disposition, r.Reason = InvalidDefinition, "XML suffix text"
				return
			}
		default:
			r.Disposition, r.Reason = InvalidDefinition, "multiple XML entries or directive"
			return
		}
	}
}

func normalizeRow(r *Record) (string, string) {
	if r.Disposition != Resolved {
		return "", ""
	}
	creator, err := dictionary.PrivateCreatorID(r.Creator)
	if err != nil {
		r.Disposition, r.Reason = InvalidDefinition, "invalid private creator LO"
		return "", ""
	}
	if strings.ContainsRune(creator, '"') {
		r.Disposition, r.Reason = UnsupportedPattern, "creator not representable in DIC grammar"
		return "", ""
	}
	group, element := strings.ToUpper(r.Group), strings.ToUpper(r.Element)
	if r.Source == GDCM {
		switch group {
		case "60XX":
			group = "6001-O-60FF"
		case "70XX":
			group = "7001-O-70FF"
		default:
			if len(group) != 4 || strings.ContainsAny(group, "-X") {
				r.Disposition, r.Reason = UnsupportedPattern, "unsupported GDCM group pattern"
				return "", ""
			}
		}
		if len(element) == 4 && (strings.HasPrefix(element, "XX") || strings.HasPrefix(element, "00")) {
			element = element[2:]
		} else {
			r.Disposition, r.Reason = UnsupportedPattern, "unsupported GDCM element pattern (including 10xx)"
			return "", ""
		}
	}
	// Canonicalize exact offsets, preserving fixed full elements and all ranges.
	if n, err := strconv.ParseUint(element, 16, 16); err == nil && n <= 255 {
		element = fmt.Sprintf("%02X", n)
	}
	normalizedVR := r.VR
	var spec dictionary.VRSpec
	switch r.VR {
	case "px":
		spec = dictionary.NewContextualVRSpec(dictionary.ContextualVRPX)
	case "ox", "OB_OW":
		spec = dictionary.NewContextualVRSpec(dictionary.ContextualVROX)
	case "xs", "US_SS":
		spec = dictionary.NewContextualVRSpec(dictionary.ContextualVRXS)
	case "lt":
		spec = dictionary.NewContextualVRSpec(dictionary.ContextualVRLT)
	case "up":
		spec = dictionary.NewContextualVRSpec(dictionary.ContextualVRUP)
	default:
		vr, err := core.ParseVR(r.VR)
		if err != nil {
			r.Disposition, r.Reason = InvalidDefinition, "invalid VR token"
			return "", ""
		}
		spec = dictionary.NewVRSpec(vr)
		if vr == core.VRUN {
			r.Disposition, r.Reason = Opaque, "upstream UN"
		}
	}
	r.AllowedVRs = spec.Values()
	if spec.Context() != "" {
		r.Disposition, r.Reason, normalizedVR = Contextual, "private contextual VR has no scoped resolution contract", "UN"
	}
	if knownContradiction(creator, group, element) {
		r.Disposition, r.Reason = KnownCorpusConflict, "existing CURATED_PRIVATE_CATALOG explicit-VR corpus contradiction"
	}
	if strings.ContainsAny(group+element+normalizedVR+r.VM, "\t\r\n") {
		r.Disposition, r.Reason = InvalidDefinition, "invalid token control character"
		return "", ""
	}
	tag := "(" + group + ",\"" + creator + "\"," + element + ")"
	return tag + "\t" + normalizedVR + "\tPrivateData\t" + r.VM + "\tPrivateTag", tag
}
