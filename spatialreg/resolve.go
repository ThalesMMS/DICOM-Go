package spatialreg

import (
	"fmt"
	"slices"
	"strings"

	"github.com/ThalesMMS/dicom-go/render"
)

// RegistrationCandidate is one parsed registration object. SOPInstanceUID
// overrides the object's UID for callers with separate instance metadata.
type RegistrationCandidate struct {
	SOPInstanceUID string
	Affine         *AffineObject
	Deformable     *Object
}

// ResolutionReason identifies why two Frames of Reference could not be linked.
type ResolutionReason string

const (
	ResolutionMissingFrame       ResolutionReason = "missing-frame"
	ResolutionCycle              ResolutionReason = "cycle"
	ResolutionAmbiguousDirect    ResolutionReason = "ambiguous-direct"
	ResolutionAmbiguousChains    ResolutionReason = "ambiguous-chains"
	ResolutionMissingSourceFrame ResolutionReason = "missing-source-frame"
	ResolutionNoLink             ResolutionReason = "no-link"
)

// ResolutionError reports an unavailable or ambiguous mapping. Count gives the
// number of alternatives when Reason reports ambiguity.
type ResolutionError struct {
	Reason ResolutionReason
	Count  int
}

func (e *ResolutionError) Error() string {
	if e.Count > 0 {
		return fmt.Sprintf("dicom/spatialreg: %s (%d alternatives)", e.Reason, e.Count)
	}
	return fmt.Sprintf("dicom/spatialreg: %s", e.Reason)
}

// FrameResolution is an identity, direct mapping, or two-hop mapping. Keep
// the underlying parsed registration objects immutable while using it.
type FrameResolution struct {
	sameFrame bool
	chain     Chain
}

// SameFrame reports whether both nonempty Frame of Reference UIDs are equal.
func (r FrameResolution) SameFrame() bool { return r.sameFrame }

// Hops returns the provenance in mapping order. An identity has no hops.
func (r FrameResolution) Hops() []Hop { return slices.Clone(r.chain.Hops) }

// Type describes the selected transform. It is empty for an identity.
func (r FrameResolution) Type() TransformType {
	switch len(r.chain.Hops) {
	case 0:
		return ""
	case 1:
		return r.chain.Hops[0].Type
	default:
		return r.chain.Type()
	}
}

// Matrix returns the From-to-To matrix for an identity or linear mapping.
func (r FrameResolution) Matrix() (render.GeometryAffine, bool) {
	if r.sameFrame {
		return identityAffine(), true
	}
	if len(r.chain.Hops) == 1 {
		return r.chain.Hops[0].Matrix()
	}
	return r.chain.Matrix()
}

// DirectDeformation returns the deformation and whether it is used in the
// inverse direction. The deformation is nil for identity, affine, and chains.
func (r FrameResolution) DirectDeformation() (deformation *Registration, inverse bool) {
	if len(r.chain.Hops) == 1 {
		hop := r.chain.Hops[0]
		if hop.deformable != nil {
			return hop.deformable, hop.SourceToRegistered
		}
	}
	return nil, false
}

// Map maps a patient point from the input Frame of Reference into the output
// Frame of Reference. A domain miss remains available through errors.Is.
func (r FrameResolution) Map(point render.Vec3) (render.Vec3, error) {
	if r.sameFrame {
		if !finiteVec(point) {
			return render.Vec3{}, fmt.Errorf("%w: non-finite patient point", ErrInvalidObject)
		}
		return point, nil
	}
	if len(r.chain.Hops) == 1 {
		return r.chain.Hops[0].Map(point)
	}
	return r.chain.Map(point)
}

