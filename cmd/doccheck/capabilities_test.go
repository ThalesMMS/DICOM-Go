package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestCapabilityInventoryRejectsContradictoryDuplicateAreas(t *testing.T) {
	repository := t.TempDir()
	name := filepath.Join(repository, "module", "docs", "CAPABILITIES.md")
	writeTestFile(t, name, strings.Join([]string{
		"## Supported surfaces", "", "| Area | Surface |", "| --- | --- |",
		"| Image processing and registration | HDF5 absent |",
		"|  image  processing and registration  | HDF5 implemented |",
	}, "\n"))
	validation := newTestChecker(repository, "module", []string{"docs"})
	if err := validation.run(); err != nil {
		t.Fatal(err)
	}
	assertDiagnostics(t, validation.diagnostics, []diagnostic{{
		Path: "module/docs/CAPABILITIES.md", Line: 6, Rule: "capabilities",
		Message: `duplicate capability area "image processing and registration" (first at line 5)`,
	}})
}

func TestCapabilityInventoryScopesRowsAndRequiresContent(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		wantMissing   bool
	}{
		{"valid", "## Supported surfaces\n| Area | Surface |\n| --- | --- |\n| Images | Decoding |\n```md\n| Images | Example |\n```\n## History\n| Images | Historical |\n", false},
		{"separate example table", "## Supported surfaces\n| Area | Surface |\n| --- | --- |\n| Images | Decoding |\n\n### Example operations\n| Action | Details |\n| --- | --- |\n| Images | Example |\n", false},
		{"separate table after prose", "## Supported surfaces\n| Area | Surface |\n| --- | --- |\n| Images | Decoding |\n\nExample:\n| Action | Details |\n| --- | --- |\n| Images | Example |\n", false},
		{"indented example", "## Supported surfaces\n| Area | Surface |\n| --- | --- |\n| Images | Decoding |\n\n    | Area | Surface |\n    | --- | --- |\n    | Images | Example |\n", false},
		{"empty", "## Supported surfaces\n| Area | Surface |\n| --- | --- |\n", true},
		{"missing", "## History\n| Area | Surface |\n| --- | --- |\n| Images | Historical |\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repository := t.TempDir()
			writeTestFile(t, filepath.Join(repository, "module", "docs", "CAPABILITIES.md"), tc.content)
			validation := newTestChecker(repository, "module", []string{"docs"})
			if err := validation.run(); err != nil {
				t.Fatal(err)
			}
			if tc.wantMissing {
				assertDiagnostics(t, validation.diagnostics, []diagnostic{{Path: "module/docs/CAPABILITIES.md", Line: 1, Rule: "capabilities", Message: "missing or empty Supported surfaces inventory"}})
			} else if len(validation.diagnostics) != 0 {
				t.Fatalf("unexpected diagnostics: %+v", validation.diagnostics)
			}
		})
	}
}
