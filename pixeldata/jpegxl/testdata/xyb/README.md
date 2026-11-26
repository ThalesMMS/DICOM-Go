# XYB DCT8 decoder profile

These fixtures contain deterministic synthetic RGB images. They contain no
patient data. They were encoded and independently decoded on Windows amd64
with the official libjxl 0.11.2 static release, commit
`332feb1`. The JSON manifest records every encoded and reference file's hash.

The input images have the dimensions embedded in their PPM references.
The source RGB samples are generated with the expressions below, evaluated
at integer pixel coordinates `x,y`.

| Fixture | Source values | Effort |
|---|---|---:|
| tiny | `32*x+18, 56*y+20, 25*x+18*y` | 1 and 5 |
| colors | `(17*x+3*y)%256, (23*y+5*x)%256, (11*x+13*y)%256` | 3 |
| plain | `135, 14, 141` | 1 |
| chaos | Python `random.Random(35841).randrange(256)` for each channel | 1 |
| tile and groups | `(7*x+3*y)%256, (19*y+5*x)%256, (31*x+13*y)%256` | 3 and 1 |

The supported fixture encoder command is the following, with the effort
from the table. The reference PPM is the decoder's output, which includes
libjxl's output conversion.

```powershell
cjxl input.ppm output.jxl -d 2.1 -e 1 -m 0 --gaborish=0 --epf=0 --num_threads=1 --container=0
djxl output.jxl output.ppm --num_threads=1
```

`unsupported-transform.jxl` uses the colors input with effort 5 and includes
a transform outside DCT8. `unsupported-filter.jxl` uses colors, effort 1,
`--gaborish=1 --epf=1`. Both must return a typed unsupported error.

Normal tests compare all seven supported outputs against every reference
channel within one 8-bit level. They also cover truncated streams, trailing
data, legal DICOM padding, profile limits, cancellation, concurrent use,
registry choice and caller-owned buffer boundaries.

## Reference equations

The implementation shares the existing JPEG XL entropy and DCT8 readers.
Its XYB output path implements the default DCT8 radial weights, AC quantization
bias, channel correlation, adaptive LF smoothing, inverse opsin transform
and sRGB transfer function. The inverse DCT uses the existing orthonormal
basis with a factor of eight for JPEG XL's mean-normalized coefficients.

The matrix and scalar constants are prescribed by ISO/IEC 18181-1. The
fixed libjxl implementation was consulted to check their interpretation
and used as an external oracle. The Go path has no dependency on libjxl.

- [Official release used for the oracle](https://github.com/libjxl/libjxl/releases/tag/v0.11.2)
- [JPEG committee white paper and XYB equations](https://ds.jpeg.org/whitepapers/jpeg-xl-whitepaper.pdf)
- [Default quantization definitions](https://github.com/libjxl/libjxl/blob/v0.11.2/lib/jxl/quant_weights.cc)
- [LF dequantization and smoothing](https://github.com/libjxl/libjxl/blob/v0.11.2/lib/jxl/compressed_dc.cc)
- [Default inverse opsin matrix](https://github.com/libjxl/libjxl/blob/v0.11.2/lib/jxl/cms/opsin_params.h)
