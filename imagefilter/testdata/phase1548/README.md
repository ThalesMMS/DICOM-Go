# Fixed 2D phase research campaign

Own synthetic Gaussian ridges and seeded noise; no patient data or third-party
assets. `plan.json` fixes the family, parameters, development/held-out split,
metrics and promotion gate before measuring the new implementation. Its SHA-256
is `ae35b18f5661a0383d6f2a1f8548c222f314e13b7f45fb7f493371adc7de43ab`;
the opt-in test refuses a changed plan. An altered corpus needs a separately
preregistered campaign, not an adjustment of this gate after seeing results.

The Windows campaign **failed the promotion gate**. It remains experimental.
The numerical contract, resources and failure are in
[PHASE_WAVELET_2D.md](../../../docs/PHASE_WAVELET_2D.md).

`evidence-windows/` preserves the QPC-timed result, whole-process sampled peak
working set, allocation benchmark, independent NumPy oracle and a comparison
figure. `manifest.json` pins those artifacts by SHA-256 and byte size. Source,
phase and existing Frangi maps are headerless little-endian float32, 96 columns
by 96 rows, x-fastest. Cases 0 through 4 follow the plan: development first,
then the four held-out cases. The response hashes also appear in `campaign.json`.
Native executables are not committed.

Normal Go tests use the analytic contracts and replay the pinned own inputs;
they require neither NumPy nor matplotlib. Optional fresh measurement:

```powershell
# From dicom-go-dev; choose a new temporary output directory.
$phaseOutput = Join-Path $env:TEMP 'phase-research-run'
New-Item -ItemType Directory -Path $phaseOutput -Force | Out-Null
$env:DICOM_PHASE1548_REPORT = Join-Path $phaseOutput 'campaign.json'
$env:GOMAXPROCS = '4'
go test ./imagefilter -run '^TestPhase2DResearchCampaign$' -count=1 -v
py -3.12 imagefilter/testdata/phase1548/compare_numpy.py $phaseOutput
go test ./imagefilter -run '^$' -bench '^BenchmarkPhaseSymmetry2DResearch$' -benchtime=3x -count=3
```

The saved oracle used Python 3.12.0, NumPy 2.5.2 and matplotlib 3.10.8.
`compare_numpy.py` derives the declared transfer functions independently using
NumPy FFT, checks source reconstruction/phase maps, and creates the figure.
It does not compare to ITK's oriented log-Gabor implementation: that is a
different filter bank. The published mathematical references are in the report.
The figure scales each source independently for visibility, while both response
columns use the same fixed [0,1] range. PNGs are visualization, not the numeric
oracle. Measured timing/RSS are host observations, not deterministic fixtures.
