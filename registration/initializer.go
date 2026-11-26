package registration

import (
	"errors"
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/render"
)

// CenteringMode chooses the centres CenterTransform aligns.
type CenteringMode int

const (
	// CenterGeometry aligns the physical centres of the image grids.
	CenterGeometry CenteringMode = iota
	// CenterMoments aligns the intensity centres of mass.
	CenterMoments
)

// ErrNoMass reports an image whose intensities sum to zero, which has no
// centre of mass.
var ErrNoMass = errors.New("registration: image has no intensity mass")

// centeredTransform is a transform with a centre of rotation and a
// translation, as the matrix transforms are.
type centeredTransform interface {
	Transform
	SetCenter(center render.Vec3) error
	SetTranslation(translation render.Vec3) error
}

// CenterTransform initializes transform as ITK's CenteredTransformInitializer
// does: the centre of rotation becomes the fixed image's centre and the
// translation the displacement to the moving image's centre, so that the
// transform (fixed to moving) maps one centre onto the other. The rotation
// part is left as it is.
func CenterTransform(transform Transform, fixed, moving *Image, mode CenteringMode) error {
	centered, ok := transform.(centeredTransform)
	if !ok {
		return fmt.Errorf("registration: %s has no centre to initialize", transform.Kind())
	}
	fixedCentre, err := imageCentre(fixed, mode)
	if err != nil {
		return err
	}
	movingCentre, err := imageCentre(moving, mode)
	if err != nil {
		return err
	}
	if err := centered.SetCenter(fixedCentre); err != nil {
		return err
	}
	return centered.SetTranslation(movingCentre.Sub(fixedCentre))
}

func imageCentre(im *Image, mode CenteringMode) (render.Vec3, error) {
	if mode == CenterGeometry {
		return im.Point(render.Vec3{X: float64(im.size[0]-1) / 2, Y: float64(im.size[1]-1) / 2, Z: float64(im.size[2]-1) / 2}), nil
	}
	// Centre of mass in index space, then physical.
	var mass, sx, sy, sz float64
	for i, v := range im.data {
		if v == 0 {
			continue
		}
		w := float64(v)
		x, y, z := i%im.size[0], i/im.size[0]%im.size[1], i/(im.size[0]*im.size[1])
		mass += w
		sx += w * float64(x)
		sy += w * float64(y)
		sz += w * float64(z)
	}
	if mass == 0 || math.IsNaN(mass) || math.IsInf(mass, 0) {
		return render.Vec3{}, ErrNoMass
	}
	return im.Point(render.Vec3{X: sx / mass, Y: sy / mass, Z: sz / mass}), nil
}
