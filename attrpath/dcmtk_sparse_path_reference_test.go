package attrpath

import (
	"bytes"
	"errors"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// dcmdata/tests/tpath.cc checks that wildcards address existing sequence
// items. Sparse branches exercise concrete path resolution and editing across
// missing sequences, empty sequences and items without the requested leaf.
func TestDCMTKReferenceSparseWildcardPaths(t *testing.T) {
	child := func(id string) core.DataSet {
		return dccondDataSet(dccondElement(tagPatientID, core.VRLO, core.StringValue{id}))
	}
	content := func(children ...core.DataSet) core.Element {
		return dccondElement(tagContentSequence, core.VRSQ, core.SequenceValue{Items: children})
	}
	newDataset := func() *object.Object {
		return object.FromElements([]core.Element{content(
			dccondDataSet(content(child("FIRST"), core.DataSet{})),
			core.DataSet{},
			dccondDataSet(content()),
			dccondDataSet(content(core.DataSet{}, child("SECOND"), child("THIRD"))),
		)}, std.Dictionary)
	}

	const itemPattern = "ContentSequence[*].ContentSequence[*]"
	const leafPattern = itemPattern + ".PatientID"
	assertSelected := func(t *testing.T, dataset *object.Object, path string, want ...string) {
		t.Helper()
		matches, err := Find(dataset, MustParse(path))
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, match := range matches {
			if match.Path.Wildcard() {
				t.Fatalf("unresolved wildcard in path %s", match.Path)
			}
			value := match.Element.StringValue()
			if match.Item != nil {
				value, _ = match.Item.GetString(tagPatientID)
			}
			got = append(got, match.Path.String()+"="+value)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("%s selected %v, want %v", path, got, want)
		}
	}

	t.Run("concrete attribute paths", func(t *testing.T) {
		assertSelected(t, newDataset(), leafPattern,
			"ContentSequence[0].ContentSequence[0].PatientID=FIRST",
			"ContentSequence[3].ContentSequence[1].PatientID=SECOND",
			"ContentSequence[3].ContentSequence[2].PatientID=THIRD",
		)
	})
	t.Run("concrete item paths include empty items", func(t *testing.T) {
		assertSelected(t, newDataset(), itemPattern,
			"ContentSequence[0].ContentSequence[0]=FIRST",
			"ContentSequence[0].ContentSequence[1]=",
			"ContentSequence[3].ContentSequence[0]=",
			"ContentSequence[3].ContentSequence[1]=SECOND",
			"ContentSequence[3].ContentSequence[2]=THIRD",
		)
	})
	t.Run("selected item is detached", func(t *testing.T) {
		dataset := newDataset()
		const item = "ContentSequence[3].ContentSequence[1]"
		matches, err := Find(dataset, MustParse(item))
		if err != nil || len(matches) != 1 || matches[0].Item == nil {
			t.Fatalf("Find %s = %+v, %v", item, matches, err)
		}
		matches[0].Item.Remove(tagPatientID)
		assertSelected(t, dataset, item+".PatientID", item+".PatientID=SECOND")
	})
	t.Run("wildcard modification preserves sparse branches", func(t *testing.T) {
		dataset := newDataset()
		n, err := Modify(dataset, MustParse(leafPattern), "REVISED", Options{})
		if err != nil || n != 3 {
			t.Fatalf("Modify = %d, %v, want three leaves", n, err)
		}
		assertSelected(t, dataset, leafPattern,
			"ContentSequence[0].ContentSequence[0].PatientID=REVISED",
			"ContentSequence[3].ContentSequence[1].PatientID=REVISED",
			"ContentSequence[3].ContentSequence[2].PatientID=REVISED",
		)
	})
	t.Run("indexed removal under wildcard rebases remaining paths", func(t *testing.T) {
		dataset := newDataset()
		n, err := Remove(dataset, MustParse("ContentSequence[*].ContentSequence[1]"))
		if err != nil || n != 2 {
			t.Fatalf("Remove = %d, %v, want two items", n, err)
		}
		assertSelected(t, dataset, itemPattern,
			"ContentSequence[0].ContentSequence[0]=FIRST",
			"ContentSequence[3].ContentSequence[0]=",
			"ContentSequence[3].ContentSequence[1]=THIRD",
		)
	})
	t.Run("creation below wildcard stays inside existing items", func(t *testing.T) {
		dataset := newDataset()
		path := MustParse(itemPattern + ".PatientName")
		n, err := Create(dataset, path, Options{})
		if err != nil || n != 5 {
			t.Fatalf("Create = %d, %v, want five leaves", n, err)
		}
		assertSelected(t, dataset, path.String(),
			"ContentSequence[0].ContentSequence[0].PatientName=",
			"ContentSequence[0].ContentSequence[1].PatientName=",
			"ContentSequence[3].ContentSequence[0].PatientName=",
			"ContentSequence[3].ContentSequence[1].PatientName=",
			"ContentSequence[3].ContentSequence[2].PatientName=",
		)
		if _, err := Find(dataset, MustParse("ContentSequence[1].ContentSequence")); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing sequence = %v, want ErrNotFound", err)
		}
		if _, err := Find(dataset, MustParse("ContentSequence[2].ContentSequence[*]")); !errors.Is(err, ErrNotFound) {
			t.Fatalf("empty sequence = %v, want ErrNotFound", err)
		}
	})

	// The defined form uses four 12-byte SQ headers, nine 8-byte Item
	// headers, three 8-byte LO headers and three padded 6-byte values.
	// In the undefined form each of the 13 containers adds an 8-byte delimiter.
	for _, tc := range []struct {
		name string
		form object.SequenceLengthPolicy
		size int
	}{
		{"defined lengths", object.SequenceLengthDefined, 162},
		{"undefined lengths", object.SequenceLengthUndefined, 266},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var data bytes.Buffer
			syntax := transfer.ExplicitVRLittleEndian
			if err := object.WriteDataSetWithOptions(&data, newDataset(), syntax, object.WriteDataSetOptions{SequenceLength: tc.form}); err != nil {
				t.Fatal(err)
			}
			if data.Len() != tc.size {
				t.Fatalf("encoded length = %d, want %d", data.Len(), tc.size)
			}
			decoded, err := object.ReadDataSet(&data, syntax)
			if err != nil {
				t.Fatal(err)
			}
			assertSelected(t, decoded, leafPattern,
				"ContentSequence[0].ContentSequence[0].PatientID=FIRST",
				"ContentSequence[3].ContentSequence[1].PatientID=SECOND",
				"ContentSequence[3].ContentSequence[2].PatientID=THIRD",
			)
		})
	}
}
