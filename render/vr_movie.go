package render

import (
	"image"
	"io"
	"math"
)

// Orbit-movie support for the 3D renderer (3d-toolbar-plan.md §5.13). RenderVRMovie
// orbits the camera over an arc and renders one frame per step; EncodeGIF packs
// the frames into an animated GIF (adaptive palette) and EncodeMJPEGAVI into a
// Motion-JPEG AVI, both without external encoders.

// RenderVRMovie renders `frames` images orbiting the camera in yaw over arcDeg
// degrees (e.g. 360 for a full turntable, 180 for a half sweep).
func RenderVRMovie(vol *Volume, cam VRCamera, preset VRPreset, window WindowLevel, shading bool, clip *VRClip, arcDeg float64, frames int, quality VRQuality) []image.Image {
	if frames < 1 {
		frames = 1
	}
	arc := arcDeg * math.Pi / 180
	out := make([]image.Image, 0, frames)
	for i := 0; i < frames; i++ {
		c := cam
		c.Yaw = cam.Yaw + arc*float64(i)/float64(frames)
		out = append(out, RenderVR(vol, c, preset, window, shading, clip, quality))
	}
	return out
}

// EncodeGIF writes the frames as an animated GIF that loops forever with the
// given per-frame delay in centiseconds (100/fps). Frames share one adaptive
// median-cut palette and are Floyd–Steinberg dithered (see EncodeAdaptiveGIF),
// which removes the banding a fixed palette causes on smooth VR shading.
func EncodeGIF(w io.Writer, frames []image.Image, delayCs int) error {
	return EncodeAdaptiveGIF(w, frames, GIFOptions{DelayCs: delayCs})
}
