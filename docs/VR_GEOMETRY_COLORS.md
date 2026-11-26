# Per-vertex geometry colors

`render.VRGeometryMesh.Colors` optionally supplies one linear-light RGB float32
triplet per vertex. Each component must be finite and in `[0,1]`. An absent list
retains the existing uniform `Color`; a present mismatched list is invalid.
`ValidateVertexColorsContext` validates the complete attribute with cancellation
checks, while `ValidateVertexColors` uses a background context. Geometry buffers
are immutable after publication to a clip or renderer.

The headless CPU geometry oracle interpolates these attributes over triangles.
Tests prove that distinct colors reach the covered image and malformed counts,
nonfinite values and out-of-range components return an error without a partial
image. These tests qualify library behavior only. Twin's GPU application path
packs the same attributes into its existing vertex-color upload contract; it
never uses the CPU oracle as a fallback.

Stored tractography can therefore retain direction/scalar color variation along
each prepared tube/ribbon without splitting every triangle into a separate mesh
or losing its original measurement identity. Color attributes do not assign a
physical area, volume or scalar unit to display geometry. Native GPU rendering
and its cost require separate application qualification.
