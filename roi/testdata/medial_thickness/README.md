# Authored medial-thickness primitive oracle

The fifteen 25-cubed binary masks are generated from our analytic tube, sphere,
slab, branching-tube and cavity definitions at three physical spacings. There
are no patient files or copied upstream test assets. The Python generator and
fixture use this repository's license; NumPy, scikit-image and SimpleITK are
external tools, not added application/runtime dependencies. No upstream filter
implementation is copied into the project.

The checked-in gzip stores all foreground indices, an independent Lee skeleton
from scikit-image, and **all foreground** Signed Maurer distances from SimpleITK
with positive inside, nonsquared distances and physical spacing. The generator
digest and exact SDK versions are embedded. Fixed gzip mtime avoids timestamp
drift. Stored size is 15,082 bytes and decoded JSON is 119,574 bytes.

The executed producer used NumPy 2.3.5, scikit-image 0.26.0, SimpleITK 2.5.3
(ITK 5.4). Generator SHA-256:
`c8f3330ba098b8e58ca667a7fe187ba08a7594e5c6796358dd05a5e59025a3b0`.
Use an isolated Python environment to reproduce from this module root:

```sh
python -m venv /path/to/temporary/medial-oracle
/path/to/temporary/medial-oracle/bin/python -m pip install numpy==2.3.5 scikit-image==0.26.0 SimpleITK==2.5.3
/path/to/temporary/medial-oracle/bin/python roi/testdata/medial_thickness/generate.py
go test -race ./roi -run '^TestMedialThicknessIndependentSkeletonAndDistance$' -v
```

On Windows the interpreter is `Scripts/python.exe` inside the environment.
The campaign used a temporary environment outside the checkout. Routine Go
tests read the small fixed fixture and do not install or execute Python.

The two primitive comparisons are separate. Lee skeletons agreed exactly for
these masks; ITK distance round-off is bounded by four float32 epsilons over the
largest physical span. This is not a claim of complete ITK filter parity.
The [library contract and measured continuous-geometry biases](../../../docs/MEDIAL_THICKNESS.md)
show why independent agreement does not imply unbiased anatomical thickness.
