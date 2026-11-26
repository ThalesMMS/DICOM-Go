package spatialreg

import (
	"errors"
	"fmt"
	"strings"

	"github.com/ThalesMMS/dicom-go/render"
)

// Chained registrations (#1092): a mapping between two Frames of Reference
// that no single registration links, composed explicitly from registrations
// through intermediate Frames of Reference, as ITK's CompositeTransform
// composes transforms. Affine registrations are usable in both directions;
// deformable ones only Registered-to-Source, as stored, because a chain
// would otherwise compound the per-point inverse's residual.

// TransformDeformable names a deformable hop in a chain's type.
const TransformDeformable TransformType = "DEFORMABLE"

// DefaultMaxChainHops is the longest chain FindChains builds by default.
const DefaultMaxChainHops = 2

// ErrChain reports a chain that cannot map a point.
var ErrChain = errors.New("dicom/spatialreg: chained registration")

// Hop is one direct registration used from one Frame of Reference to
// another: a point in From maps to To.
type Hop struct {
	SOPInstanceUID string
	From, To       string
	Type           TransformType
	// SourceToRegistered is true when a registration is used from its
	// source to its registered Frame of Reference.
	SourceToRegistered bool

	affine     *AffineRegistration
	deformable *Registration
}

// Map maps a patient point from the hop's From Frame of Reference to To.
func (h Hop) Map(point render.Vec3) (render.Vec3, error) {
	switch {
	case h.affine != nil && h.SourceToRegistered:
		return h.affine.MapSourceToRegistered(point)
	case h.affine != nil:
		return h.affine.MapRegisteredToSource(point)
	case h.deformable != nil && h.SourceToRegistered:
		return h.deformable.MapSourceToRegistered(point)
	case h.deformable != nil:
		return h.deformable.MapRegisteredToSource(point)
	}
	return render.Vec3{}, fmt.Errorf("%w: empty hop", ErrInvalidObject)
}

// Matrix is the hop's From-to-To matrix; ok is false for a deformable hop.
func (h Hop) Matrix() (matrix render.GeometryAffine, ok bool) {
	switch {
	case h.affine != nil && h.SourceToRegistered:
		return h.affine.SourceToRegistered, true
	case h.affine != nil:
		return h.affine.RegisteredToSource, true
	}
	return render.GeometryAffine{}, false
}

// Hops lists the direct hops of validated registrations: every affine item
// in both directions and every deformable item Registered-to-Source. Items
// without a source Frame of Reference, and identity items naming the
// registered Frame of Reference itself, are skipped.
func Hops(affine []*AffineObject, deformable []*Object) []Hop {
	var hops []Hop
	for _, object := range affine {
		if object != nil {
			hops = appendAffineHops(hops, object, object.SOPInstanceUID)
		}
	}
	for _, object := range deformable {
		if object != nil {
			hops = appendDeformableHops(hops, object, object.SOPInstanceUID)
		}
	}
	return hops
}

func appendAffineHops(hops []Hop, object *AffineObject, sopUID string) []Hop {
	registered := strings.TrimSpace(object.RegisteredFrameOfReferenceUID)
	for index := range object.Registrations {
		item := &object.Registrations[index]
		source := strings.TrimSpace(item.SourceFrameOfReferenceUID)
		if source == "" || registered == "" || source == registered {
			continue
		}
		hops = append(hops,
			Hop{SOPInstanceUID: sopUID, From: registered, To: source, Type: item.Type, affine: item},
			Hop{SOPInstanceUID: sopUID, From: source, To: registered, Type: item.Type, SourceToRegistered: true, affine: item})
	}
	return hops
}

func appendDeformableHops(hops []Hop, object *Object, sopUID string) []Hop {
	registered := strings.TrimSpace(object.RegisteredFrameOfReferenceUID)
	for index := range object.Registrations {
		item := &object.Registrations[index]
		source := strings.TrimSpace(item.SourceFrameOfReferenceUID)
		if source == "" || registered == "" || source == registered {
			continue
		}
		hops = append(hops, Hop{SOPInstanceUID: sopUID, From: registered, To: source, Type: TransformDeformable, deformable: item})
	}
	return hops
}

// Chain is an explicit composition of hops, applied in order: the To of
// each hop is the From of the next.
type Chain struct {
	Hops []Hop
}

// NewChain checks that hops connect and visit no Frame of Reference twice.
func NewChain(hops ...Hop) (Chain, error) {
	if len(hops) == 0 {
		return Chain{}, fmt.Errorf("%w: no hops", ErrChain)
	}
	visited := map[string]bool{hops[0].From: true}
	for index, hop := range hops {
		if hop.affine == nil && hop.deformable == nil {
			return Chain{}, fmt.Errorf("%w: hop %d is empty", ErrChain, index+1)
		}
		if index > 0 && hops[index-1].To != hop.From {
			return Chain{}, fmt.Errorf("%w: hop %d starts in %s, not in %s", ErrChain, index+1, hop.From, hops[index-1].To)
		}
		if visited[hop.To] {
			return Chain{}, fmt.Errorf("%w: Frame of Reference %s is visited twice", ErrChain, hop.To)
		}
		visited[hop.To] = true
	}
	return Chain{Hops: append([]Hop(nil), hops...)}, nil
}