// ResolveFrames selects a mapping from one Frame of Reference to another among
// distinct, parsed candidate instances. Cycles in the components of either
// requested frame are refused. A unique direct mapping takes precedence over a
// two-hop chain. Affine hops are allowed in either direction. A deformable
// inverse is allowed only as a direct mapping and is solved per point.
func ResolveFrames(from, to string, candidates []RegistrationCandidate) (FrameResolution, error) {
	from, to = strings.TrimSpace(from), strings.TrimSpace(to)
	if from == "" || to == "" {
		return FrameResolution{}, &ResolutionError{Reason: ResolutionMissingFrame}
	}
	if from == to {
		return FrameResolution{sameFrame: true}, nil
	}

	var hops []Hop
	for _, candidate := range candidates {
		if candidate.Affine != nil {
			hops = appendAffineHops(hops, candidate.Affine, registrationSOPUID(candidate.SOPInstanceUID, candidate.Affine.SOPInstanceUID))
		}
		if candidate.Deformable != nil {
			hops = appendDeformableHops(hops, candidate.Deformable, registrationSOPUID(candidate.SOPInstanceUID, candidate.Deformable.SOPInstanceUID))
		}
	}
	if registrationCycle(hops, from, to) {
		return FrameResolution{}, &ResolutionError{Reason: ResolutionCycle}
	}

	var direct Hop
	directCount := 0
	for _, hop := range hops {
		if hop.deformable != nil && hop.From == to && hop.To == from {
			hop.From, hop.To = hop.To, hop.From
			hop.SourceToRegistered = true
		}
		if hop.From == from && hop.To == to {
			direct = hop
			directCount++
		}
	}
	switch {
	case directCount == 1:
		return FrameResolution{chain: Chain{Hops: []Hop{direct}}}, nil
	case directCount > 1:
		return FrameResolution{}, &ResolutionError{Reason: ResolutionAmbiguousDirect, Count: directCount}
	}

	switch chains := FindChains(hops, from, to, DefaultMaxChainHops); {
	case len(chains) == 1:
		return FrameResolution{chain: chains[0]}, nil
	case len(chains) > 1:
		return FrameResolution{}, &ResolutionError{Reason: ResolutionAmbiguousChains, Count: len(chains)}
	}

	if missingSourceFrame(candidates) {
		return FrameResolution{}, &ResolutionError{Reason: ResolutionMissingSourceFrame}
	}
	return FrameResolution{}, &ResolutionError{Reason: ResolutionNoLink}
}

func registrationSOPUID(override, parsed string) string {
	if override != "" {
		return override
	}
	return parsed
}

func missingSourceFrame(candidates []RegistrationCandidate) bool {
	for _, candidate := range candidates {
		if candidate.Affine != nil {
			for _, item := range candidate.Affine.Registrations {
				if strings.TrimSpace(item.SourceFrameOfReferenceUID) == "" {
					return true
				}
			}
		}
		if candidate.Deformable != nil {
			for _, item := range candidate.Deformable.Registrations {
				if strings.TrimSpace(item.SourceFrameOfReferenceUID) == "" {
					return true
				}
			}
		}
	}
	return false
}

func registrationCycle(hops []Hop, from, to string) bool {
	directed := make(map[string][]string)
	neighbors := make(map[string][]string)
	for _, hop := range hops {
		// Check the direction in the DICOM objects, ignoring the synthetic
		// reciprocal affine edge generated in appendAffineHops.
		if hop.SourceToRegistered {
			continue
		}
		source, registered := hop.To, hop.From
		directed[source] = append(directed[source], registered)
		neighbors[source] = append(neighbors[source], registered)
		neighbors[registered] = append(neighbors[registered], source)
	}

	reached := map[string]bool{from: true, to: true}
	queue := []string{from, to}
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		for _, neighbor := range neighbors[node] {
			if !reached[neighbor] {
				reached[neighbor] = true
				queue = append(queue, neighbor)
			}
		}
	}

	const (
		active = iota + 1
		checked
	)
	state := make(map[string]uint8)
	var visit func(string) bool
	visit = func(node string) bool {
		state[node] = active
		for _, next := range directed[node] {
			if state[next] == active || (state[next] == 0 && visit(next)) {
				return true
			}
		}
		state[node] = checked
		return false
	}
	for node := range reached {
		if state[node] == 0 && visit(node) {
			return true
		}
	}
	return false
}
