# ITK test vectors

Test cases ported from the Insight Toolkit (ITK) at commit
`35b196e13c366e0386389a71c936a0436c568cea`. They are test data only. ITK is
Apache-2.0: [LICENSE.ITK](LICENSE.ITK) holds its license and NOTICE. Each
file's provenance, with the ITK sources it came from, is in
`THIRD_PARTY_PROVENANCE.json`.

| File | ITK source | Consumed by | Modifications |
|---|---|---|---|
| `series_direction.json` | `Modules/IO/GDCM/test/itkGDCMSeriesDirectionGTest.cxx` | `render` `TestITKSeriesDirectionOracles` | See below. |
| `transforms/*.tfm`, `transforms/*.mat` | Written by `TransformFileWriter` (`itkTxtTransformIO.cxx`, `itkMatlabTransformIO.cxx`) | `registration` `TestReadITKWrittenTransformFiles`, `TestWriteITKTransformFilesAsITKDoes` | Synthetic transforms; text files normalized to LF. |
| `transforms/points.txt` | ITK's `TransformPoint` through each of those transforms | the same tests | None. |
| `n4/phantom.f32`, `n4/mask.u8` | Synthetic input (dicom-go's N4 test phantom and its positive voxels) | `imagefilter` `TestN4MatchesITK` | Not from ITK. |
| `n4/logbias.f32` | ITK's `N4BiasFieldCorrectionImageFilter` on that input, reconstructed by `BSplineControlPointImageFilter` | the same test | None. |


Modifications to `series_direction.json`:

- Each synthetic `SeriesSpec` is written as data: row and column cosines,
  first Image Position, step, slice count and expected slice spacing.
- The oblique cosines are evaluated in float64 as ITK composes them.
- The direction and origin assertions are adapted to `render.VolumeGeometry`,
  which sorts slices along Row × Column instead of keeping the input order.
  The test documents where the contract differs.
