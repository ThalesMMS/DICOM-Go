package main

import (
	"errors"
	"flag"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/attrpath"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary"
)

type rawAction struct{ kind, arg string }
type action struct {
	kind          string
	edit          attrpath.Edit
	tag           attrpath.Segment
	value, source string
	uidTag        core.Tag
}

type actionFlag struct {
	kind    string
	opts    *options
	boolean bool
}

func (f actionFlag) String() string   { return "" }
func (f actionFlag) IsBoolFlag() bool { return f.boolean }
func (f actionFlag) Set(value string) error {
	if f.boolean {
		b, err := strconv.ParseBool(value)
		if err != nil {
			return errUsage
		}
		if !b {
			return nil
		}
		value = ""
	}
	if len(f.opts.jobs) >= attrpath.MaxScriptEdits || len(value) > attrpath.MaxScriptBytes-f.opts.textBytes {
		return attrpath.ErrLimit
	}
	f.opts.textBytes += len(value)
	f.opts.jobs = append(f.opts.jobs, rawAction{f.kind, value})
	if f.kind == "insert" || f.kind == "modify" || f.kind == "remove" || f.kind == "remove-if-present" {
		f.opts.edits = append(f.opts.edits, f.kind+" "+value)
	}
	return nil
}

type conditionFlag struct{ opts *options }

func (f conditionFlag) String() string { return "" }
func (f conditionFlag) Set(value string) error {
	if len(f.opts.conditionText) >= 128 || len(value) > attrpath.MaxConditionLength || len(value) > attrpath.MaxScriptBytes-f.opts.textBytes {
		return attrpath.ErrLimit
	}
	f.opts.textBytes += len(value)
	f.opts.conditionText = append(f.opts.conditionText, value)
	return nil
}

func registerParity(fs *flag.FlagSet, opts *options) {
	for _, pair := range [][3]string{{"gst", "gen-stud-uid", "study-uid"}, {"gse", "gen-ser-uid", "series-uid"}, {"gin", "gen-inst-uid", "instance-uid"}, {"ep", "erase-private", "private"}} {
		f := actionFlag{kind: pair[2], opts: opts, boolean: true}
		fs.Var(f, pair[0], "ordered action: "+pair[1])
		fs.Var(f, pair[1], "ordered action: "+pair[1])
	}
	for _, pair := range [][3]string{{"ma", "modify-all", "modify-all"}, {"ea", "erase-all", "remove-all"}, {"if", "insert-from-file", "insert-file"}, {"mf", "modify-from-file", "modify-file"}} {
		f := actionFlag{kind: pair[2], opts: opts}
		help := map[string]string{"ma": "modify every existing occurrence of tag=value", "ea": "remove every occurrence of one tag", "if": "insert path=encoded-value-file; fixed-width binary source LE", "mf": "modify existing path=encoded-value-file; fixed-width binary source LE"}[pair[0]]
		fs.Var(f, pair[0], help)
		fs.Var(f, pair[1], help)
	}
	fs.Var(conditionFlag{opts}, "iff", "process-if condition (repeatable AND; finite VR comparisons)")
	fs.Var(conditionFlag{opts}, "process-if", "condition (repeatable AND; finite VR comparisons)")
	fs.BoolVar(&opts.ignoreMissing, "imt", false, "ignore only missing tags during modify or erase")
	fs.BoolVar(&opts.ignoreMissing, "ignore-missing-tags", false, "ignore only missing tags during modify or erase")
	fs.BoolVar(&opts.createFile, "create-file", false, "create absent Part 10 Explicit LE input; SOP identities required")
	fs.IntVar(&opts.maxBatch, "max-batch-files", 256, "maximum input files (positive)")
	fs.Int64Var(&opts.maxValueFile, "max-value-file-bytes", 64<<20, "maximum encoded value file bytes (positive; even; binary source LE)")
	fs.Int64Var(&opts.maxValueSources, "max-value-source-bytes", 128<<20, "maximum cumulative value-source snapshot bytes (positive)")
	d := attrpath.DefaultEditLimits()
	opts.editLimits = d
	for _, x := range []struct {
		name  string
		p     *int
		value int
	}{{"depth", &opts.editLimits.MaxDepth, d.MaxDepth}, {"elements", &opts.editLimits.MaxElements, d.MaxElements}, {"items", &opts.editLimits.MaxItems, d.MaxItems}, {"visits", &opts.editLimits.MaxVisits, d.MaxVisits}, {"matches", &opts.editLimits.MaxMatches, d.MaxMatches}, {"path-bytes", &opts.editLimits.MaxPathBytes, d.MaxPathBytes}, {"touches", &opts.editLimits.MaxTouches, d.MaxTouches}, {"created-items", &opts.editLimits.MaxCreatedItems, d.MaxCreatedItems}} {
		fs.IntVar(x.p, "max-edit-"+x.name, x.value, "positive per-file edit resource limit")
	}
	fs.Int64Var(&opts.editLimits.MaxValueBytes, "max-edit-value-bytes", d.MaxValueBytes, "positive cumulative applied-value and comparison/conversion work bytes per input")
}

