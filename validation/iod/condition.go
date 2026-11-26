package iod

import (
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
)

// condition is the formal reading of a PS3.3 "Required if ..." sentence:
// clauses joined by "and" (all) or "or" (any).
type condition struct {
	clauses []clause
	any     bool
	// forbidden reports "shall not be present otherwise".
	forbidden bool
}

type operator int

const (
	opPresent operator = iota
	opAbsent
	opNonEmpty
	opEquals
	opNotEquals
	opGreater
	opNonZero
	// opEqualsTag: an AT Value is one of tags.
	opEqualsTag
	// opCodeItem: a Sequence has an Item whose Code Value and Coding Scheme
	// Designator are values[0] and values[1].
	opCodeItem
)

// clause tests one Attribute: its presence, or its Value (the valueIndex-th
// when set, 1-based, otherwise any Value).
type clause struct {
	tag        core.Tag
	valueIndex int
	op         operator
	values     []string
	tags       []core.Tag
	number     float64
}

var (
	clausePattern = regexp.MustCompile(`^(?:the )?(?:[Vv]alue (\d) of )?(?:the [Vv]alue (?:for|of) )?(?:(?:the )?Attribute )?[A-Z0-9][A-Za-z0-9'’ /&\-]*? \(([0-9A-Fa-f]{4}),([0-9A-Fa-f]{4})\)(?: [Vv]alue (\d))?(?: of this [A-Za-z ]+?)?(?: in this (?:Sequence )?Item)? (.+)$`)
	remarkPattern = regexp.MustCompile(`\s*\((?:[^()0-9][^()]*)\)`)
	valuePattern  = regexp.MustCompile(`^(?:"[^"]*"|[A-Z0-9_][A-Z0-9_ .\-]*)$`)
)

// requirementLead are the phrases that open a condition.
var requirementLead = []string{"Required if: ", "Required if ", "Required, if ", "Required only if ", "Required when ", "Shall be present if "}

var bareTagPattern = regexp.MustCompile(`^(?:either )?(?:the )?[A-Z0-9][A-Za-z0-9'’ /&\-]*? \([0-9A-Fa-f]{4},[0-9A-Fa-f]{4}\)$`)

type parsedCondition struct {
	c  condition
	ok bool
}

// conditionCache holds the parse of each condition text; the texts come
// from the generated rules, so it stays bounded.
var conditionCache sync.Map

// parseCondition reads a condition's first sentence. It returns false for
// conditions it cannot formalize, which are left to manual review.
func parseCondition(text string) (condition, bool) {
	if cached, ok := conditionCache.Load(text); ok {
		parsed := cached.(parsedCondition)
		return parsed.c, parsed.ok
	}
	c, ok := parseConditionText(text)
	conditionCache.Store(text, parsedCondition{c: c, ok: ok})
	return c, ok
}

// measurableConditions rewrites conditions worded as facts about the
// instance into the Attributes that record those facts (lower case, without
// the requirement lead and final period).
var measurableConditions = map[string]string{
	"the sop instance was created in response to a frame-level retrieve request": "Frame Extraction Sequence (0008,1164) is present",
	"pixel data is multi-frame cine data":                                        "Frame Increment Pointer (0028,0009) is Frame Time (0018,1063) or Frame Time Vector (0018,1065)",
	"contrast media was used in this image":                                      "Contrast/Bolus Agent (0018,0010) or Contrast/Bolus Agent Sequence (0018,0012) is present",
	"contrast media was used":                                                    "Contrast/Bolus Agent (0018,0010) or Contrast/Bolus Agent Sequence (0018,0012) is present",
	"contrast media was applied":                                                 "Contrast/Bolus Agent (0018,0010) or Contrast/Bolus Agent Sequence (0018,0012) is present",
	"contrast media were applied":                                                "Contrast/Bolus Agent (0018,0010) or Contrast/Bolus Agent Sequence (0018,0012) is present",
	"lossy compression has been performed on the image":                          "Lossy Image Compression (0028,2110) is 01",
}

