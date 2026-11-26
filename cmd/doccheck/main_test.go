package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

const (
	helpProbeExitEnv = "DOCCHECK_TEST_HELP_EXIT"
	helpProbeReadme  = "```sh\ngo run ./cmd/probe -h\n```\n"
)

func TestMain(m *testing.M) {
	if exit := os.Getenv(helpProbeExitEnv); exit != "" {
		os.Exit(runHelpProbe(exit))
	}
	os.Exit(m.Run())
}

func runHelpProbe(exit string) int {
	if !slices.Equal(os.Args[1:], []string{"run", "./cmd/probe", "-h"}) {
		fmt.Fprintln(os.Stderr, "unexpected probe arguments:", os.Args[1:])
		return 3
	}
	document, err := os.ReadFile("README.md")
	if err != nil || string(document) != helpProbeReadme {
		fmt.Fprintln(os.Stderr, "probe did not run in the fixture module")
		return 3
	}
	if os.Getenv("GOPROXY") != "off" || os.Getenv("GOSUMDB") != "off" {
		fmt.Fprintln(os.Stderr, "module services were not disabled")
		return 3
	}
	for _, key := range []string{"HTTP_PROXY", "DICOMGO_DOCCHECK_TEST", "PACS_DOCCHECK_TEST"} {
		if _, exists := os.LookupEnv(key); exists {
			fmt.Fprintln(os.Stderr, "unexpected inherited environment variable:", key)
			return 3
		}
	}
	switch exit {
	case "0":
		fmt.Fprintln(os.Stdout, "Usage: probe")
		return 0
	case "2":
		fmt.Fprintln(os.Stderr, "Usage: probe")
		return 2
	default:
		fmt.Fprintln(os.Stderr, "invalid probe exit status:", exit)
		return 3
	}
}

func TestCheckLinksReportsMissingTargetsAndAnchorsWithLines(t *testing.T) {
	repository := t.TempDir()
	module := filepath.Join(repository, "module")
	writeTestFile(t, filepath.Join(module, "README.md"), strings.Join([]string{
		"# Read me",
		"",
		"[missing](missing.md)",
		"[bad anchor](guide.md#not-there)",
	}, "\n"))
	writeTestFile(t, filepath.Join(module, "guide.md"), "# Present\n")

	validation := newTestChecker(repository, "module", []string{"README.md", "guide.md"})
	if err := validation.run(); err != nil {
		t.Fatal(err)
	}

	want := []diagnostic{
		{Path: "module/README.md", Line: 3, Rule: "links", Message: `missing local target "module/missing.md"`},
		{Path: "module/README.md", Line: 4, Rule: "anchors", Message: "missing anchor #not-there in module/guide.md"},
	}
	assertDiagnostics(t, validation.diagnostics, want)
}

func TestCheckLinksSupportsReferencesImagesEscapesAndDuplicateAnchors(t *testing.T) {
	repository := t.TempDir()
	module := filepath.Join(repository, "module")
	writeTestFile(t, filepath.Join(module, "README.md"), strings.Join([]string{
		"[second duplicate](guide.md#repeat-1-1)",
		"[parentheses](name_(final).md)",
		"![fixture][image]",
		"[encoded](space%20name.md#ol%C3%A1)",
		"`[not a link](missing.md)`",
		"[image]: assets/pixel.png",
		"[external](https://example.test/not-opened)",
	}, "\n"))
	writeTestFile(t, filepath.Join(module, "guide.md"), "# Repeat\n# Repeat\n# Repeat-1\nTitle\n-----\n")
	writeTestFile(t, filepath.Join(module, "name_(final).md"), "# Parentheses\n")
	writeTestFile(t, filepath.Join(module, "space name.md"), "# Olá\n")
	writeTestFile(t, filepath.Join(module, "assets", "pixel.png"), "fixture")

	validation := newTestChecker(repository, "module", []string{"."})
	if err := validation.run(); err != nil {
		t.Fatal(err)
	}
	if len(validation.diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics: %#v", validation.diagnostics)
	}
	if validation.linkCount != 4 {
		t.Fatalf("linkCount = %d, want 4", validation.linkCount)
	}
}

func TestCheckLinksIgnoresFencedExamplesWithDifferentMarker(t *testing.T) {
	repository := t.TempDir()
	module := filepath.Join(repository, "module")
	writeTestFile(t, filepath.Join(module, "README.md"), strings.Join([]string{
		"# Read me",
		"```md",
		"[also missing](other.md)",
		"~~~",
		"[still fenced](third.md)",
		"```",
	}, "\n"))

	validation := newTestChecker(repository, "module", []string{"README.md"})
	if err := validation.run(); err != nil {
		t.Fatal(err)
	}
	if len(validation.diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics: %#v", validation.diagnostics)
	}
}