// From and To are the Frames of Reference the chain links.
func (c Chain) From() string {
	if len(c.Hops) == 0 {
		return ""
	}
	return c.Hops[0].From
}

func (c Chain) To() string {
	if len(c.Hops) == 0 {
		return ""
	}
	return c.Hops[len(c.Hops)-1].To
}

// Map maps a patient point through every hop in order. A hop's error is
// returned wrapped, so errors.Is still finds ErrOutsideDomain and the like.
func (c Chain) Map(point render.Vec3) (render.Vec3, error) {
	if len(c.Hops) == 0 {
		return render.Vec3{}, fmt.Errorf("%w: no hops", ErrChain)
	}
	for index, hop := range c.Hops {
		mapped, err := hop.Map(point)
		if err != nil {
			return render.Vec3{}, fmt.Errorf("%w: hop %d (%s, SOP %s): %w", ErrChain, index+1, hop.Type, hop.SOPInstanceUID, err)
		}
		point = mapped
	}
	return point, nil
}

// Matrix is the product of the hops' matrices, the first hop applied first;
// ok is false when a hop is deformable.
func (c Chain) Matrix() (matrix render.GeometryAffine, ok bool) {
	matrix = identityAffine()
	for _, hop := range c.Hops {
		next, isAffine := hop.Matrix()
		if !isAffine {
			return render.GeometryAffine{}, false
		}
		matrix = multiplyAffine(next, matrix)
	}
	return matrix, len(c.Hops) > 0
}

// Type is the type of the composed mapping: DEFORMABLE when a hop is
// deformable, otherwise the most constrained matrix type the product
// satisfies (RIGID, RIGID_SCALE or AFFINE).
func (c Chain) Type() TransformType {
	matrix, ok := c.Matrix()
	if !ok {
		return TransformDeformable
	}
	for _, kind := range []TransformType{TransformRigid, TransformRigidScale} {
		if validateMatrixType(matrix, kind) == nil {
			return kind
		}
	}
	return TransformAffine
}

// String describes the hop, for example "RIGID inverse (SOP 1.2.3)" for a
// registration used from its source Frame of Reference.
func (h Hop) String() string {
	direction := ""
	if h.SourceToRegistered {
		direction = " inverse"
	}
	return fmt.Sprintf("%s%s (SOP %s)", h.Type, direction, h.SOPInstanceUID)
}

// SOPInstanceUIDs are the registrations used, in hop order.
func (c Chain) SOPInstanceUIDs() []string {
	uids := make([]string, len(c.Hops))
	for index, hop := range c.Hops {
		uids[index] = hop.SOPInstanceUID
	}
	return uids
}

// String describes the chain, for example
// "RIGID (SOP 1.2.3) then AFFINE (SOP 1.2.4)".
func (c Chain) String() string {
	parts := make([]string, len(c.Hops))
	for index, hop := range c.Hops {
		parts[index] = hop.String()
	}
	return strings.Join(parts, " then ")
}

// FindChains returns every chain of at most maxHops hops (DefaultMaxChainHops
// when maxHops ≤ 0) from Frame of Reference from to to that visits no Frame
// of Reference twice, shortest first. Several results mean the mapping is
// ambiguous; callers should not pick one silently.
func FindChains(hops []Hop, from, to string, maxHops int) []Chain {
	from, to = strings.TrimSpace(from), strings.TrimSpace(to)
	if maxHops <= 0 {
		maxHops = DefaultMaxChainHops
	}
	if from == "" || to == "" || from == to {
		return nil
	}
	outgoing := make(map[string][]Hop)
	for _, hop := range hops {
		outgoing[hop.From] = append(outgoing[hop.From], hop)
	}
	var found [][]Hop
	var walk func(path []Hop, visited map[string]bool)
	walk = func(path []Hop, visited map[string]bool) {
		at := from
		if len(path) > 0 {
			at = path[len(path)-1].To
		}
		if at == to {
			found = append(found, append([]Hop(nil), path...))
			return
		}
		if len(path) == maxHops {
			return
		}
		for _, hop := range outgoing[at] {
			if visited[hop.To] {
				continue
			}
			visited[hop.To] = true
			walk(append(path, hop), visited)
			delete(visited, hop.To)
		}
	}
	walk(nil, map[string]bool{from: true})
	chains := make([]Chain, 0, len(found))
	for length := 1; length <= maxHops; length++ {
		for _, path := range found {
			if len(path) == length {
				chains = append(chains, Chain{Hops: path})
			}
		}
	}
	return chains
}
