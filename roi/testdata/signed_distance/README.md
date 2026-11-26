# ITK signed distance numerical oracle

`itk-5.4.json` contains independently generated numerical outputs for masks
created by `generate.py`. It is synthetic data; no upstream
corpus or implementation source was imported.

Generation used the macOS arm64 SimpleITK 2.5.3 wheel, reporting ITK 5.4,
compiled November 20, 2025. The wheel SHA256 is recorded below. The filter used
zero background, `useImageSpacing=True`, both sign choices and both squared
choices, with float32 output. Generation preceded production comparison.
Twenty cases, eighty results and 15,120 values are checked by the ordinary Go
test; there is no runtime Python/ITK dependency in that test.

The tolerance was set before comparison to absolute 1e-5 plus relative 2e-6.
Empty/full-mask ITK sentinels are excluded from this finite corpus and tested
as an explicit infinity contract in Go. Nonuniform axes are tested against
an independent exhaustive oracle, since ITK's image spacing is uniform.

To regenerate in an environment with NumPy and SimpleITK 2.5.3 installed:

```sh
python3 generate.py
```

The generator refuses a different ITK/SimpleITK version. It writes only the
sibling JSON file. Package installation is optional and external to normal
library tests; no CI or temporary validation module is required.

Wheel SHA256: `5438fb87b7e3380b1ba02b2447bf9f474560b51a649b2ad6973195c6515a43a5`.

Corpus SHA256: `844fcaae395390ef742798ed3dd7b7916b5c98df80d84a000d2a1bb46ea47e46`.
