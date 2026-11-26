package roi

// Consensus of several readers' segmentations (#1101): label voting,
// multi-label STAPLE and binary STAPLE (Warfield, Zou and Wells,
// "Simultaneous truth and performance level estimation", IEEE TMI 2004).
// The algorithms follow ITK's LabelVotingImageFilter,
// MultiLabelSTAPLEImageFilter and STAPLEImageFilter (ITK 35b196e13c,
// Apache-2.0, Copyright NumFOCUS), including the tie handling fixed by ITK
// issue #6575. Modifications: flat label slices instead of images, context
// cancellation and a voxel budget, a bounded default iteration count, an
// explicit error instead of a wrapped undecided label, and binary STAPLE
// keeps the prior probability where both likelihoods vanish instead of
// dividing zero by zero.

import (
	"context"
	"errors"
	"fmt"
	"math"
)

var (
	// ErrConsensusInput reports raters of different sizes, fewer than two
	// raters, or an empty image.
	ErrConsensusInput = errors.New("roi: invalid consensus input")
	// ErrUndecidedLabel reports that no label is free for undecided voxels.
	ErrUndecidedLabel = errors.New("roi: no label available for undecided voxels")
)

const (
	// DefaultSTAPLEIterations bounds the EM iterations when not set.
	DefaultSTAPLEIterations = 1000
	// STAPLESeedOnly runs no EM iteration: multi-label STAPLE keeps the
	// confusion matrices seeded by voting.
	STAPLESeedOnly = -1
	// DefaultSTAPLETermination is ITK's multi-label termination threshold on
	// the largest confusion-matrix change.
	DefaultSTAPLETermination = 1e-5
	// MaxConsensusVoxels bounds raters × voxels.
	MaxConsensusVoxels = 1 << 31
	// MaxConsensusLabels bounds the label count (max label + 1) of
	// multi-label STAPLE, whose confusion matrices grow with its square.
	MaxConsensusLabels = 1024
)

func checkRaters[T any](raters [][]T) (int, error) {
	if len(raters) < 2 {
		return 0, fmt.Errorf("%w: %d raters, want at least 2", ErrConsensusInput, len(raters))
	}
	n := len(raters[0])
	if n == 0 {
		return 0, fmt.Errorf("%w: empty image", ErrConsensusInput)
	}
	for i, r := range raters {
		if len(r) != n {
			return 0, fmt.Errorf("%w: rater %d has %d voxels, want %d", ErrConsensusInput, i, len(r), n)
		}
	}
	if uint64(n)*uint64(len(raters)) > MaxConsensusVoxels {
		return 0, fmt.Errorf("%w: %d raters × %d voxels exceed the budget", ErrConsensusInput, len(raters), n)
	}
	return n, nil
}

func maxLabel(raters [][]uint16) int {
	m := 0
	for _, r := range raters {
		for _, v := range r {
			m = max(m, int(v))
		}
	}
	return m
}

// LabelVoting returns, per voxel, the label most raters chose, or undecided
// when two or more labels tie for the most votes. A negative undecided
// selects max label + 1.
func LabelVoting(raters [][]uint16, undecided int) ([]int, error) {
	n, err := checkRaters(raters)
	if err != nil {
		return nil, err
	}
	total := maxLabel(raters) + 1
	if undecided < 0 {
		undecided = total
	}
	votes := make([]int, total)
	out := make([]int, n)
	for voxel := range out {
		clear(votes)
		for _, r := range raters {
			votes[r[voxel]]++
		}
		winner, best := 0, votes[0]
		for label := 1; label < total; label++ {
			switch {
			case votes[label] > best:
				winner, best = label, votes[label]
			case votes[label] == best:
				winner = undecided
			}
		}
		out[voxel] = winner
	}
	return out, nil
}

// MultiLabelSTAPLEOptions configures MultiLabelSTAPLE.
type MultiLabelSTAPLEOptions struct {
	// MaxIterations bounds the EM iterations: 0 selects
	// DefaultSTAPLEIterations, STAPLESeedOnly runs none.
	MaxIterations int
	// Termination stops when no confusion-matrix entry changes by more; 0
	// selects DefaultSTAPLETermination.
	Termination float64
	// UndecidedLabel labels voxels whose most probable labels tie. The
	// default (HasUndecidedLabel false) is max label + 1.
	UndecidedLabel    int
	HasUndecidedLabel bool
	// Priors are the prior probabilities of labels 0..max; the default is
	// each label's share of all raters' voxels.
	Priors []float64
}

