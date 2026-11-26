package dsig

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/ThalesMMS/dicom-go/core"
)

const (
	// ProfileStandardVersion identifies the normative profile list used here.
	ProfileStandardVersion = "PS3.15 2026d"
	// ProfileSourceSHA256 identifies the official PS3.15 DocBook snapshot.
	ProfileSourceSHA256 = "1eb2a9724822476b3f1f3de23d15ae655a4c7f6e9d32bf16f2d9ef76d7d8437c"
	// ProfileModuleSourceSHA256 identifies the official PS3.3 module snapshot.
	ProfileModuleSourceSHA256 = "3ae695b956ba57c490933a49220b515aeaedb5ece86f05c4bf7e94ee0fbcc6ef"
)

var (
	// ErrProfileIncomplete means coverage needs external context or an available
	// reviewed catalog. It is not success, even when no attribute is missing.
	ErrProfileIncomplete = errors.New("dicom dsig: attribute-profile coverage incomplete")
	// ErrProfileLimit means a bounded profile evaluation could not complete.
	ErrProfileLimit = errors.New("dicom dsig: attribute-profile evaluation limit exceeded")
)

// ProfileCoverage describes evaluation coverage, not cryptographic validity,
// certificate trust, IOD validity or the authority/truth of a human declaration.
type ProfileCoverage string

const (
	ProfileCoverageComplete     ProfileCoverage = "complete"
	ProfileCoverageNeedsContext ProfileCoverage = "needs-context"
	ProfileCoveragePartial      ProfileCoverage = "partial"
)

// ProfileOptions configure metadata-only attribute-profile evaluation. Inputs
// are borrowed read-only for the duration of the call; callers must not mutate
// the signature, dataset or options concurrently.
type ProfileOptions struct {
	// HumanVerifiedPaths declares the attributes a human verified (C.3 item 3).
	// Nil means unknown, and fails closed for Authorization. A nonnil empty
	// slice explicitly declares none. The caller is responsible for the truth
	// and authority of this declaration; the library cannot attest either.
	// Paths select existing attributes, including nested attributes. Signing
	// their root sequence covers its children; child tags are not root AT tags.
	// Wildcards and paths ending in an item are rejected. At most 256 paths.
	HumanVerifiedPaths []string
	// Zero selects defaults: 100000 elements/items, depth 64, and 256 issues.
	// Negative values are invalid. Limits count metadata, not payload bytes.
	MaxElements, MaxDepth, MaxIssues int
}

// ProfileIssue has a fixed diagnostic code and tag/canonical path, never a
// dataset value, private creator text, patient identity or certificate detail.
type ProfileIssue struct {
	Code string
	Tag  core.Tag
	Path string
}

// ProfileReport reports attribute coverage. Complete does not mean satisfied:
// Issues and the returned error still describe violations. Verification of
// signature bytes, trust, timestamps, IODs and human authority is separate.
// SRVerificationCandidate means only a structurally eligible purpose-5
// candidate exists; it does not assert its signature bytes verify.
type ProfileReport struct {
	Profile                                           Profile
	StandardVersion, SourceSHA256, ModuleSourceSHA256 string
	Coverage                                          ProfileCoverage
	RequiredTags                                      []core.Tag
	Issues                                            []ProfileIssue
	SRVerificationCandidate                           bool
}

type profileEvaluation struct {
	ctx      context.Context
	opts     ProfileOptions
	report   ProfileReport
	required map[core.Tag]bool
	issues   map[ProfileIssue]bool
	limitErr error
}

