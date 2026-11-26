package dicomconv

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dicomjson"
	"github.com/ThalesMMS/dicom-go/dicomxml"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/object"
)

// normalizedJSON is the tolerance of the JSON comparisons: element by
// element after numbers (and DS/IS strings) become float64 and group
// lengths and the File Meta Information are dropped.
const normalizedJSON = "equal element by element after number normalization (group lengths and File Meta dropped)"

// dcmtk runs tool (a path from dcmtkoracle.Tool), failing on a non-zero exit
// or an error line.
func dcmtk(t *testing.T, tolerance, tool string, args ...string) {
	t.Helper()
	if out := dcmtkoracle.Run(t, tolerance, tool, args...); bytes.Contains(out, []byte("E: ")) {
		t.Fatalf("%s %v:\n%s", tool, args, out)
	}
}

// normalizeJSON reduces a DICOM JSON document to comparable values: numbers
// (and numeric strings of DS/IS) become float64, and group lengths and the
// File Meta Information are dropped.
func normalizeJSON(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	return normalizeDataSet(document)
}

func normalizeDataSet(document map[string]any) map[string]any {
	out := map[string]any{}
	for tag, raw := range document {
		if strings.HasPrefix(tag, "0002") || strings.HasSuffix(tag, "0000") {
			continue
		}
		element := raw.(map[string]any)
		normalized := map[string]any{"vr": element["vr"]}
		if values, ok := element["Value"].([]any); ok {
			var list []any
			for _, value := range values {
				switch v := value.(type) {
				case map[string]any:
					if element["vr"] == "SQ" {
						list = append(list, normalizeDataSet(v))
					} else {
						list = append(list, v)
					}
				case string:
					if element["vr"] == "DS" || element["vr"] == "IS" {
						if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
							list = append(list, f)
							continue
						}
					}
					list = append(list, v)
				default:
					list = append(list, v)
				}
			}
			normalized["Value"] = list
		}
		if inline, ok := element["InlineBinary"]; ok {
			normalized["InlineBinary"] = inline
		}
		out[tag] = normalized
	}
	return out
}

func diffJSON(a, b map[string]any) []string {
	var diffs []string
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	var sorted []string
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	for _, k := range sorted {
		if !reflect.DeepEqual(a[k], b[k]) {
			diffs = append(diffs, fmt.Sprintf("%s: ours %v, dcmtk %v", k, a[k], b[k]))
		}
	}
	return diffs
}