// codeItemPattern reads "Sequence (gggg,eeee) contains an Item with the
// value ." whose coded value the DocBook projection moved to the end of the
// text: "... (392012008, SCT, "Optical Coherence Tomography Scanner")".
var (
	codeItemPattern    = regexp.MustCompile(`^(?:the )?[A-Z0-9][A-Za-z0-9'’ /&\-]*? \(([0-9A-Fa-f]{4}),([0-9A-Fa-f]{4})\) (?:contains an Item with (?:the )?(?:[Vv]alue|Coded Entry Value|Code Value)|includes an Item with (?:the )?[Vv]alue|is|has an Item with (?:the )?[Vv]alue)$`)
	codeTripletPattern = regexp.MustCompile(`\(([^,()"]+), ([^,()"]+), "[^"]*"\)\s*$`)
	tagValuePattern    = regexp.MustCompile(`^[A-Z0-9][A-Za-z0-9'’ /&\-]*? \(([0-9A-Fa-f]{4}),([0-9A-Fa-f]{4})\)$`)
	tagInTextPattern   = regexp.MustCompile(`\(([0-9A-Fa-f]{4}),([0-9A-Fa-f]{4})\)`)
)

func parseConditionText(text string) (condition, bool) {
	lower := strings.ToLower(text)
	c := condition{forbidden: strings.Contains(lower, "shall not be present") && !strings.Contains(lower, "may be present otherwise")}
	sentence := ""
	for _, lead := range requirementLead {
		if strings.HasPrefix(text, lead) {
			sentence = firstSentence(text[len(lead):])
			break
		}
	}
	if sentence == "" {
		return c, false
	}
	if rewrite, ok := measurableConditions[strings.ToLower(strings.TrimSuffix(sentence, "."))]; ok {
		sentence = rewrite
	}
	if m := codeItemPattern.FindStringSubmatch(sentence); m != nil {
		code := codeTripletPattern.FindStringSubmatch(text)
		if code == nil || len(codeTripletPattern.FindAllString(text, -1)) != 1 {
			return c, false
		}
		group, _ := strconv.ParseUint(m[1], 16, 16)
		element, _ := strconv.ParseUint(m[2], 16, 16)
		c.clauses = []clause{{tag: core.NewTag(uint16(group), uint16(element)), op: opCodeItem, values: []string{strings.TrimSpace(code[1]), strings.TrimSpace(code[2])}}}
		return c, true
	}
	sentence = normalizeSentence(sentence)
	if cl, ok := parseClause(sentence); ok {
		c.clauses = []clause{cl}
		return c, true
	}
	for _, sep := range []string{" and ", " or "} {
		if clauses, ok := parseParts(splitParts(sentence, sep), sep == " or "); ok {
			c.clauses, c.any = clauses, sep == " or "
			return c, true
		}
	}
	return c, false
}

// normalizeSentence removes remarks in parentheses and trailing "may be
// present otherwise" phrases, joins ", Value n" to its Attribute, and
// protects the "and"/"or" inside Attribute names ("RT Radiation Physical and
// Geometric Content Detail Flag") from the clause split.
func normalizeSentence(sentence string) string {
	sentence = strings.ReplaceAll(remarkPattern.ReplaceAllString(sentence, ""), ", and ", " and ")
	sentence = strings.ReplaceAll(sentence, "), Value ", ") Value ")
	sentence = strings.ReplaceAll(sentence, ") at the image level ", ") ")
	for _, tail := range []string{", may be present otherwise", "; may be present otherwise", ", otherwise may be present"} {
		if strings.HasSuffix(strings.ToLower(sentence), tail) {
			sentence = sentence[:len(sentence)-len(tail)]
		}
	}
	return protectAttributeNames(sentence)
}

