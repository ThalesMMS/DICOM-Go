# Authored Canny oracle

`generate.py` defines ten synthetic regular 2D/3D geometries and executes public
SimpleITK CannyEdgeDetectionImageFilter, with single-threaded oracle execution.
The JSON records runtime version, dimensions, physical spacing, sigma/variance,
thresholds, source values, binary edges and public threshold unit sweeps.
No upstream fixture or implementation is copied.

Expected arrays and criteria were frozen before Go comparison. Acceptance is
bidirectional edge distance <=one maximum-spacing voxel, equal full 8/26
component counts and an exactly empty constant image. The independent Go
comparison uses brute-force positions, not the production EDT. All ten cases,
including sphere and anisotropic planes, remain in the corpus.

Generate using SimpleITK 2.5.3 / ITK 5.4 and NumPy:

```
python3 generate.py
```

JSON serialization forbids NaN. Go uses recursive Gaussian/interpolated NMS;
ITK's discrete Canny differs. This is a geometric/connectivity oracle, not a
bitwise output claim. See `docs/CANNY_EDGE_LEVELSETS.md` for the exact boundary
and plateau contract, units, resource reservations and limitations.
