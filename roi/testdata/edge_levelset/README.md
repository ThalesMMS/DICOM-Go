# Authored edge-level-set oracle

`generate.py` independently defines radial images, edge potentials and initial
negative-inside masks, then executes only public SimpleITK ShapeDetection and
CannySegmentationLevelSetImageFilter APIs. It records geometry, values,
initial/output masks, scalings, stopping, iterations and RMS. Canny's exposed
edges and public spacing-aware Danielsson distances record D in mm and D²/2 in
mm². ReverseExpansionDirection=true is the explicit public sign mapping for
the negative-inside Canny attraction convention. No upstream fixture/code is
copied and no private filter data is inspected.

The four original `cases` and acceptance criteria remain unchanged, including
both Shape comparisons that failed Dice>=.95 / symmetric Hausdorff<=two maximum
spacing voxels. Their RED evidence is retained outside the product corpus; Go
regression tests assert/report the original incompatibilities as scoped solver
limitations, not parity successes. Canny 2D/3D meets those fixed criteria.

`positive_cases` is a separately frozen generation 2 extension, created before
its first Go comparison: exact g=0 radial barriers and three-iteration pure
curvature horizons, each in anisotropic 2D and 3D. These use explicit
SumCurvature and RMSOnly/FixedIterations. They meet the same unchanged Dice/HD
criteria. Matching barrier masks stop at the iteration cap with nonzero RMS;
they are not reported as numerical convergence. The extension does not replace
or reclassify either original failure.

Generate with SimpleITK 2.5.3 / ITK 5.4 and NumPy:

```
python3 generate.py
```

JSON serialization rejects NaN. Exact metrics, solver differences, stopping and
boundary policies are in `docs/CANNY_EDGE_LEVELSETS.md`. These fixtures support
bounded independent operation, not full ITK parity or clinical validation.
