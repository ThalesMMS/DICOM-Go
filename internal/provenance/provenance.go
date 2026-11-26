// Package provenance verifies THIRD_PARTY_PROVENANCE.json: every artifact
// derived from another project (tables, vectors, fixtures, adapted sources)
// must identify its upstream commit, source files, hashes and license notice.
//
// The check is local and offline. When DICOMGO_REFERENCE_ROOT points at a
// directory holding reference checkouts (dcmtk/, GDCM/, VTK/), source hashes
// are additionally recomputed from the pinned commits with git.
package provenance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ManifestName is the manifest file name at the module root.
const ManifestName = "THIRD_PARTY_PROVENANCE.json"

// NoticesName is the human-readable notice file at the module root.
const NoticesName = "THIRD_PARTY_NOTICES.md"

// UpstreamDataDir holds imported test data; every file in it must be listed.
const UpstreamDataDir = "testdata/upstream"

// Distribution values.
const (
	// DistributionTestData is never compiled or embedded into binaries.
	DistributionTestData = "test-data"
	// DistributionEmbedded is compiled or embedded into base-module binaries.
	DistributionEmbedded = "embedded"
	// DistributionOptionalModule ships only with an optional adapter module.
	DistributionOptionalModule = "optional-module"
)

// Manifest is the decoded provenance manifest.
type Manifest struct {
	Schema    int                 `json:"schema"`
	Upstreams map[string]Upstream `json:"upstreams"`
	Artifacts []Artifact          `json:"artifacts"`
}

// Upstream identifies one pinned upstream project revision and its notice.
type Upstream struct {
	Project       string `json:"project"`
	Repository    string `json:"repository"`
	Commit        string `json:"commit"`
	License       string `json:"license"`
	LicenseFile   string `json:"license_file,omitempty"`
	LicenseSHA256 string `json:"license_sha256,omitempty"`
	LicenseSource string `json:"license_source,omitempty"`
	LicenseURL    string `json:"license_url,omitempty"`
	NoticeHeading string `json:"notice_heading"`
}

// Artifact is one file in this module derived from an upstream.
type Artifact struct {
	Path           string   `json:"path"`
	Upstream       string   `json:"upstream"`
	SHA256         string   `json:"sha256,omitempty"`
	SHA256Policy   string   `json:"sha256_policy,omitempty"`
	Attribution    string   `json:"attribution,omitempty"`
	Distribution   string   `json:"distribution"`
	Transformation string   `json:"transformation"`
	Sources        []Source `json:"sources"`
	GeneratedBy    string   `json:"generated_by,omitempty"`
}

// Source is one upstream file an artifact derives from.
type Source struct {
	File             string `json:"file"`
	Lines            string `json:"lines,omitempty"`
	SHA256           string `json:"sha256,omitempty"`
	URL              string `json:"url,omitempty"`
	UnverifiedReason string `json:"unverified_reason,omitempty"`
}

// Load reads and strictly decodes a manifest.
func Load(file string) (Manifest, error) {
	var m Manifest
	data, err := os.ReadFile(file)
	if err != nil {
		return m, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return m, fmt.Errorf("%s: %w", file, err)
	}
	return m, nil
}

// Options control optional checks.
type Options struct {
	// ReferenceRoot, when set, holds checkouts named by ReferenceCheckouts;
	// source hashes are recomputed from the pinned commit with git.
	ReferenceRoot string
}

// ReferenceCheckouts maps upstream repositories to local checkout names.
var ReferenceCheckouts = map[string]string{
	"https://github.com/DCMTK/dcmtk":                   "dcmtk",
	"https://github.com/malaterre/GDCM":                "GDCM",
	"https://github.com/Kitware/VTK":                   "VTK",
	"https://github.com/InsightSoftwareConsortium/ITK": "ITK",
}

var (
	hex40 = regexp.MustCompile(`^[0-9a-f]{40}$`)
	hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)
	lines = regexp.MustCompile(`^([1-9][0-9]*)-([1-9][0-9]*)$`)
)

