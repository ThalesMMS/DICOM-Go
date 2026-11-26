package dicomjson

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
)

func TestSingleDatasetRequiresObject(t *testing.T) {
	for _, read := range []struct {
		name string
		run  func(string) (*object.Object, error)
	}{
		{"resident", func(input string) (*object.Object, error) {
			return Unmarshal([]byte(input), std.Dictionary)
		}},
		{"stream", func(input string) (*object.Object, error) {
			return NewDecoder(strings.NewReader(input), std.Dictionary, UnmarshalOptions{}).Decode()
		}},
	} {
		t.Run(read.name, func(t *testing.T) {
			for _, input := range []string{" \n null \t", "[]", "77", "true", `"value"`} {
				if got, err := read.run(input); got != nil || !errors.Is(err, ErrExpectedDatasetObject) || !strings.Contains(err.Error(), " at $") {
					t.Errorf("%q: dataset present = %t, error = %v, want object error at root", input, got != nil, err)
				}
			}
			if got, err := read.run("{}"); err != nil || got == nil || got.Len() != 0 {
				t.Fatalf("empty object = %v, %v, want an empty dataset", got, err)
			}
			if got, err := read.run(`{"0020000E":{"vr":"UI"},"0020000\u0045":{"vr":"UI"}}`); got != nil || !errors.Is(err, ErrDuplicateDatasetTag) {
				t.Errorf("escaped duplicate: dataset present = %t, error = %v, want duplicate-tag error", got != nil, err)
			}
		})
	}
}

func TestDatasetArraysVerifyShapeBeforeDelivering(t *testing.T) {
	for _, read := range []struct {
		name string
		run  func(*Decoder, func()) error
	}{
		{"object", func(decoder *Decoder, handle func()) error {
			return decoder.DecodeArrayContext(context.Background(), func(*object.Object) error { handle(); return nil })
		}},
		{"validated", func(decoder *Decoder, handle func()) error {
			return decoder.DecodeDatasetsContext(context.Background(), func(Dataset) error { handle(); return nil })
		}},
		{"raw", func(decoder *Decoder, handle func()) error {
			return decoder.DecodeRawDatasetsContext(context.Background(), func(Dataset) error { handle(); return nil })
		}},
	} {
		t.Run(read.name, func(t *testing.T) {
			for _, tc := range []struct {
				input string
				count int
				cause error
				path  string
			}{
				{"null", 0, ErrExpectedDatasetArray, ""},
				{"{}", 0, ErrExpectedDatasetArray, ""},
				{"[]", 0, nil, ""},
				{"[{}]", 1, nil, ""},
				{"[null,{}]", 0, ErrExpectedDatasetObject, "[0]"},
				{"[{},null,{}]", 1, ErrExpectedDatasetObject, "[1]"},
				{"[{} ,[]]", 1, ErrExpectedDatasetObject, "[1]"},
				{`[{"0020000E":{"vr":"UI"},"0020000E":{"vr":"UI"}}]`, 0, ErrDuplicateDatasetTag, "[0]/0020000E"},
				{`[{},{"0020000E":{"vr":"UI"}," 0020000e ":{"vr":"UI"}},{}]`, 1, ErrDuplicateDatasetTag, "[1]/0020000E"},
				{`[{"0020000E":{"vr":"UI"}},{"0020000E":{"vr":"UI"}}]`, 2, nil, ""},
			} {
				count := 0
				decoder := NewDecoder(strings.NewReader(tc.input), std.Dictionary, UnmarshalOptions{})
				err := read.run(decoder, func() { count++ })
				if count != tc.count || !errors.Is(err, tc.cause) || tc.path != "" && !strings.Contains(err.Error(), tc.path) {
					t.Errorf("%s: callbacks = %d, error = %v, want %d, %v at %q", tc.input, count, err, tc.count, tc.cause, tc.path)
				}
			}
		})
	}
}

func TestRawDatasetArraysPreserveElementRepresentation(t *testing.T) {
	const input = `[{}, {
		"00280010":{"vr":"US","Value":[65536],"PeerField":true},
		"00660040":{"vr":"UV","Value":[18446744073709551615]},
		"00081111":{"vr":"SQ","Value":[null, {"0020000E":{"vr":"UI","Value":["2.25.3"]}}]}
	}]`
	legacy := json.NewDecoder(strings.NewReader(input))
	legacy.UseNumber()
	var want []Dataset
	if err := legacy.Decode(&want); err != nil {
		t.Fatal(err)
	}
	var got []Dataset
	decoder := NewDecoder(strings.NewReader(input), std.Dictionary, UnmarshalOptions{})
	err := decoder.DecodeRawDatasetsContext(context.Background(), func(dataset Dataset) error {
		got = append(got, dataset)
		return nil
	})
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("raw decode = %#v, %v, want %#v", got, err, want)
	}
}

func TestRawDatasetArraysRespectCumulativeLimits(t *testing.T) {
	const input = `[{"0020000E":{"vr":"UI"}},{"0020000E":{"vr":"UI"}}]`
	for _, tc := range []struct {
		limits Limits
		cause  error
	}{
		{Limits{MaxElements: 1}, ErrMaxElementsExceeded},
		{Limits{MaxJSONBytes: int64(len(input)) - 2}, ErrMaxJSONBytesExceeded},
	} {
		count := 0
		decoder := NewDecoder(strings.NewReader(input), std.Dictionary, UnmarshalOptions{Limits: tc.limits})
		err := decoder.DecodeRawDatasetsContext(context.Background(), func(Dataset) error {
			count++
			return nil
		})
		if count != 1 || !errors.Is(err, tc.cause) {
			t.Errorf("limits %+v: callbacks = %d, error = %v, want 1 and %v", tc.limits, count, err, tc.cause)
		}
	}
}

func TestDuplicateMalformedDatasetKeyIsNotInDiagnostic(t *testing.T) {
	const canary = "CANARY_PATIENT_TEXT"
	decoder := NewDecoder(strings.NewReader(`[{"`+canary+`":{"vr":"UI"},"`+canary+`":{"vr":"UI"}}]`), nil, UnmarshalOptions{})
	err := decoder.DecodeRawDatasetsContext(context.Background(), func(Dataset) error {
		t.Error("malformed dataset was delivered")
		return nil
	})
	diagnostic := fmt.Sprintf("%+v", err)
	if !errors.Is(err, ErrDuplicateDatasetTag) || !strings.Contains(diagnostic, "at [0]") || strings.Contains(diagnostic, canary) {
		t.Fatalf("unexpected duplicate diagnostic %q", diagnostic)
	}
}

func TestEncodeNilRepresentedDatasetAsEmptyObject(t *testing.T) {
	var out bytes.Buffer
	if err := EncodeDatasetContext(context.Background(), &out, nil, DatasetOptions{}); err != nil || out.String() != "{}" {
		t.Fatalf("nil dataset = %q, %v, want empty JSON object", out.String(), err)
	}
}
