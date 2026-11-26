package dimsecli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestQueryRejectsEveryPixelRepresentation(t *testing.T) {
	for _, tt := range []struct {
		name  string
		tag   core.Tag
		vr    core.VR
		value core.Value
	}{
		{"PixelData", core.NewTag(0x7fe0, 0x0010), core.VROB, core.RawValue{1, 2}},
		{"FloatPixelData", core.NewTag(0x7fe0, 0x0008), core.VROF, core.Float32Value{1}},
		{"DoubleFloatPixelData", core.NewTag(0x7fe0, 0x0009), core.VROD, core.Float64Value{1}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Run("key", func(t *testing.T) {
				for _, key := range []string{tt.name, fmt.Sprint(tt.tag), "ScheduledProcedureStepSequence[0]." + tt.name} {
					if _, _, err := ParseQueryKey(key); err == nil {
						t.Errorf("pixel representation admitted as key")
					}
				}
			})
			t.Run("file", func(t *testing.T) {
				id := queryTestObject()
				id.Put(core.Element{Header: core.ElementHeader{Tag: tt.tag, VR: tt.vr}, Value: tt.value})
				var data bytes.Buffer
				if err := object.WriteDataSet(&data, id, transfer.ExplicitVRLittleEndian); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(t.TempDir(), "query.ds")
				if err := os.WriteFile(path, data.Bytes(), 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := (QueryFlags{Files: []string{path}}).Load(context.Background()); err == nil {
					t.Fatal("pixel representation admitted from file")
				}
			})
			t.Run("nested", func(t *testing.T) {
				id := queryTestObject()
				id.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0040, 0x0100), VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{{Header: core.ElementHeader{Tag: tt.tag, VR: tt.vr}, Value: tt.value}}}}}})
				if _, err := ValidateQuery(context.Background(), id); err == nil {
					t.Fatal("pixel representation admitted in sequence item")
				}
			})
		})
	}
}

func TestQueryDecodeErrorPreservesCauseWithoutPayload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "query.json")
	if err := os.WriteFile(path, []byte(`{"00280010":{"vr":"US","Value":["SYN_SECRET_1355"]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := (QueryFlags{Files: []string{path}}).Load(context.Background())
	if err == nil || strings.Contains(err.Error(), "SYN_SECRET_1355") || errors.Unwrap(err) == nil || !strings.Contains(errors.Unwrap(err).Error(), "SYN_SECRET_1355") {
		t.Fatal("safe query diagnostic lost its decoder cause or exposes payload")
	}
	if !errors.Is(queryDecodeError{cause: context.Canceled}, context.Canceled) || !errors.Is(queryDecodeError{cause: ErrQueryLimit}, ErrQueryLimit) {
		t.Fatal("safe query diagnostic lost cancellation/limit identity")
	}
}

func queryTestObject() *object.Object {
	return object.FromElements([]core.Element{{Header: core.ElementHeader{Tag: core.NewTag(0x0010, 0x0010), VR: core.VRPN}, Value: core.StringValue{"SYNTHETIC^QUERY"}}, {Header: core.ElementHeader{Tag: core.NewTag(0x0020, 0x000d), VR: core.VRUI}, Value: core.StringValue{"1.2.826.0.1.3680043.10.1355.1"}}}, std.Dictionary)
}
func TestQueryFilesRawSyntaxAndOrderedKeys(t *testing.T) {
	for _, tt := range []struct {
		name   string
		syntax transfer.Syntax
	}{{"explicit-le", transfer.ExplicitVRLittleEndian}, {"implicit-le", transfer.ImplicitVRLittleEndian}, {"explicit-be", transfer.ExplicitVRBigEndian}} {
		t.Run(tt.name, func(t *testing.T) {
			var b bytes.Buffer
			if err := object.WriteDataSet(&b, queryTestObject(), tt.syntax); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "query.ds")
			if err := os.WriteFile(path, b.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			q := QueryFlags{Files: []string{path}, RawSyntax: tt.name, Keys: []string{"PatientName=FIRST*", "0010,0010=LAST*", "ScheduledProcedureStepSequence[0].Modality=CT", "ScheduledProcedureStepSequence[0].ScheduledProcedureStepStartDate=20261001-20261031", "PatientID"}}
			ids, err := q.Load(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if err := q.Apply(context.Background(), ids[0]); err != nil {
				t.Fatal(err)
			}
			if got, _ := ids[0].GetString(core.NewTag(0x0010, 0x0010)); got != "LAST*" {
				t.Fatalf("ordered replacement=%q", got)
			}
			if got, _ := ids[0].GetString(core.NewTag(0x0010, 0x0020)); got != "" {
				t.Fatalf("empty key=%q", got)
			}
			items, ok := ids[0].GetSequence(core.NewTag(0x0040, 0x0100))
			if !ok || len(items) != 1 {
				t.Fatal("SPS item missing")
			}
			if got, _ := items[0].GetString(core.NewTag(0x0040, 0x0002)); got != "20261001-20261031" {
				t.Fatalf("range changed=%q", got)
			}
		})
	}
}
func TestQueryLimitsAndCancellationBeforeEdit(t *testing.T) {
	for _, key := range []string{"ScheduledProcedureStepSequence[256].Modality=CT", "(7fe0,0010)=", "(0000,0100)=", "UnknownAttribute=", "PatientName=" + string(bytes.Repeat([]byte{'x'}, MaxQueryKeyBytes))} {
		q := QueryFlags{}
		fs := flag.NewFlagSet("q", flag.ContinueOnError)
		q.Register(fs)
		if err := fs.Parse([]string{"-k", key}); err == nil {
			t.Fatalf("admitted %q", key[:min(len(key), 60)])
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	q := QueryFlags{Keys: []string{"PatientName=X"}}
	id := queryTestObject()
	if err := q.Apply(ctx, id); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v", err)
	}
	if got, _ := id.GetString(core.NewTag(0x0010, 0x0010)); got != "SYNTHETIC^QUERY" {
		t.Fatal("edited canceled identifier")
	}
	path := filepath.Join(t.TempDir(), "too-big")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(MaxQueryBytes + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := (QueryFlags{Files: []string{path}}).Load(context.Background()); !errors.Is(err, ErrQueryLimit) {
		t.Fatalf("byte cap=%v", err)
	}
}
func TestQueryJSONAndPixelBulkRefusal(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "query.json")
	data := `[{"00100020":{"vr":"LO","Value":["SYNTHETIC"]}}]`
	if err := os.WriteFile(p, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	ids, err := (QueryFlags{Files: []string{p}}).Load(context.Background())
	if err != nil || len(ids) != 1 {
		t.Fatalf("JSON=%v", err)
	}
	id := queryTestObject()
	id.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x7fe0, 0x0010), VR: core.VROB}, Value: core.RawValue{1, 2}})
	if _, err := ValidateQuery(context.Background(), id); err == nil {
		t.Fatal("query Pixel Data admitted")
	}
	id.Remove(core.NewTag(0x7fe0, 0x0010))
	id.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0011, 0x1010), VR: core.VROB}, Value: core.BulkDataValue{URI: "external"}})
	if _, err := ValidateQuery(context.Background(), id); err == nil {
		t.Fatal("external bulk admitted")
	}
}
