package iodmodules

import (
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"testing"
)

func TestConstraintsPinnedProvenanceAndRowBindings(t *testing.T) {
	if ConstraintSourceEdition != StandardVersion || ConstraintSourceSHA256 != SourceSHA256 {
		t.Fatal("constraints use a different DocBook snapshot")
	}
	seen := map[string]bool{}
	compiled, manual := 0, 0
	for table, byIndex := range ConstraintTables() {
		rows, ok := Rows(table)
		if !ok {
			t.Fatalf("unknown table %s", table)
		}
		for index, constraints := range byIndex {
			if index < -1 || index >= len(rows) {
				t.Fatalf("row outside table %s %d", table, index)
			}
			row := Row{}
			if index >= 0 {
				row = rows[index]
			}
			for _, c := range constraints {
				s := c.Source
				if s.Table != table || s.Index != index || (index >= 0 && (s.Tag != row.Tag || s.Depth != row.Depth)) || s.Row < 1 || s.TableID == "" || s.Edition != StandardVersion || s.SHA256 != SourceSHA256 || c.Rule == "" || (index == -1 && s.Tag == 0) {
					t.Fatalf("bad binding: %+v", c)
				}
				if index == -1 && c.Kind != "manual" {
					if len(c.ParentPath) != int(s.Depth) || c.AttributeType == "" {
						t.Fatalf("unproved supplemental binding: %+v", c)
					}
					for depth, parent := range c.ParentPath {
						entry, ok := std.Dictionary.ByTag(core.NewTag(uint16(parent>>16), uint16(parent)))
						bound := false
						for _, row := range rows {
							if row.Tag == parent && int(row.Depth) == depth {
								bound = true
								break
							}
						}
						if !ok || entry.VR != core.VRSQ || !bound {
							t.Fatalf("unproved supplemental parent: %+v", c)
						}
					}
				}
				if seen[c.Rule] {
					t.Fatalf("duplicate rule identifier: %s", c.Rule)
				}
				seen[c.Rule] = true
				if c.Kind == "manual" {
					manual++
					if c.Reason == "" {
						t.Fatal("manual qualification without reason")
					}
				} else {
					compiled++
				}
			}
		}
	}
	if compiled < 1100 || manual < 190 {
		t.Fatalf("compiled=%d manual=%d", compiled, manual)
	}
}