// keywordOf squashes an Attribute name to its keyword form.
func keywordOf(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func protectAttributeNames(sentence string) string {
	matches := tagInTextPattern.FindAllStringSubmatchIndex(sentence, -1)
	for i := len(matches) - 1; i >= 0; i-- {
		m := matches[i]
		group, _ := strconv.ParseUint(sentence[m[2]:m[3]], 16, 16)
		element, _ := strconv.ParseUint(sentence[m[4]:m[5]], 16, 16)
		entry, ok := std.Dictionary.ByTag(core.NewTag(uint16(group), uint16(element)))
		if !ok {
			continue
		}
		before := strings.TrimRight(sentence[:m[0]], " ")
		words := strings.Fields(before)
		for n := min(len(words), 16); n >= 2; n-- {
			name := strings.Join(words[len(words)-n:], " ")
			if !strings.Contains(name, " and ") && !strings.Contains(name, " or ") {
				continue
			}
			if strings.EqualFold(keywordOf(name), entry.Keyword) {
				protected := strings.ReplaceAll(strings.ReplaceAll(name, " and ", " & "), " or ", " / ")
				sentence = before[:len(before)-len(name)] + protected + " " + sentence[m[0]:]
				break
			}
		}
	}
	return sentence
}

// withAttributeTag inserts the tag after an Attribute named without one
// ("Number of Frames is greater than 1"): the longest leading run of two or
// more words that is the name of a standard Attribute.
func withAttributeTag(text string) (string, bool) {
	if tagInTextPattern.MatchString(text) {
		return "", false
	}
	words := strings.Fields(strings.TrimPrefix(text, "the "))
	for n := min(len(words)-1, 10); n >= 2; n-- {
		name := strings.Join(words[:n], " ")
		if strings.ToUpper(name[:1]) != name[:1] {
			return "", false
		}
		entry, ok := std.Dictionary.ByKeyword(keywordOf(name))
		if !ok || entry.Retired {
			continue
		}
		return name + " " + entry.Tag.String() + " " + strings.Join(words[n:], " "), true
	}
	return "", false
}

// splitParts splits a sentence into clauses at sep, and a clause that is a
// comma-separated list of Attributes into one part per Attribute.
func splitParts(sentence, sep string) []string {
	var parts []string
	for _, part := range strings.Split(sentence, sep) {
		pieces := strings.Split(part, ", ")
		list := len(pieces) > 1
		for _, piece := range pieces[:len(pieces)-1] {
			if !bareTagPattern.MatchString(strings.TrimSpace(piece)) {
				list = false
			}
		}
		if list {
			parts = append(parts, pieces...)
		} else {
			parts = append(parts, part)
		}
	}
	return parts
}

// parseParts reads the clauses of a conjunction or disjunction: an
// Attribute without a predicate shares the last part's predicate ("A (t)
// and B (t) are not present"), and "the Value is X" refers to the
// Attribute of the part before.
func parseParts(parts []string, disjunction bool) ([]clause, bool) {
	if len(parts) < 2 {
		return nil, false
	}
	shared := ""
	if m := clausePattern.FindStringSubmatch(strings.TrimSpace(parts[len(parts)-1])); m != nil {
		shared = m[5]
		for plural, singular := range map[string]string{"are not present": "is not present", "are present": "is present", "are absent": "is absent", "are not": "is not", "are ": "is "} {
			if strings.HasPrefix(shared, plural) {
				shared = singular + strings.TrimPrefix(shared, plural)
				break
			}
		}
	}
	var clauses []clause
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if cl, ok := parseClause(part); ok {
			clauses = append(clauses, cl)
			continue
		}
		if shared != "" && bareTagPattern.MatchString(part) {
			if cl, ok := parseClause(part + " " + shared); ok {
				clauses = append(clauses, cl)
				continue
			}
		}
		if disjunction && len(clauses) > 0 && clauses[len(clauses)-1].op == opEquals && valuePattern.MatchString(part) {
			// "A (t) equals COLOR or MIXED" split at " or ".
			last := &clauses[len(clauses)-1]
			last.values = append(append([]string(nil), last.values...), strings.Trim(part, `"`))
			continue
		}
		if len(clauses) > 0 {
			for _, prefix := range []string{
				"the Value is ", "the value is ", "its Value is ", "has a Value of ", "has a value of ", "has the Value ", "has the value ",
				"has Value ", "has value ", "equals ", "is present and has Value ", "is present and has a Value of ", "is present and equals ",
			} {
				if rest, ok := strings.CutPrefix(part, prefix); ok {
					if cl, ok := (clause{tag: clauses[len(clauses)-1].tag, op: opEquals}).withValues(rest); ok {
						clauses = append(clauses, cl)
						goto next
					}
				}
			}
		}
		return nil, false
	next:
	}
	return clauses, true
}