// MultiLabelSTAPLEResult is the estimated consensus and each rater's
// performance.
type MultiLabelSTAPLEResult struct {
	// Labels is the consensus label of every voxel, or UndecidedLabel.
	Labels         []int
	UndecidedLabel int
	// ConfusionMatrices[k][j][c] is the estimated probability that rater k
	// labels j a voxel whose true label is c. Rows run over 0..max+1 (the
	// last row, ITK's reject row, stays zero); columns over 0..max.
	ConfusionMatrices [][][]float64
	Priors            []float64
	// Iterations is ITK's ElapsedNumberOfIterations: the EM iterations
	// completed before the one that met the termination threshold.
	Iterations int
	Converged  bool
}

// MultiLabelSTAPLE estimates the consensus of label maps by multi-label
// STAPLE, seeded with label voting.
func MultiLabelSTAPLE(ctx context.Context, raters [][]uint16, options MultiLabelSTAPLEOptions) (MultiLabelSTAPLEResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	n, err := checkRaters(raters)
	if err != nil {
		return MultiLabelSTAPLEResult{}, err
	}
	total := maxLabel(raters) + 1
	if total > MaxConsensusLabels {
		return MultiLabelSTAPLEResult{}, fmt.Errorf("%w: %d labels exceed %d", ErrConsensusInput, total, MaxConsensusLabels)
	}
	result := MultiLabelSTAPLEResult{UndecidedLabel: total}
	if options.HasUndecidedLabel {
		result.UndecidedLabel = options.UndecidedLabel
	} else if total > math.MaxUint16 {
		return MultiLabelSTAPLEResult{}, ErrUndecidedLabel
	}
	maxIterations := options.MaxIterations
	switch {
	case maxIterations == 0:
		maxIterations = DefaultSTAPLEIterations
	case maxIterations < 0:
		maxIterations = 0
	}
	termination := options.Termination
	if termination <= 0 {
		termination = DefaultSTAPLETermination
	}
	// Seed the confusion matrices from voting: counts of each rater label
	// against the decided voting label, normalized per true-label column.
	voting, err := LabelVoting(raters, total)
	if err != nil {
		return MultiLabelSTAPLEResult{}, err
	}
	matrices := make([][][]float64, len(raters))
	updated := make([][][]float64, len(raters))
	for k, rater := range raters {
		matrices[k], updated[k] = newMatrix(total+1, total), newMatrix(total+1, total)
		for voxel, vote := range voting {
			if vote != total {
				matrices[k][rater[voxel]][vote]++
			}
		}
		for c := 0; c < total; c++ {
			sum := 0.0
			for j := range matrices[k] {
				sum += matrices[k][j][c]
			}
			if sum > 0 {
				for j := range matrices[k] {
					matrices[k][j][c] /= sum
				}
			}
		}
	}
	priors := options.Priors
	if priors == nil {
		counts := make([]float64, total)
		for _, rater := range raters {
			for _, v := range rater {
				counts[v]++
			}
		}
		priors = make([]float64, total)
		for label, count := range counts {
			priors[label] = count / float64(n*len(raters))
		}
	} else if len(priors) < total {
		return MultiLabelSTAPLEResult{}, fmt.Errorf("%w: %d priors for %d labels", ErrConsensusInput, len(priors), total)
	}
	result.Priors = append([]float64(nil), priors[:total]...)
	weights := make([]float64, total)
	estep := func(voxel int) {
		copy(weights, priors[:total])
		for k, rater := range raters {
			row := matrices[k][rater[voxel]]
			for c := range weights {
				weights[c] *= row[c]
			}
		}
	}
	iteration := 0
	for ; iteration < maxIterations; iteration++ {
		for k := range updated {
			for _, row := range updated[k] {
				clear(row)
			}
		}
		for voxel := 0; voxel < n; voxel++ {
			if voxel&0xffff == 0 {
				if err := ctx.Err(); err != nil {
					return MultiLabelSTAPLEResult{}, err
				}
			}
			estep(voxel)
			sum := 0.0
			for _, w := range weights {
				sum += w
			}
			if sum != 0 {
				for c := range weights {
					weights[c] /= sum
				}
			}
			for k, rater := range raters {
				row := updated[k][rater[voxel]]
				for c, w := range weights {
					row[c] += w
				}
			}
		}
		maximumUpdate := 0.0
		for k := range updated {
			for c := 0; c < total; c++ {
				sum := 0.0
				for j := range updated[k] {
					sum += updated[k][j][c]
				}
				if sum != 0 {
					for j := range updated[k] {
						updated[k][j][c] /= sum
					}
				}
			}
			for j := range updated[k] {
				for c := range updated[k][j] {
					maximumUpdate = max(maximumUpdate, math.Abs(updated[k][j][c]-matrices[k][j][c]))
					matrices[k][j][c] = updated[k][j][c]
				}
			}
		}
		if maximumUpdate < termination {
			result.Converged = true
			break
		}
	}
	result.Iterations = iteration
	result.Labels = make([]int, n)
	for voxel := range result.Labels {
		estep(voxel)
		winner, best := result.UndecidedLabel, 0.0
		for c, w := range weights {
			if w > best {
				winner, best = c, w
			} else if !(w < best) {
				winner = result.UndecidedLabel
			}
		}
		result.Labels[voxel] = winner
	}
	result.ConfusionMatrices = matrices
	return result, nil
}