func TestReasonedDirectiveExemptsOnlyNamedRule(t *testing.T) {
	repository := t.TempDir()
	module := filepath.Join(repository, "module")
	writeTestFile(t, filepath.Join(module, "README.md"), strings.Join([]string{
		`<!-- doccheck: exempt=links reason="Generated paths are intentionally archival." -->`,
		"[missing path](absent.md)",
		"[missing anchor](guide.md#absent)",
	}, "\n"))
	writeTestFile(t, filepath.Join(module, "guide.md"), "# Present\n")

	validation := newTestChecker(repository, "module", []string{"README.md", "guide.md"})
	if err := validation.run(); err != nil {
		t.Fatal(err)
	}
	want := []diagnostic{{
		Path: "module/README.md", Line: 3, Rule: "anchors", Message: "missing anchor #absent in module/guide.md",
	}}
	assertDiagnostics(t, validation.diagnostics, want)
}

func TestCheckLinksReportsUndefinedReferenceAndUnclosedFence(t *testing.T) {
	repository := t.TempDir()
	module := filepath.Join(repository, "module")
	writeTestFile(t, filepath.Join(module, "README.md"), "[missing][definition]\n\n```md\n")

	validation := newTestChecker(repository, "module", []string{"README.md"})
	if err := validation.run(); err != nil {
		t.Fatal(err)
	}
	want := []diagnostic{
		{Path: "module/README.md", Line: 3, Rule: "links", Message: "unclosed fenced code block"},
		{Path: "module/README.md", Line: 1, Rule: "links", Message: `undefined link reference "definition"`},
	}
	assertDiagnostics(t, validation.diagnostics, want)
}

func TestSetextAnchorRequiresImmediatelyPrecedingText(t *testing.T) {
	repository := t.TempDir()
	module := filepath.Join(repository, "module")
	writeTestFile(t, filepath.Join(module, "README.md"), "[not a heading](guide.md#paragraph)\n")
	writeTestFile(t, filepath.Join(module, "guide.md"), "Paragraph\n\n---\n")

	validation := newTestChecker(repository, "module", []string{"README.md", "guide.md"})
	if err := validation.run(); err != nil {
		t.Fatal(err)
	}
	want := []diagnostic{{
		Path: "module/README.md", Line: 1, Rule: "anchors", Message: "missing anchor #paragraph in module/guide.md",
	}}
	assertDiagnostics(t, validation.diagnostics, want)
}

func TestLoadConfigRejectsExemptionWithoutReason(t *testing.T) {
	name := filepath.Join(t.TempDir(), "doccheck.json")
	writeTestFile(t, name, `{"module":"module","include":["."],"exemptions":[{"pattern":"old/**","rules":["links"],"reason":""}]}`)

	_, err := loadConfig(name)
	if err == nil || !strings.Contains(err.Error(), "requires pattern, rules, and reason") {
		t.Fatalf("loadConfig() error = %v", err)
	}
}

func TestLoadConfigRejectsHelpCommandWithExtraArguments(t *testing.T) {
	name := filepath.Join(t.TempDir(), "doccheck.json")
	writeTestFile(t, name, `{
  "module":"module",
  "include":["README.md"],
  "help_commands":[{
    "document":"README.md",
    "argv":["go","run","./cmd/probe","server","-h"],
    "contains":"usage",
    "timeout_seconds":10
  }]
}`)

	_, err := loadConfig(name)
	if err == nil || !strings.Contains(err.Error(), "exact [go run ./cmd/name -h] argv") {
		t.Fatalf("loadConfig() error = %v", err)
	}
}

func TestBoundedBufferLimitsCapturedCommandOutput(t *testing.T) {
	buffer := &boundedBuffer{limit: 4}
	payload := []byte("123456")
	written, err := buffer.Write(payload)
	if err != nil || written != len(payload) || buffer.String() != "1234" {
		t.Fatalf("Write() = (%d, %v), buffer %q", written, err, buffer.String())
	}
}

