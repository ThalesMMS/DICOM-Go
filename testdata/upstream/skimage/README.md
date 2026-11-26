# scikit-image threshold fixture

`thresholds.json` holds automatic thresholds computed by scikit-image 0.26.0
(tag `v0.26.0`, commit `ee0a7a3ebd9ac8c2602f40e55bc015a3c8a81ae8`, the PyPI
wheel) on synthetic integer images described by their histograms: `min` is
the smallest value and `counts` the pixels of each value from `min` upwards,
Gaussian mixtures (and one exponential tail) rounded to integer counts. It is
test data only; scikit-image is BSD-3-Clause ([LICENSE.skimage](LICENSE.skimage))
and the fixture's provenance, with the sources that computed it, is in
`THIRD_PARTY_PROVENANCE.json`.

| Case | Shape |
|---|---|
| `bimodal` | two Gaussians of similar weight |
| `bimodal_unequal` | a narrow heavy mode and a wide light one |
| `trimodal`, `four_modes` | three and four Gaussians (multi-Otsu) |
| `skewed_tail` | one narrow peak with a long exponential tail (Triangle) |
| `negative_offset` | a bimodal mixture over negative values (Li's shift) |

Integer images take scikit-image's bincount path, one bin per integer, which
is the histogram `roi/autothreshold` `TestThresholdsMatchScikitImage` builds
with `BinWidth` 1. The test compares class partitions: a scikit-image value
`t` (foreground `image > t`) must equal the split whose lower class holds the
values `<= t`. Otsu, Li, Yen, IsoData, Triangle, Mean and Minimum must agree
exactly; a method that raised is `null`. For multi-Otsu (3 and 4 classes)
the first threshold must agree and the others lie within one bin: the test
checks that dicom-go's split is the exhaustive optimum of the between-class
variance, while scikit-image 0.26.0 places some later thresholds one bin
lower, a partition with a smaller between-class variance.