func newMatrix(rows, columns int) [][]float64 {
	m := make([][]float64, rows)
	backing := make([]float64, rows*columns)
	for i := range m {
		m[i] = backing[i*columns : (i+1)*columns]
	}
	return m
}

// BinarySTAPLEOptions configures BinarySTAPLE.
type BinarySTAPLEOptions struct {
	// ConfidenceWeight scales the prior foreground probability (the mean
	// rater foreground fraction); 0 selects 1.
	ConfidenceWeight float64
	// MaxIterations bounds the EM iterations; 0 selects
	// DefaultSTAPLEIterations.
	MaxIterations int
}

// BinarySTAPLEResult is the estimated true segmentation and each rater's
// performance.
type BinarySTAPLEResult struct {
	// Probability is, per voxel, the probability that it truly belongs to
	// the structure; threshold it at 0.5 for a consensus mask.
	Probability []float64
	// Sensitivity[k] and Specificity[k] are rater k's estimated true
	// positive and true negative rates.
	Sensitivity, Specificity []float64
	// Prior is the prior foreground probability used.
	Prior      float64
	Iterations int
	Converged  bool
}

// Mask thresholds the probability map: the voxels with probability at least
// threshold.
func (r BinarySTAPLEResult) Mask(threshold float64) []bool {
	out := make([]bool, len(r.Probability))
	for i, p := range r.Probability {
		out[i] = p >= threshold
	}
	return out
}

// BinarySTAPLE estimates the true segmentation of binary masks and each
// rater's sensitivity and specificity.
func BinarySTAPLE(ctx context.Context, raters [][]bool, options BinarySTAPLEOptions) (BinarySTAPLEResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	n, err := checkRaters(raters)
	if err != nil {
		return BinarySTAPLEResult{}, err
	}
	weight := options.ConfidenceWeight
	if weight == 0 {
		weight = 1
	}
	maxIterations := options.MaxIterations
	if maxIterations <= 0 {
		maxIterations = DefaultSTAPLEIterations
	}
	const minSquaredChange = 1e-14
	k := len(raters)
	w := make([]float64, n)
	prior := 0.0
	for voxel := range w {
		for _, r := range raters {
			if r[voxel] {
				w[voxel]++
			}
		}
		w[voxel] /= float64(k)
		prior += w[voxel]
	}
	prior = prior / float64(n) * weight
	p, q := make([]float64, k), make([]float64, k)
	lastP, lastQ := make([]float64, k), make([]float64, k)
	for i := range p {
		p[i], q[i], lastP[i], lastQ[i] = 1, 1, -10, -10
	}
	result := BinarySTAPLEResult{Prior: prior}
	iteration := 0
	for ; iteration < maxIterations; iteration++ {
		if err := ctx.Err(); err != nil {
			return BinarySTAPLEResult{}, err
		}
		for i, r := range raters {
			var pNum, pDen, qNum, qDen float64
			for voxel, v := range r {
				if v {
					pNum += w[voxel]
				} else {
					qNum += 1 - w[voxel]
				}
				pDen += w[voxel]
				qDen += 1 - w[voxel]
			}
			if pDen != 0 {
				p[i] = pNum / pDen
			}
			if qDen != 0 {
				q[i] = qNum / qDen
			}
		}
		for voxel := range w {
			alpha, beta := 1.0, 1.0
			for i, r := range raters {
				if r[voxel] {
					alpha *= p[i]
					beta *= 1 - q[i]
				} else {
					alpha *= 1 - p[i]
					beta *= q[i]
				}
			}
			if denominator := prior*alpha + (1-prior)*beta; denominator > 0 {
				w[voxel] = prior * alpha / denominator
			} else {
				w[voxel] = prior
			}
		}
		converged := iteration != 0
		for i := range p {
			if (p[i]-lastP[i])*(p[i]-lastP[i]) > minSquaredChange || (q[i]-lastQ[i])*(q[i]-lastQ[i]) > minSquaredChange {
				converged = false
			}
			lastP[i], lastQ[i] = p[i], q[i]
		}
		if converged {
			result.Converged = true
			iteration++
			break
		}
	}
	result.Iterations = iteration
	result.Probability, result.Sensitivity, result.Specificity = w, p, q
	return result, nil
}