// Only the exact flag token is normalized, and only before operands. An action
// argument, including a filename that starts with +, is never rewritten.
func normalizeCreateFlag(args []string) []string {
	out := append([]string(nil), args...)
	for i := 0; i < len(out); i++ {
		a := out[i]
		if a == "--" || a == "-" || !strings.HasPrefix(a, "-") && a != "+fc" {
			break
		}
		if a == "+fc" {
			out[i] = "--create-file"
			continue
		}
		if strings.Contains(a, "=") {
			continue
		}
		name := strings.TrimLeft(a, "-")
		if optionNeedsValue(name) {
			i++
		}
	}
	return out
}

func optionNeedsValue(name string) bool {
	switch name {
	case "i", "m", "e", "e-if-present", "script", "o", "original-attributes", "modifying-system", "ma", "ea", "if", "mf", "modify-all", "erase-all", "insert-from-file", "modify-from-file", "iff", "process-if", "read-odd-length":
		return true
	}
	return strings.HasPrefix(name, "max-") || strings.HasPrefix(name, "read-max-")
}

func compileActions(opts options, dict dictionary.DataDictionary) ([]action, []attrpath.Condition, error) {
	var jobs []action
	for _, raw := range opts.jobs {
		a := action{kind: raw.kind}
		switch raw.kind {
		case "study-uid":
			a.uidTag = core.NewTag(0x0020, 0x000d)
		case "series-uid":
			a.uidTag = core.NewTag(0x0020, 0x000e)
		case "instance-uid":
			a.uidTag = core.NewTag(0x0008, 0x0018)
		case "private":
		case "modify-all", "remove-all":
			path, value, has := cutActionValue(raw.arg)
			if raw.kind == "modify-all" && !has || raw.kind == "remove-all" && has {
				return nil, nil, errUsage
			}
			p, err := attrpath.Parse(strings.TrimSpace(path), dict)
			if err != nil {
				return nil, nil, errUsage
			}
			if len(p) != 1 || p[0].HasItem {
				return nil, nil, errUsage
			}
			a.tag = p[0]
			a.value = value
		case "insert-file", "modify-file":
			path, value, has := cutActionValue(raw.arg)
			if !has || value == "" {
				return nil, nil, errUsage
			}
			p, err := attrpath.Parse(strings.TrimSpace(path), dict)
			if err != nil || p.SelectsItem() {
				return nil, nil, errUsage
			}
			a.edit.Path = p
			a.source = value
		default:
			if strings.ContainsAny(raw.arg, "\r\n") {
				return nil, nil, errUsage
			}
			script, err := attrpath.ParseScript(raw.kind+" "+raw.arg, dict)
			if err != nil || len(script) != 1 {
				return nil, nil, errUsage
			}
			a.edit = script[0]
			a.kind = "edit"
		}
		jobs = append(jobs, a)
	}
	if opts.scriptPath != "" {
		data, err := readScript(opts.scriptPath, attrpath.MaxScriptBytes-opts.textBytes)
		if err != nil {
			return nil, nil, err
		}
		script, err := attrpath.ParseScript(string(data), dict)
		if err != nil {
			return nil, nil, errUsage
		}
		if len(script) > attrpath.MaxScriptEdits-len(jobs) {
			return nil, nil, attrpath.ErrLimit
		}
		for _, edit := range script {
			jobs = append(jobs, action{kind: "edit", edit: edit})
		}
	}
	var conditions []attrpath.Condition
	for _, text := range opts.conditionText {
		c, err := attrpath.ParseCondition(text, dict)
		if err != nil {
			return nil, nil, errUsage
		}
		conditions = append(conditions, c)
	}
	return jobs, conditions, nil
}

func cutActionValue(s string) (string, string, bool) {
	quoted := false
	for i, c := range s {
		if c == '"' {
			quoted = !quoted
		}
		if c == '=' && !quoted {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}

func safeCLIError(err error) string {
	for _, x := range []struct {
		err     error
		message string
	}{{attrpath.ErrLimit, "resource limit exceeded"}, {attrpath.ErrNotFound, "attribute not found"}, {attrpath.ErrUnknownVR, "unknown or ambiguous value representation"}, {errUsage, "invalid options or edit syntax"}} {
		if errors.Is(err, x.err) {
			return x.message
		}
	}
	return "operation rejected"
}