// TestConversionsMatchDCMTK cross-checks the converters with DCMTK 8d68392d:
// dcm2json and this dcm2json agree element by element; DCMTK json2dcm reads
// this JSON back to the same data set; this json2dcm reads DCMTK's JSON, and
// this xml2dcm reads dcm2xml --native-format, both to the original data set.
// Enable with DICOMGO_DCMTK_INTEGRATION=1.
func TestConversionsMatchDCMTK(t *testing.T) {
	dcm2json := dcmtkoracle.Tool(t, "dcm2json")
	json2dcm := dcmtkoracle.Tool(t, "json2dcm")
	dcm2xml := dcmtkoracle.Tool(t, "dcm2xml")
	ctx := context.Background()
	for name, file := range map[string]*object.File{"rich": richFile(t), "jpeg": jpegFile(t)} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, "in.dcm")
			if err := WritePart10(source, file); err != nil {
				t.Fatal(err)
			}
			want := datasetBytes(t, file)

			ours, err := Encode(ctx, file, JSON, EncodeOptions{})
			if err != nil {
				t.Fatal(err)
			}
			// dcm2json cannot inline encapsulated Pixel Data.
			var theirs []byte
			if !file.TransferSyntax.Encapsulated {
				theirsPath := filepath.Join(dir, "dcmtk.json")
				dcmtk(t, normalizedJSON, dcm2json, source, theirsPath)
				if theirs, err = os.ReadFile(theirsPath); err != nil {
					t.Fatal(err)
				}
				a, b := normalizeJSON(t, ours), normalizeJSON(t, theirs)
				// Both documents are UTF-8. This writer declares ISO_IR 192;
				// dcm2json declared it too up to 3.7.0, and since DCMTK
				// 4f8b940a (in the pinned 8d68392d) keeps the source
				// declaration unless --charset-replace is given.
				if got := charsetValue(b); got != "ISO_IR 192" && got != sourceCharset(file) {
					t.Errorf("dcm2json Specific Character Set %q, want ISO_IR 192 or the source %q", got, sourceCharset(file))
				}
				if got := charsetValue(a); sourceCharset(file) != "" && got != "ISO_IR 192" {
					t.Errorf("Specific Character Set %q, want ISO_IR 192", got)
				}
				delete(a, "00080005")
				delete(b, "00080005")
				if diffs := diffJSON(a, b); len(diffs) != 0 {
					t.Errorf("dcm2json differs:\n%s", strings.Join(diffs, "\n"))
				}
			}

			// DCMTK json2dcm reads this JSON; it cannot build encapsulated
			// Pixel Data from InlineBinary.
			oursPath := filepath.Join(dir, "ours.json")
			if err := os.WriteFile(oursPath, ours, 0o600); err != nil {
				t.Fatal(err)
			}
			viaDCMTK := filepath.Join(dir, "via-dcmtk.dcm")
			if !file.TransferSyntax.Encapsulated {
				dcmtk(t, normalizedJSON, json2dcm, oursPath, viaDCMTK)
				read, err := ReadPart10(viaDCMTK)
				if err != nil {
					t.Fatal(err)
				}
				again, err := Encode(ctx, read, JSON, EncodeOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if diffs := diffJSON(normalizeJSON(t, again), normalizeJSON(t, ours)); len(diffs) != 0 {
					t.Errorf("DCMTK json2dcm of this JSON differs:\n%s", strings.Join(diffs, "\n"))
				}
				_ = read.Close()
			}

			// This json2dcm reads DCMTK's JSON; this xml2dcm reads DCMTK's
			// Native DICOM Model XML.
			xmlPath := filepath.Join(dir, "dcmtk.xml")
			dcmtk(t, normalizedJSON+"; BulkData values not compared", dcm2xml, "-nat", "+Xn", source, xmlPath)
			xmlData, err := os.ReadFile(xmlPath)
			if err != nil {
				t.Fatal(err)
			}
			if theirs != nil {
				back, err := Decode(ctx, theirs, JSON, DecodeOptions{BaseDir: dir, CharacterSet: sourceCharset(file)})
				if err != nil {
					t.Fatalf("decode dcm2json output: %v", err)
				}
				if got := datasetBytes(t, reread(t, back)); !bytes.Equal(got, want) {
					t.Error("dcm2json output decodes to a different data set")
				}
			}
			// dcm2xml --native-format replaces binary values by UUID-only
			// BulkData references without content: compare everything else.
			native, err := dicomxml.UnmarshalWithOptions(xmlData, std.Dictionary, dicomxml.UnmarshalOptions{
				BulkDataPolicy: dicomxml.PreserveBulkData, TransferSyntax: file.TransferSyntax, Limits: dicomxml.DefaultLimits(),
			})
			if err != nil {
				t.Fatalf("decode dcm2xml output: %v", err)
			}
			fromXML, err := dicomjson.Marshal(native, dicomjson.DefaultOptions())
			if err != nil {
				t.Fatal(err)
			}
			a, b := normalizeJSON(t, fromXML), normalizeJSON(t, ours)
			for tag, element := range normalizeJSONRaw(t, fromXML) {
				if _, bulk := element["BulkDataURI"]; bulk {
					delete(a, tag)
					delete(b, tag)
				}
			}
			// dcm2xml keeps the source declaration in an ISO-8859-1 document;
			// this writer declares ISO_IR 192 in a UTF-8 one.
			delete(a, "00080005")
			delete(b, "00080005")
			if diffs := diffJSON(b, a); len(diffs) != 0 {
				t.Errorf("dcm2xml --native-format differs:\n%s", strings.Join(diffs, "\n"))
			}
		})
	}
}

// charsetValue returns the Specific Character Set of a normalized JSON data
// set, its values joined with a backslash.
func charsetValue(document map[string]any) string {
	element, _ := document["00080005"].(map[string]any)
	values, _ := element["Value"].([]any)
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, fmt.Sprint(value))
	}
	return strings.Join(parts, `\`)
}

func sourceCharset(file *object.File) string {
	value, _ := file.Dataset.GetString(core.NewTag(0x0008, 0x0005))
	return value
}

func normalizeJSONRaw(t *testing.T, data []byte) map[string]map[string]any {
	t.Helper()
	var document map[string]map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	return document
}
