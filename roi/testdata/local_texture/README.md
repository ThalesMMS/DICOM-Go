# Authored ITK local-window oracle

Own constants, periodic and heterogeneous patterns and a partial mask on a
7-cubed grid; no patient or copied reference assets. `generate.cpp` is an
authored driver of installed ITK public APIs, using
`ScalarImageToCooccurrenceMatrixFilter` and `HistogramToTextureFeaturesFilter`
for each exact clipped window, four features and average/merged directions.
It performs global fixed-width quantization: integer values 0..7, eight bins,
explicit minimum 0, maximum 7, and symmetric histograms. A missing centre/pair
is stored as null so undefined validity is compared separately from zero.

This is a contract-equivalent window comparison with **ITK 5.4.6** primitives,
not execution or parity of the newer
[CoocurrenceTextureFeaturesImageFilter](https://github.com/InsightSoftwareConsortium/ITK/blob/be770907fc62a2ae0b1395a1f092465d7ab9b535/Modules/Filtering/TextureFeatures/include/itkCoocurrenceTextureFeaturesImageFilter.h).
That filter is absent from the installed SDK. The named filter's input/range,
direction, normalization and boundary semantics must be aligned explicitly;
its output convention for missing data is not adopted as quantitative zero.
Window-minimum quantization and non-finite float inputs use the independent
direct oracle, not an ITK parity claim.

Run once from the library module on the qualified Windows SDK host:

```powershell
$env:Path = 'C:\msys64\ucrt64\bin;C:\msys64\usr\bin;' + $env:Path
cmake -S roi/testdata/local_texture -B "$env:TEMP\go-dev-1540-itk-build" `
  -G Ninja -DITK_DIR=C:/msys64/ucrt64/lib/cmake/ITK -DCMAKE_BUILD_TYPE=Release
cmake --build "$env:TEMP\go-dev-1540-itk-build" --parallel 4
& "$env:TEMP\go-dev-1540-itk-build\local_texture_oracle.exe" `
  roi/testdata/local_texture/itk.json
go test -race ./roi -run '^TestLocalTextureIndependentITKWindows$' -count=1 -v
```

The driver uses Windows process-memory APIs only for this campaign's peak RSS;
it is neither an application dependency nor a permanent validation module.
Routine Go tests read the checked-in 173,840-byte LF-normalized JSON without building ITK.
The fixture contains producer version/time/RSS. Generator SHA256:
`ee01a93877a1083e95f2030b111367a3c747c4fe344aa889c69826888310263d`.
Fixture SHA256:
`60e0363a5dca47d9351e26027d90294e0ccde407abdf606a515e5a4211f56378`.
The time/RSS fields describe that generation, so rebuilding can change its
digest without changing the mathematical case definitions or feature values.
