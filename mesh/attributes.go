package mesh

import (
	"fmt"
	"math"
	"strings"
)

const (
	// MaxVertexScalarAttributes bounds extra scalar properties per mesh/part.
	MaxVertexScalarAttributes = 32
	// MaxGLBScalarAttributes bounds extra scalar metadata across a GLB call.
	MaxGLBScalarAttributes = 4096
)

// VertexScalarAttribute is one finite scalar per vertex. Writers borrow
// Values only for the call; callers must keep the slice stable. Name is an
// ASCII identifier starting with a letter, at most 64 bytes, and must not
// collide with x/y/z/nx/ny/nz. PLY writes a double property named Name; GLB
// writes a FLOAT/SCALAR attribute named "_"+Name, rejecting float32 overflow.
// Units is an optional ASCII token (letters/digits and ^*/._-) of at most
// 64 bytes. The stored scalars are never rescaled by a GLB node transform.
type VertexScalarAttribute struct {
	Name   string
	Values []float64
	Units  string
}

func validateVertexScalars(attributes []VertexScalarAttribute, vertices int, float32Output bool) error {
	if len(attributes) > MaxVertexScalarAttributes {
		return fmt.Errorf("%w: %d vertex scalar attributes exceed %d", ErrInvalidField, len(attributes), MaxVertexScalarAttributes)
	}
	if len(attributes) != 0 && (vertices < 0 || uint64(vertices) >= math.MaxUint32) {
		return fmt.Errorf("%w: vertex scalar index capacity", ErrInvalidField)
	}
	for i, attribute := range attributes {
		if !scalarName(attribute.Name) {
			return fmt.Errorf("%w: invalid vertex scalar name %q", ErrInvalidField, attribute.Name)
		}
		switch strings.ToLower(attribute.Name) {
		case "x", "y", "z", "nx", "ny", "nz":
			return fmt.Errorf("%w: reserved vertex scalar name %q", ErrInvalidField, attribute.Name)
		}
		for _, previous := range attributes[:i] {
			if previous.Name == attribute.Name {
				return fmt.Errorf("%w: duplicate vertex scalar name %q", ErrInvalidField, attribute.Name)
			}
		}
		if !scalarUnits(attribute.Units) {
			return fmt.Errorf("%w: invalid units for vertex scalar %q", ErrInvalidField, attribute.Name)
		}
		if len(attribute.Values) != vertices {
			return fmt.Errorf("%w: %d values for vertex scalar %q of %d vertices", ErrInvalidField, len(attribute.Values), attribute.Name, vertices)
		}
		for vertex, value := range attribute.Values {
			if !finiteCurvature(value) || (float32Output && !finiteCurvature(float64(float32(value)))) {
				return fmt.Errorf("%w: vertex scalar %q value %d is not finite/representable", ErrInvalidField, attribute.Name, vertex)
			}
		}
	}
	return nil
}

func scalarName(name string) bool {
	if len(name) == 0 || len(name) > 64 || !scalarLetter(name[0]) {
		return false
	}
	for i := 1; i < len(name); i++ {
		c := name[i]
		if !scalarLetter(c) && !(c >= '0' && c <= '9') && c != '_' {
			return false
		}
	}
	return true
}

func scalarLetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

func scalarUnits(units string) bool {
	if len(units) > 64 {
		return false
	}
	for i := 0; i < len(units); i++ {
		c := units[i]
		if !scalarLetter(c) && !(c >= '0' && c <= '9') && !strings.ContainsRune("^*/._-", rune(c)) {
			return false
		}
	}
	return true
}
