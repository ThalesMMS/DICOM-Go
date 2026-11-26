package provenance

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const moduleRoot = "../.."

func TestModuleManifest(t *testing.T) {
	m, err := Load(filepath.Join(moduleRoot, ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	opt := Options{ReferenceRoot: os.Getenv("DICOMGO_REFERENCE_ROOT")}
	for _, err := range Verify(moduleRoot, m, opt) {
		t.Error(err)
	}
	if len(m.Artifacts) == 0 {
		t.Fatal("manifest lists no artifacts")
	}
}

// fixture builds a minimal module with one upstream and one test-data artifact.
func fixture(t *testing.T) (string, Manifest) {
	t.Helper()
	root := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(NoticesName, "# Notices\n\n### Example — tables\n\ntext\n")
	write("testdata/upstream/ex/LICENSE.EX", "license text\n")
	write("testdata/upstream/ex/table.json", "{}\n")
	m := Manifest{
		Schema: 1,
		Upstreams: map[string]Upstream{"ex-1": {
			Project: "Example", Repository: "https://example.invalid/ex", Commit: strings.Repeat("a", 40),
			License: "BSD-3-Clause", LicenseFile: "testdata/upstream/ex/LICENSE.EX",
			LicenseSHA256: sum([]byte("license text\n")), NoticeHeading: "### Example — tables",
		}},
		Artifacts: []Artifact{{
			Path: "testdata/upstream/ex/table.json", Upstream: "ex-1", SHA256: sum([]byte("{}\n")),
			Distribution: DistributionTestData, Transformation: "converted",
			Sources: []Source{{File: "t.cxx", Lines: "1-2", SHA256: strings.Repeat("b", 64)}},
		}},
	}
	return root, m
}

func TestVerifyFixtureClean(t *testing.T) {
	root, m := fixture(t)
	if errs := Verify(root, m, Options{}); len(errs) != 0 {
		t.Fatalf("clean fixture: %v", errs)
	}
}

func TestVerifyDetectsProblems(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, root string, m *Manifest)
		want   string
	}{
		{"license removed", func(t *testing.T, root string, m *Manifest) {
			os.Remove(filepath.Join(root, "testdata/upstream/ex/LICENSE.EX"))
		}, "license file"},
		{"license edited", func(t *testing.T, root string, m *Manifest) {
			os.WriteFile(filepath.Join(root, "testdata/upstream/ex/LICENSE.EX"), []byte("changed\n"), 0o644)
		}, "license file testdata/upstream/ex/LICENSE.EX SHA-256"},
		{"notice removed", func(t *testing.T, root string, m *Manifest) {
			os.WriteFile(filepath.Join(root, NoticesName), []byte("# Notices\n"), 0o644)
		}, "missing from THIRD_PARTY_NOTICES.md"},
		{"artifact edited", func(t *testing.T, root string, m *Manifest) {
			os.WriteFile(filepath.Join(root, "testdata/upstream/ex/table.json"), []byte("[]\n"), 0o644)
		}, "regenerate or update the manifest"},
		{"orphan file", func(t *testing.T, root string, m *Manifest) {
			os.WriteFile(filepath.Join(root, "testdata/upstream/ex/extra.json"), []byte("{}"), 0o644)
		}, "has no provenance entry"},
		{"missing source hash", func(t *testing.T, root string, m *Manifest) {
			m.Artifacts[0].Sources[0].SHA256 = ""
		}, "needs sha256 or unverified_reason"},
		{"short commit", func(t *testing.T, root string, m *Manifest) {
			u := m.Upstreams["ex-1"]
			u.Commit = "abc123"
			m.Upstreams["ex-1"] = u
		}, "full 40-hex"},
		{"test data outside testdata", func(t *testing.T, root string, m *Manifest) {
			os.WriteFile(filepath.Join(root, "table.json"), []byte("{}\n"), 0o644)
			m.Artifacts[0].Path = "table.json"
		}, "must live under a testdata directory"},
		{"reversed lines", func(t *testing.T, root string, m *Manifest) {
			m.Artifacts[0].Sources[0].Lines = "9-2"
		}, "reversed"},
		{"modified source without attribution", func(t *testing.T, root string, m *Manifest) {
			m.Artifacts[0].SHA256Policy = "modified"
			m.Artifacts[0].Attribution = "Adapted from example"
		}, "must contain attribution"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, m := fixture(t)
			tc.mutate(t, root, &m)
			errs := Verify(root, m, Options{})
			for _, err := range errs {
				if strings.Contains(err.Error(), tc.want) {
					return
				}
			}
			t.Fatalf("want error containing %q, got %v", tc.want, errs)
		})
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	p := filepath.Join(t.TempDir(), "m.json")
	os.WriteFile(p, []byte(`{"schema":1,"upstreams":{},"artifacts":[],"extra":1}`), 0o644)
	if _, err := Load(p); err == nil {
		t.Fatal("unknown field accepted")
	}
}

func TestVerifySourceCommitDiagnostics(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required for source verification")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "GDCM")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init")
	if err := os.WriteFile(filepath.Join(dir, "source.txt"), []byte("source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "source.txt")
	git("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "commit", "-m", "fixture")
	commit := git("rev-parse", "HEAD")
	for _, tc := range []struct{ name, commit, file, want string }{
		{"empty", "", "missing.txt", "source missing.txt at :"},
		{"short missing", "bad", "source.txt", "source source.txt at bad:"},
		{"short hash mismatch", "HEAD", "source.txt", "source source.txt at HEAD: SHA-256"},
		{"full hash mismatch", commit, "source.txt", "source source.txt at " + commit[:8] + ": SHA-256"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := verifySource(root, Upstream{Repository: "https://github.com/malaterre/GDCM", Commit: tc.commit}, Source{File: tc.file, SHA256: strings.Repeat("0", 64)})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}
