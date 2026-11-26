package registration

import (
	"context"
	"fmt"

	"github.com/ThalesMMS/dicom-go/optimize"
)

// LevelMinimizer is Method.Minimize's existing replacement-optimizer hook.
type LevelMinimizer func(int, optimize.GradientFunction, []float64, []float64) (optimize.Result, error)

// CoarseSearchOptions supply starts in the SAME transform family, fixed
// parameters, center and Euler order as Method.Transform. Nil Starts uses its
// actual starting parameters. Grid, if set, searches around each start only on
// level zero. Its StepSizes are actual parameter units (Euler angles:radians,
// translations:mm). Limits also bound all local refinements at that level.
type CoarseSearchOptions struct {
	Starts [][]float64
	Grid   *optimize.Exhaustive
	Limits optimize.SearchLimits
}

// CoarseSearchMinimizer snapshots options and returns a serial coarse-to-fine
// hook. Level zero performs multi-start/grid search plus optional refinement;
// later levels refine only the preceding result. refine nil only evaluates the
// preceding result on later levels. This helper does not alter Method defaults,
// create a second metric/image pyramid, or imply success for clinical images.
// Supplied starts must use an explicit common transform center/order.
func CoarseSearchMinimizer(ctx context.Context, opts CoarseSearchOptions, refine LevelMinimizer) (LevelMinimizer, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Hook construction also has a finite admission before snapshotting arrays.
	maxStarts := opts.Limits.MaxStarts
	if maxStarts == 0 {
		maxStarts = 4096
	}
	maxParams := opts.Limits.MaxParameters
	if maxParams == 0 {
		maxParams = 4096
	}
	maxBytes := opts.Limits.MaxWorkspaceBytes
	if maxBytes == 0 {
		maxBytes = 64 << 20
	}
	if maxStarts < 0 || maxParams < 0 || maxBytes < 0 || opts.Limits.MaxEvaluations < 0 {
		return nil, optimize.ErrOptions
	}
	if maxBytes < 8192 || len(opts.Starts) > maxStarts {
		return nil, optimize.ErrSearchBudget
	}
	var elements int64
	n := 0
	for _, x := range opts.Starts {
		if n == 0 {
			n = len(x)
		}
		if len(x) == 0 || len(x) != n || len(x) > maxParams || !finiteSlice(x) {
			return nil, fmt.Errorf("%w: coarse starts", optimize.ErrOptions)
		}
		if elements > (maxBytes-8192)/8-int64(len(x)) {
			return nil, optimize.ErrSearchBudget
		}
		elements += int64(len(x))
	}
	if int64(len(opts.Starts)) > (maxBytes-8192-8*elements)/24 {
		return nil, optimize.ErrSearchBudget
	}
	// Reserve both the immutable hook snapshot and simultaneous search stages
	// before copying either starts or grid options. A nested grid shares the
	// remaining workspace conservatively with MultiStart; local optimizer
	// scratch remains the caller's separately bounded responsibility.
	hookBytes := int64(8192) + 8*elements + 24*int64(len(opts.Starts))
	if opts.Grid != nil {
		if opts.Grid.Limits.MaxParameters < 0 || opts.Grid.Limits.MaxStarts < 0 || opts.Grid.Limits.MaxEvaluations < 0 || opts.Grid.Limits.MaxWorkspaceBytes < 0 {
			return nil, optimize.ErrOptions
		}
		if len(opts.Grid.Steps) > maxParams || len(opts.Grid.StepSizes) > maxParams || int64(len(opts.Grid.Steps)) > (maxBytes-hookBytes)/8 || int64(len(opts.Grid.StepSizes)) > (maxBytes-hookBytes)/8-int64(len(opts.Grid.Steps)) {
			return nil, optimize.ErrSearchBudget
		}
		hookBytes += 8 * (int64(len(opts.Grid.Steps)) + int64(len(opts.Grid.StepSizes)))
	}
	searchLimits := opts.Limits
	searchLimits.MaxWorkspaceBytes = maxBytes - hookBytes
	if opts.Grid != nil {
		searchLimits.MaxWorkspaceBytes /= 2
	}
	if searchLimits.MaxWorkspaceBytes < 8192 {
		return nil, optimize.ErrSearchBudget
	}
	starts := make([][]float64, len(opts.Starts))
	for i, x := range opts.Starts {
		starts[i] = make([]float64, len(x))
		copy(starts[i], x)
	}
	var grid *optimize.Exhaustive
	if opts.Grid != nil {
		g := *opts.Grid
		if g.Limits.MaxWorkspaceBytes < 0 {
			return nil, optimize.ErrOptions
		}
		if g.Limits.MaxWorkspaceBytes == 0 || g.Limits.MaxWorkspaceBytes > searchLimits.MaxWorkspaceBytes {
			g.Limits.MaxWorkspaceBytes = searchLimits.MaxWorkspaceBytes
		}
		g.Steps = make([]int, len(opts.Grid.Steps))
		copy(g.Steps, opts.Grid.Steps)
		g.StepSizes = make([]float64, len(opts.Grid.StepSizes))
		copy(g.StepSizes, opts.Grid.StepSizes)
		grid = &g
	}
	return func(level int, objective optimize.GradientFunction, start, scales []float64) (optimize.Result, error) {
		if err := ctx.Err(); err != nil {
			return optimize.Result{}, err
		}
		if level < 0 {
			return optimize.Result{}, optimize.ErrOptions
		}
		if level != 0 && refine != nil {
			run, err := refine(level, objective, start, scales)
			if ctxErr := ctx.Err(); ctxErr != nil {
				return optimize.Result{}, ctxErr
			}
			return run, err
		}
		points := starts
		if level != 0 || len(points) == 0 {
			points = [][]float64{start}
		}
		multi := optimize.MultiStart{Starts: points, Limits: searchLimits}
		if level == 0 && (grid != nil || refine != nil) {
			multi.Local = func(localCtx context.Context, f optimize.Function, x []float64) (optimize.Result, error) {
				run := optimize.Result{X: x}
				var err error
				if grid != nil {
					run, err = grid.MinimizeContext(localCtx, f, x)
					if err != nil {
						return optimize.Result{}, err
					}
				}
				if refine != nil {
					return refine(level, f.(optimize.GradientFunction), run.X, scales)
				}
				return run, nil
			}
		}
		return multi.MinimizeContext(ctx, objective)
	}, nil
}