func TestCheckCurrentSectionUsesHeadingBoundsAndIssueAllowlist(t *testing.T) {
	repository := t.TempDir()
	module := filepath.Join(repository, "module")
	writeTestFile(t, filepath.Join(module, "ROADMAP.md"), strings.Join([]string{
		"# Roadmap",
		"",
		"## Current",
		"Epic #809 remains current.",
		"Issue #123 is obsolete.",
		"[Hidden label](https://github.com/acme/project/issues/124) is obsolete.",
		"`#456` is an example, not a reference.",
		"```md",
		"## Not a boundary",
		"```",
		"## History",
		"Issue #321 is historical.",
	}, "\n"))

	validation := newTestChecker(repository, "module", []string{"ROADMAP.md"})
	validation.configuration.CurrentSections = []currentSection{{
		Path:          "ROADMAP.md",
		Heading:       "Current",
		AllowedIssues: []int{809},
	}}
	if err := validation.run(); err != nil {
		t.Fatal(err)
	}

	want := []diagnostic{
		{Path: "module/ROADMAP.md", Line: 5, Rule: "issues", Message: `issue #123 is not allowed in explicitly current section "Current"`},
		{Path: "module/ROADMAP.md", Line: 6, Rule: "issues", Message: `issue #124 is not allowed in explicitly current section "Current"`},
	}
	assertDiagnostics(t, validation.diagnostics, want)
}

func TestCheckHelpDocumentExecutesDocumentedCommandOffline(t *testing.T) {
	validation := newHelpProbeChecker(t, "0")
	if err := validation.run(); err != nil {
		t.Fatal(err)
	}
	if len(validation.diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics: %#v", validation.diagnostics)
	}
	if validation.helpCount != 1 {
		t.Fatalf("helpCount = %d, want 1", validation.helpCount)
	}
}

func TestCheckHelpDocumentAcceptsDocumentedHelpExitStatus(t *testing.T) {
	validation := newHelpProbeChecker(t, "2")
	if err := validation.run(); err != nil {
		t.Fatal(err)
	}
	if len(validation.diagnostics) != 0 || validation.helpCount != 1 {
		t.Fatalf("diagnostics/helpCount = %#v / %d, want no diagnostics and 1", validation.diagnostics, validation.helpCount)
	}
}

func newHelpProbeChecker(t *testing.T, exit string) *checker {
	t.Helper()
	repository := t.TempDir()
	writeTestFile(t, filepath.Join(repository, "module", "README.md"), helpProbeReadme)

	// Let doccheck resolve the documented Go command normally, with the
	// already compiled test binary standing in for the toolchain.
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	name := "go"
	if runtime.GOOS == "windows" {
		name = "go.exe"
		t.Setenv("PATHEXT", ".EXE")
	}
	if err := os.WriteFile(filepath.Join(binDir, name), binary, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	t.Setenv(helpProbeExitEnv, exit)
	t.Setenv("GOPROXY", "https://modules.example.invalid")
	t.Setenv("GOSUMDB", "sum.example.invalid")
	t.Setenv("HTTP_PROXY", "https://proxy.example.invalid")
	t.Setenv("DICOMGO_DOCCHECK_TEST", "fixture")
	t.Setenv("PACS_DOCCHECK_TEST", "fixture")

	validation := newTestChecker(repository, "module", []string{"README.md"})
	validation.configuration.HelpCommands = []helpCommand{{
		Document: "README.md", Argv: []string{"go", "run", "./cmd/probe", "-h"}, Contains: "usage", TimeoutSeconds: 10,
	}}
	return validation
}

func TestCheckPathReferenceRequiresDocumentedTextAndExistingTarget(t *testing.T) {
	repository := t.TempDir()
	module := filepath.Join(repository, "module")
	writeTestFile(t, filepath.Join(module, "README.md"), "Implementation: `internal/engine`.\n")
	writeTestFile(t, filepath.Join(module, "internal", "engine", "engine.go"), "package engine\n")

	validation := newTestChecker(repository, "module", []string{"README.md"})
	validation.configuration.PathReferences = []pathReference{{
		Document: "README.md", Text: "internal/engine", Target: "module/internal/engine",
	}}
	if err := validation.run(); err != nil {
		t.Fatal(err)
	}
	if len(validation.diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics: %#v", validation.diagnostics)
	}
}

func newTestChecker(repository, module string, include []string) *checker {
	return &checker{
		repositoryRoot: repository,
		moduleRoot:     filepath.Join(repository, module),
		configuration: config{
			Module:  module,
			Include: include,
		},
		anchorCache: make(map[string]map[string]bool),
	}
}

func writeTestFile(t *testing.T, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertDiagnostics(t *testing.T, got, want []diagnostic) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("diagnostics = %#v, want %#v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("diagnostics[%d] = %#v, want %#v", index, got[index], want[index])
		}
	}
}
