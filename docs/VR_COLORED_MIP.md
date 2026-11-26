# Colored maximum-intensity projection

`render.VRPreset.ColoredMIP` selects an opaque RGB maximum-intensity
projection when `Mode == VRModeMIP`. It defaults to false; other modes ignore
the retained flag. The selected sample is the greatest modality value along
the visible ray, including rescale and clipping, rather than the brightest
colour or the largest value after window clipping.

The maximum is classified through the preset's 1D RGB transfer function.
Normalized transfer functions use their baked continuous VR window domain;
HU transfer functions use their absolute modality domain. Transfer alpha,
gradient opacity and shading do not attenuate this projection. Inversion
complements each sRGB display channel before converting back to linear light.
Opaque geometry limits the ray; translucent fragments composite over the
projection using the existing projection scene contract.

`VRMIPColor` classifies one modality value using a baked `VRLUT` and forces
opaque output. `RenderVRContext` remains available for independent library
consumers and headless tests. The application uses its GPU worker; this library
renderer is never an application preview, final frame or fallback.

Windowed empty-space bounds cannot prove raw maxima for a non-monotonic CLUT,
so the headless colored-MIP profile uses the bounded full march. Tests verify
the raw maximum above the window, non-monotonic colour stops, zero transfer
opacity, inversion and unchanged default grayscale projection.
