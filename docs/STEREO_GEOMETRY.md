# Binocular camera geometry

The V16 view contract adds StereoPresentationState for mono, side-by-side and
red-cyan presentation. DeriveStereoEyeViews performs camera mathematics only:
positions are offset symmetrically along the central right vector and converge
by toe-in on a point along the central forward axis. All generations, physical
sampling and rendering controls are preserved. It neither renders eyes nor
composes pixels. The central camera is immutable; zero separation keeps its
exact pose. Odd width and HiDPI geometry are explicit and bounded.

Distances are millimetres. Finite separation is 0–1000 and finite active
convergence is >0–1000000; separation cannot exceed convergence. Preprojected
geometry and legacy image-distance LOD are rejected for active stereo. Zero
stereo is omitted from legacy JSON. This profile does not model calibrated
human stereo vision or off-axis frustums.

Run go test -race ./render -run '^Test(Stereo|ValidateViewState|VRViewState)'
-count=3. The application renderer/controls and current GPU evidence are in
the Twin stereo profile.