// Verify checks the manifest against the module rooted at root and returns
// every problem found, sorted.
func Verify(root string, m Manifest, opt Options) []error {
	var errs []error
	fail := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if m.Schema != 1 {
		fail("manifest schema %d, want 1", m.Schema)
	}
	notices, err := os.ReadFile(filepath.Join(root, NoticesName))
	if err != nil {
		fail("%s: %v", NoticesName, err)
	}
	headings := map[string]bool{}
	for _, l := range strings.Split(strings.ReplaceAll(string(notices), "\r\n", "\n"), "\n") {
		if strings.HasPrefix(l, "#") {
			headings[strings.TrimSpace(l)] = true
		}
	}
	licenseFiles := map[string]bool{}
	for id, u := range m.Upstreams {
		if u.Project == "" || u.License == "" || !strings.HasPrefix(u.Repository, "https://") {
			fail("upstream %s: project, license and https repository are required", id)
		}
		if !hex40.MatchString(u.Commit) {
			fail("upstream %s: commit %q is not a full 40-hex SHA-1", id, u.Commit)
		}
		switch {
		case u.LicenseFile != "":
			licenseFiles[u.LicenseFile] = true
			data, err := readModuleFile(root, u.LicenseFile)
			if err != nil {
				fail("upstream %s: license file: %v", id, err)
			} else if len(bytes.TrimSpace(data)) == 0 {
				fail("upstream %s: license file %s is empty", id, u.LicenseFile)
			} else if !hex64.MatchString(u.LicenseSHA256) {
				fail("upstream %s: license_sha256 is required with license_file", id)
			} else if got := sum(data); got != u.LicenseSHA256 {
				fail("upstream %s: license file %s SHA-256 %s, manifest %s", id, u.LicenseFile, got, u.LicenseSHA256)
			}
		case u.LicenseURL != "":
			if !strings.HasPrefix(u.LicenseURL, "https://") {
				fail("upstream %s: license_url must be https", id)
			}
		default:
			fail("upstream %s: license_file or license_url is required", id)
		}
		if u.NoticeHeading == "" || !headings[u.NoticeHeading] {
			fail("upstream %s: notice heading %q missing from %s", id, u.NoticeHeading, NoticesName)
		}
	}

	listed := map[string]bool{}
	used := map[string]bool{}
	for _, a := range m.Artifacts {
		where := "artifact " + a.Path
		if a.Path == "" || path.Clean(a.Path) != a.Path || strings.HasPrefix(a.Path, "../") || path.IsAbs(a.Path) {
			fail("%s: path must be clean and module-relative", where)
			continue
		}
		if listed[a.Path] {
			fail("%s: listed twice", where)
		}
		listed[a.Path] = true
		u, ok := m.Upstreams[a.Upstream]
		if !ok {
			fail("%s: unknown upstream %q", where, a.Upstream)
		}
		used[a.Upstream] = true
		inTestdata := strings.HasPrefix(a.Path, "testdata/") || strings.Contains(a.Path, "/testdata/")
		switch a.Distribution {
		case DistributionTestData:
			if !inTestdata {
				fail("%s: test-data artifacts must live under a testdata directory", where)
			}
		case DistributionEmbedded, DistributionOptionalModule:
			if inTestdata {
				fail("%s: %s artifact must not live under testdata", where, a.Distribution)
			}
		default:
			fail("%s: distribution %q is not test-data, embedded or optional-module", where, a.Distribution)
		}
		if strings.TrimSpace(a.Transformation) == "" {
			fail("%s: transformation is required", where)
		}
		data, err := readModuleFile(root, a.Path)
		if err != nil {
			fail("%s: %v", where, err)
		} else {
			switch a.SHA256Policy {
			case "", "pinned":
				if !hex64.MatchString(a.SHA256) {
					fail("%s: sha256 is required", where)
				} else if got := sum(data); got != a.SHA256 {
					fail("%s: SHA-256 %s, manifest %s (regenerate or update the manifest)", where, got, a.SHA256)
				}
			case "modified":
				if a.Attribution == "" || !bytes.Contains(data, []byte(a.Attribution)) {
					fail("%s: modified source must contain attribution %q", where, a.Attribution)
				}
			default:
				fail("%s: sha256_policy %q is not pinned or modified", where, a.SHA256Policy)
			}
		}
		if len(a.Sources) == 0 {
			fail("%s: at least one source is required", where)
		}
		for _, s := range a.Sources {
			if s.File == "" {
				fail("%s: source file is required", where)
			}
			if s.SHA256 == "" && s.UnverifiedReason == "" {
				fail("%s: source %s needs sha256 or unverified_reason", where, s.File)
			}
			if s.SHA256 != "" && !hex64.MatchString(s.SHA256) {
				fail("%s: source %s sha256 is malformed", where, s.File)
			}
			if s.Lines != "" {
				mm := lines.FindStringSubmatch(s.Lines)
				if mm == nil {
					fail("%s: source %s lines %q is not first-last", where, s.File, s.Lines)
				} else if a, _ := strconv.Atoi(mm[1]); a > atoi(mm[2]) {
					fail("%s: source %s lines %q are reversed", where, s.File, s.Lines)
				}
			}
			if opt.ReferenceRoot != "" && s.SHA256 != "" && ok {
				if err := verifySource(opt.ReferenceRoot, u, s); err != nil {
					fail("%s: %v", where, err)
				}
			}
		}
	}
	for id := range m.Upstreams {
		// An upstream may be registered ahead of its first import only when
		// its notice already lives beside the imported test data.
		if !used[id] && !strings.HasPrefix(m.Upstreams[id].LicenseFile, UpstreamDataDir+"/") {
			fail("upstream %s: no artifact references it", id)
		}
	}

	dir := filepath.Join(root, filepath.FromSlash(UpstreamDataDir))
	if _, err := os.Stat(dir); err == nil {
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				fail("%s: %v", p, err)
				return nil
			}
			if d.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			rel = filepath.ToSlash(rel)
			if !listed[rel] && !licenseFiles[rel] && path.Base(rel) != "README.md" {
				fail("%s: file has no provenance entry in %s", rel, ManifestName)
			}
			return nil
		})
	}
	sort.Slice(errs, func(i, j int) bool { return errs[i].Error() < errs[j].Error() })
	return errs
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func readModuleFile(root, rel string) ([]byte, error) {
	return os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
}

func verifySource(refRoot string, u Upstream, s Source) error {
	name, ok := ReferenceCheckouts[u.Repository]
	if !ok {
		return nil
	}
	dir := filepath.Join(refRoot, name)
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("reference checkout for %s: %v", u.Repository, err)
	}
	commit := u.Commit
	if len(commit) > 8 {
		commit = commit[:8]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "show", u.Commit+":"+s.File).Output()
	if err != nil {
		return fmt.Errorf("source %s at %s: %v", s.File, commit, err)
	}
	if got := sum(out); got != s.SHA256 {
		return fmt.Errorf("source %s at %s: SHA-256 %s, manifest %s", s.File, commit, got, s.SHA256)
	}
	return nil
}