func newProfileEvaluation(ctx context.Context, profile Profile, opts ProfileOptions) (*profileEvaluation, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	e := &profileEvaluation{ctx: ctx, opts: opts, required: map[core.Tag]bool{}, issues: map[ProfileIssue]bool{}, report: ProfileReport{
		Profile: profile, StandardVersion: ProfileStandardVersion, SourceSHA256: ProfileSourceSHA256,
		ModuleSourceSHA256: ProfileModuleSourceSHA256, Coverage: ProfileCoverageComplete,
	}}
	if opts.MaxElements < 0 || opts.MaxDepth < 0 || opts.MaxIssues < 0 {
		return e, fmt.Errorf("%w: negative bound", ErrProfileLimit)
	}
	if e.opts.MaxElements == 0 {
		e.opts.MaxElements = 100000
	}
	if e.opts.MaxDepth == 0 {
		e.opts.MaxDepth = 64
	}
	if e.opts.MaxIssues == 0 {
		e.opts.MaxIssues = 256
	}
	// The recursion bound is never caller-expandable: synthetic cyclic graphs
	// must terminate before helpers written for parsed acyclic DICOM datasets.
	if e.opts.MaxDepth > 64 {
		return e, fmt.Errorf("%w: depth exceeds 64", ErrProfileLimit)
	}
	if len(opts.HumanVerifiedPaths) > 256 {
		return e, fmt.Errorf("%w: human path count exceeds 256", ErrProfileLimit)
	}
	return e, ctx.Err()
}

func (e *profileEvaluation) issue(code string, tag core.Tag, path string) {
	issue := ProfileIssue{Code: code, Tag: tag, Path: path}
	if e.issues[issue] {
		return
	}
	if len(e.issues) >= e.opts.MaxIssues {
		e.limitErr = ErrProfileLimit
		e.report.Coverage = ProfileCoveragePartial
		return
	}
	e.issues[issue] = true
}

func (e *profileEvaluation) finish(err error) (ProfileReport, error) {
	for tag := range e.required {
		e.report.RequiredTags = append(e.report.RequiredTags, tag)
	}
	sort.Slice(e.report.RequiredTags, func(i, j int) bool { return tagLess(e.report.RequiredTags[i], e.report.RequiredTags[j]) })
	for issue := range e.issues {
		e.report.Issues = append(e.report.Issues, issue)
	}
	sort.Slice(e.report.Issues, func(i, j int) bool {
		a, b := e.report.Issues[i], e.report.Issues[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Tag != b.Tag {
			return tagLess(a.Tag, b.Tag)
		}
		return a.Code < b.Code
	})
	if err != nil || e.limitErr != nil {
		e.report.Coverage = ProfileCoveragePartial
	}
	if len(e.report.Issues) > 0 {
		err = errors.Join(err, fmt.Errorf("%w (%s): %d attribute-profile issue(s)", ErrProfileViolation, e.report.Profile, len(e.report.Issues)))
	}
	if e.report.Coverage == ProfileCoverageNeedsContext {
		err = errors.Join(err, ErrProfileIncomplete)
	}
	return e.report, errors.Join(err, e.limitErr)
}

// boundedProfileWalk counts elements AND items (including empty ones), visits
// no bulk payload bytes and periodically observes cancellation. It must run
// before recursive exclusion/path/candidate helpers.
func (e *profileEvaluation) boundedWalk(elements []core.Element, visit func(core.Element, string)) error {
	count := 0
	var walk func([]core.Element, string, int) error
	walk = func(elements []core.Element, prefix string, depth int) error {
		if depth > e.opts.MaxDepth {
			return ErrProfileLimit
		}
		for _, el := range elements {
			count++
			if count > e.opts.MaxElements {
				return ErrProfileLimit
			}
			if count&255 == 0 {
				if err := e.ctx.Err(); err != nil {
					return err
				}
			}
			path := prefix + el.Tag().String()
			if visit != nil {
				visit(el, path)
			}
			if seq, ok := el.Value.(core.SequenceValue); ok {
				for i, item := range seq.Items {
					count++
					if count > e.opts.MaxElements {
						return ErrProfileLimit
					}
					if err := e.ctx.Err(); err != nil {
						return err
					}
					if err := walk(item.Elements, fmt.Sprintf("%s[%d].", path, i), depth+1); err != nil {
						return err
					}
				}
			}
		}
		return e.ctx.Err()
	}
	return walk(elements, "", 0)
}