// firstSentence cuts text at the first period that ends a sentence (not
// one inside quotes or a number).
func firstSentence(text string) string {
	quoted := false
	for i, r := range text {
		switch {
		case r == '"':
			quoted = !quoted
		case r == '.' && !quoted && (i+1 == len(text) || text[i+1] == ' '):
			return strings.TrimSpace(text[:i])
		}
	}
	return strings.TrimSpace(text)
}

// attributeName rejects a clause whose "name" is prose that runs into the
// Attribute ("the Patient is an animal and if Patient Species Code
// Sequence (0010,2202)").
func attributeName(name string) bool {
	name = " " + strings.TrimSpace(name) + " "
	for _, word := range []string{" is ", " are ", " and ", " if ", " or ", " has ", " have ", " not ", " was ", " be ", " an ", " a ", " when ", " that ", " which "} {
		if strings.Contains(name, word) {
			return false
		}
	}
	return true
}

func parseClause(text string) (clause, bool) {
	text = strings.TrimSpace(text)
	for _, lead := range []string{"if ", "either "} {
		text = strings.TrimPrefix(text, lead)
	}
	m := clausePattern.FindStringSubmatch(text)
	if m == nil {
		if tagged, ok := withAttributeTag(text); ok {
			text = tagged
			m = clausePattern.FindStringSubmatch(text)
		}
	}
	if m == nil || !attributeName(text[:strings.Index(text, "(")]) {
		return clause{}, false
	}
	group, _ := strconv.ParseUint(m[2], 16, 16)
	element, _ := strconv.ParseUint(m[3], 16, 16)
	cl := clause{tag: core.NewTag(uint16(group), uint16(element))}
	for _, index := range []string{m[1], m[4]} {
		if index != "" {
			cl.valueIndex, _ = strconv.Atoi(index)
		}
	}
	predicate := strings.TrimSpace(m[5])
	switch predicate {
	case "is present", "is sent", "is included", "are present":
		cl.op = opPresent
		return cl, true
	case "is not present", "is absent", "is not sent", "is not included", "not present", "are not present", "are absent":
		cl.op = opAbsent
		return cl, true
	case "is present and has a value", "is present with a value", "has a value", "is present and not empty", "is not empty",
		"has a Value", "is present and has a Value", "is present with a Value":
		cl.op = opNonEmpty
		return cl, true
	case "has a non-zero value", "is present and has a non-zero value", "is non-zero":
		cl.op = opNonZero
		return cl, true
	}
	for _, prefix := range []string{"is ", "equals ", "has a Value of ", "has the Value "} {
		rest, ok := strings.CutPrefix(predicate, prefix)
		if !ok {
			continue
		}
		var tags []core.Tag
		for _, part := range strings.Split(rest, " or ") {
			part = strings.TrimSpace(part)
			m := tagValuePattern.FindStringSubmatch(part)
			if m == nil {
				tags = nil
				break
			}
			group, _ := strconv.ParseUint(m[1], 16, 16)
			element, _ := strconv.ParseUint(m[2], 16, 16)
			tag := core.NewTag(uint16(group), uint16(element))
			// The Value must be exactly the named Attribute, not prose
			// that runs into a tag.
			entry, ok := std.Dictionary.ByTag(tag)
			if !ok || !strings.EqualFold(keywordOf(part[:strings.LastIndex(part, "(")]), entry.Keyword) {
				tags = nil
				break
			}
			tags = append(tags, tag)
		}
		if len(tags) > 0 {
			cl.op, cl.tags = opEqualsTag, tags
			return cl, true
		}
	}
	for _, prefix := range []string{"is greater than ", "has a value greater than ", "has a Value greater than "} {
		if rest, ok := strings.CutPrefix(predicate, prefix); ok {
			n, err := strconv.ParseFloat(rest, 64)
			if err != nil {
				return clause{}, false
			}
			cl.op, cl.number = opGreater, n
			return cl, true
		}
	}
	for _, prefix := range []string{
		"is not ", "does not equal ", "is other than ", "does not have a value of ", "does not have a Value of ",
		"Value is not ", "value is not ", "equals other than ",
	} {
		if rest, ok := strings.CutPrefix(predicate, prefix); ok {
			cl.op = opNotEquals
			return cl.withValues(rest)
		}
	}
	for _, prefix := range []string{
		"is present and has a Value of ", "is present and has a value of ", "is present and has the Value ", "is present with a Value of ",
		"is present with a value of ", "is present and equals ", "is present with Value ", "is present and has Value ", "has Value ", "has a Value of ", "has a value of ", "has the Value ", "has the value ",
		"is equal to ", "equals ", "= ", "Value is ", "value is ", "Value equals ", "includes Value ", "includes the Value ", "includes ",
		"contains the Value ", "is ",
	} {
		if rest, ok := strings.CutPrefix(predicate, prefix); ok {
			cl.op = opEquals
			return cl.withValues(rest)
		}
	}
	return clause{}, false
}

