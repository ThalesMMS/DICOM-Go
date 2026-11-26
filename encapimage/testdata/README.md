# Synthetic JPEG fixtures

32x24 gradients generated for this repository (no patient data) and encoded
with libjpeg-turbo 3.2.0 `cjpeg`:

| File | Command |
| --- | --- |
| `baseline_gray.jpg` | `cjpeg -quality 90 gray8.pgm` |
| `baseline_444.jpg` | `cjpeg -quality 90 -sample 1x1 color.ppm` |
| `baseline_422.jpg` | `cjpeg -quality 90 -sample 2x1 color.ppm` |
| `baseline_rgb.jpg` | `cjpeg -quality 90 -rgb color.ppm` (Adobe APP14, transform 0) |
| `extended12_gray.jpg` | `cjpeg -quality 90 -precision 12 gray12.pgm` |
| `progressive.jpg` | `cjpeg -quality 90 -progressive color.ppm` |
| `lossless_sv1_gray16.jpg` | `cjpeg -precision 16 -lossless 1 gray16.pgm` |
| `lossless_p4_rgb.jpg` | `cjpeg -rgb -lossless 4 color.ppm` |
| `arithmetic.jpg` | `cjpeg -arithmetic color.ppm` |

Sources: gray sample (x, y) = (37x + 101y) * max / (37*32 + 101*24) with max
255, 4095 or 65535; color pixel (x, y) = (8x, 10y, xy) mod 256. The tests
rebuild these values to check the lossless fixtures exactly.
