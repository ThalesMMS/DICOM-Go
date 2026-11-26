package render

import (
	"math"
	"testing"
)

func angleBetween(a, b Vec3) float64 {
	a, b = a.Normalize(), b.Normalize()
	return math.Atan2(a.Cross(b).Length(), a.Dot(b))
}

// TestVRCameraBasisMatchesTurntableForStoredOrientations guards existing VPS
// files: every yaw/pitch/roll they can hold opens with the frame of the
// former turntable camera.
func TestVRCameraBasisMatchesTurntableForStoredOrientations(t *testing.T) {
	for _, yaw := range []float64{-3, -1.2, 0, 0.4, 2.5} {
		for _, pitch := range []float64{-1.35, -0.7, 0, 0.3, 1.35, 1.5} {
			for _, roll := range []float64{0, 0.6, -2} {
				c := VRCamera{Yaw: yaw, Pitch: pitch, Roll: roll, Distance: 100}
				right, up, forward := c.basis()
				oldRight := forward.Cross(worldUp).Normalize().Rotate(forward, roll)
				oldUp := forward.Cross(worldUp).Normalize().Cross(forward).Normalize().Rotate(forward, roll)
				if angleBetween(right, oldRight) > 1e-9 || angleBetween(up, oldUp) > 1e-9 {
					t.Fatalf("yaw %v pitch %v roll %v: frame differs from the turntable camera", yaw, pitch, roll)
				}
				var back VRCamera
				back.SetBasis(right, up, forward)
				r2, u2, f2 := back.basis()
				if angleBetween(r2, right) > 1e-9 || angleBetween(u2, up) > 1e-9 || angleBetween(f2, forward) > 1e-9 {
					t.Fatalf("SetBasis(basis()) changed the frame at yaw %v pitch %v roll %v", yaw, pitch, roll)
				}
			}
		}
	}
}

// TestVRCameraRotatesContinuouslyThroughThePoles drags vertically through
// the cranial and caudal views and all the way around.
func TestVRCameraRotatesContinuouslyThroughThePoles(t *testing.T) {
	// A rolled view turns about a tilted screen axis and does not cross the
	// poles; it must still be continuous and return after a full turn.
	for _, start := range []VRCamera{{Yaw: 0.4, Pitch: 0.3, Distance: 100}, {Yaw: -2, Pitch: -0.2, Distance: 100}, {Yaw: 1, Pitch: 0.1, Roll: 0.5, Distance: 100}} {
		c := start
		r0, u0, f0 := c.basis()
		const steps = 400
		step := 2 * math.Pi / 0.008 / steps // one full turn in total
		prevR, prevU, prevF := r0, u0, f0
		maxZ, minZ := -1.0, 1.0
		for i := 0; i < steps; i++ {
			c.Rotate(0, step)
			r, u, f := c.basis()
			for _, pair := range [][2]Vec3{{r, prevR}, {u, prevU}, {f, prevF}} {
				if d := angleBetween(pair[0], pair[1]); d > step*0.008+1e-6 {
					t.Fatalf("step %d jumped by %v rad (step %v)", i, d, step*0.008)
				}
			}
			if angleBetween(r, r0) > 1e-6 {
				t.Fatalf("step %d: a vertical drag changed the screen right axis", i)
			}
			maxZ, minZ = math.Max(maxZ, f.Z), math.Min(minZ, f.Z)
			prevR, prevU, prevF = r, u, f
		}
		if start.Roll == 0 && (maxZ < 0.999 || minZ > -0.999) {
			t.Fatalf("drag did not pass both poles: forward.Z in [%v, %v]", minZ, maxZ)
		}
		r, u, f := c.basis()
		if angleBetween(r, r0) > 1e-6 || angleBetween(u, u0) > 1e-6 || angleBetween(f, f0) > 1e-6 {
			t.Fatal("a full vertical turn did not return to the start")
		}
		if c.Pitch < -math.Pi/2-1e-12 || c.Pitch > math.Pi/2+1e-12 {
			t.Fatalf("pitch %v outside [-π/2, π/2]", c.Pitch)
		}
	}
}

// TestVRCameraRotateMatchesTurntableWhenLevel keeps the drag direction of
// the turntable camera for a level, unrolled view.
func TestVRCameraRotateMatchesTurntableWhenLevel(t *testing.T) {
	c := VRCamera{Yaw: 0.7, Distance: 100}
	c.Rotate(10, 0)
	if math.Abs(c.Yaw-(0.7-0.08)) > 1e-9 || math.Abs(c.Pitch) > 1e-9 || math.Abs(c.Roll) > 1e-9 {
		t.Fatalf("horizontal drag = yaw %v pitch %v roll %v", c.Yaw, c.Pitch, c.Roll)
	}
	c = VRCamera{Yaw: 0.7, Distance: 100}
	c.Rotate(0, 10)
	if math.Abs(c.Yaw-0.7) > 1e-9 || math.Abs(c.Pitch+0.08) > 1e-9 || math.Abs(c.Roll) > 1e-9 {
		t.Fatalf("vertical drag = yaw %v pitch %v roll %v", c.Yaw, c.Pitch, c.Roll)
	}
	// SetOrientation to a pole now keeps an orientation the next drag continues from.
	c.SetOrientation(Vec3{Z: -1})
	_, _, before := c.basis()
	c.Rotate(0, 1)
	_, _, after := c.basis()
	if d := angleBetween(before, after); math.Abs(d-0.008) > 1e-9 {
		t.Fatalf("drag from the caudal snap moved %v rad, want 0.008", d)
	}
}

func TestVRCameraLookAlongAtBothPoles(t *testing.T) {
	for _, forward := range []Vec3{{Z: -1}, {Z: 1}} {
		camera := NewVRCamera(25)
		camera.Target = Vec3{X: 7, Z: 2}
		camera.Projection = VRProjectionEndoscopy
		camera.Roll = 0.3
		previous := camera
		up := Vec3{X: 2, Y: 3, Z: 4}
		camera.LookAlong(forward.Scale(2), up)
		right, screenUp, direction := camera.Basis()
		assertVec3Near(t, direction, forward, 1e-12)
		wantUp := up.Sub(forward.Scale(up.Dot(forward))).Normalize()
		assertVec3Near(t, screenUp, wantUp, 1e-12)
		assertVec3Near(t, right, direction.Cross(wantUp), 1e-12)
		if camera.Target != previous.Target || camera.Distance != previous.Distance ||
			camera.Radius != previous.Radius || camera.FovY != previous.FovY || camera.Projection != previous.Projection {
			t.Fatalf("LookAlong modified saved camera fields: before %+v, after %+v", previous, camera)
		}
	}
}