// withValues parses "A", "A or B" or "A, B or C" of code-like values.
func (cl clause) withValues(text string) (clause, bool) {
	text = strings.TrimSpace(strings.TrimPrefix(text, "the Value "))
	var values []string
	for _, part := range strings.Split(strings.ReplaceAll(text, ", ", " or "), " or ") {
		part = strings.TrimSpace(part)
		if !valuePattern.MatchString(part) {
			return clause{}, false
		}
		values = append(values, strings.Trim(part, `"`))
	}
	if len(values) == 0 {
		return clause{}, false
	}
	cl.values = values
	return cl, true
}

// eval evaluates the condition on a Sequence Item (or the data set itself)
// and the data set root: each Attribute is looked up in the item first.
func (c condition) eval(item, root *object.Object) bool {
	for _, cl := range c.clauses {
		result := cl.eval(item, root)
		if c.any && result {
			return true
		}
		if !c.any && !result {
			return false
		}
	}
	return !c.any
}

func (cl clause) eval(item, root *object.Object) bool {
	holder := item
	if !item.Has(cl.tag) && root != item && root.Has(cl.tag) {
		holder = root
	}
	element, present := holder.Get(cl.tag)
	switch cl.op {
	case opPresent:
		return present
	case opAbsent:
		return !present
	case opNonEmpty:
		return present && !emptyElement(holder, element)
	}
	if !present {
		return cl.op == opNotEquals
	}
	switch cl.op {
	case opEqualsTag:
		tags, _ := holder.GetTags(cl.tag)
		for _, tag := range tags {
			for _, want := range cl.tags {
				if tag == want {
					return true
				}
			}
		}
		return false
	case opCodeItem:
		items, _ := holder.GetSequence(cl.tag)
		for _, item := range items {
			value, _ := item.GetString(core.NewTag(0x0008, 0x0100))
			scheme, _ := item.GetString(core.NewTag(0x0008, 0x0102))
			if strings.TrimSpace(value) == cl.values[0] && strings.TrimSpace(scheme) == cl.values[1] {
				return true
			}
		}
		return false
	}
	values := elementValues(holder, cl.tag)
	if cl.valueIndex > 0 {
		if cl.valueIndex > len(values) {
			return cl.op == opNotEquals
		}
		values = values[cl.valueIndex-1 : cl.valueIndex]
	}
	switch cl.op {
	case opNonZero, opGreater:
		for _, v := range values {
			n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			if err == nil && (cl.op == opNonZero && n != 0 || cl.op == opGreater && n > cl.number) {
				return true
			}
		}
		return false
	}
	matched := false
	for _, v := range values {
		for _, want := range cl.values {
			if strings.EqualFold(strings.TrimSpace(v), want) {
				matched = true
			}
		}
	}
	if cl.op == opNotEquals {
		return !matched
	}
	return matched
}

// elementValues returns an Attribute's values as strings, numbers included.
func elementValues(obj *object.Object, tag core.Tag) []string {
	if values, ok := obj.GetStrings(tag); ok && len(values) > 0 {
		return values
	}
	if numbers, err := obj.GetFloats(tag); err == nil {
		out := make([]string, len(numbers))
		for i, n := range numbers {
			out[i] = strconv.FormatFloat(n, 'g', -1, 64)
		}
		return out
	}
	return nil
}
