package dicomjson

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
)

func TestStreamDecoderEveryByteBoundary(t *testing.T) {
	// Exercise token boundaries inside UTF-8, an escaped quotation mark, a
	// backslash, a Unicode escape, braces in a text value, and nested items.
	const document = ` [
  {"0040A160":{"vr":"UT","Value":["ação \\ [} \"\u00e9\""]},"00280011":{"vr":"US","Value":[389]}},
  {"00081115":{"vr":"SQ","Value":[{}, {"0020000E":{"vr":"UI","Value":["2.25.71"]}}]}},
  {}
] `
	const text = `ação \ [} "é"`
	textTag := core.NewTag(0x0040, 0xA160)
	columns := core.NewTag(0x0028, 0x0011)
	referencedSeries := core.NewTag(0x0008, 0x1115)
	seriesUID := core.NewTag(0x0020, 0x000E)
	for seam := 1; seam < len(document); seam++ {
		t.Run(fmt.Sprint(seam), func(t *testing.T) {
			input := io.MultiReader(strings.NewReader(document[:seam]), strings.NewReader(document[seam:]))
			var objects []*object.Object
			decoder := NewDecoder(input, std.Dictionary, UnmarshalOptions{Limits: Limits{MaxJSONBytes: int64(len(document))}})
			if err := decoder.DecodeArrayContext(context.Background(), func(dataset *object.Object) error {
				objects = append(objects, dataset)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if len(objects) != 3 || objects[2].Len() != 0 {
				t.Fatalf("unexpected callback count or last dataset: %d objects", len(objects))
			}
			if got, err := objects[0].LookupString(textTag); err != nil || got != text {
				t.Fatalf("literal text = %q, %v, want %q", got, err, text)
			}
			if got, err := objects[0].GetInt(columns); err != nil || got != 389 {
				t.Fatalf("Columns = %d, %v", got, err)
			}
			items, ok := objects[1].GetSequence(referencedSeries)
			if !ok || len(items) != 2 || items[0].Len() != 0 {
				t.Fatalf("nested sequence lost its empty item: %v %v", items, ok)
			}
			if got, err := items[1].LookupString(seriesUID); err != nil || got != "2.25.71" {
				t.Fatalf("nested Series Instance UID = %q, %v", got, err)
			}
		})
	}
}

func TestStreamDecoderMalformedArrayKeepsOnlyCompletedDatasets(t *testing.T) {
	for _, tc := range []struct {
		name      string
		document  string
		completed int
	}{
		{"scalar item", `[67]`, 0},
		{"array item", `[[]]`, 0},
		{"missing value", `[{"00100020":}]`, 0},
		{"partial escape", `[{"00100020":{"vr":"LO","Value":["\u00`, 0},
		{"trailing comma", `[{},]`, 1},
		{"unclosed array", `[{} `, 1},
		{"no separator", `[{} {}]`, 1},
		{"object after array", `[{}]{}`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			completed := 0
			decoder := NewDecoder(strings.NewReader(tc.document), std.Dictionary, UnmarshalOptions{})
			err := decoder.DecodeArrayContext(context.Background(), func(*object.Object) error {
				completed++
				return nil
			})
			if err == nil || completed != tc.completed {
				t.Fatalf("completed %d datasets, want %d and a parse error; got %v", completed, tc.completed, err)
			}
		})
	}
}
