# Masked rank public fixture contract

`generate.py` authors only deterministic inputs. It never runs ITK/the product
or changes expected outputs. `fixtures.json` contains 96 sealed inputs; the
expectations were initially null. The authorized PUBLIC96 G2 campaign produced
all 12096 expected value bits and validity entries, in fixed order with one
run per case and no retries. The fixture records the raw-output, process,
campaign, binary, harness, runner, seal and SDK hashes; its canonical inputs
remain unchanged. Partial public output is an error, not a green corpus.

The public reference is ITK 5.4.5 tag/commit
`f51594ad88194a72cdc06a314dafff709fcdc2e9`,
[MaskedRankImageFilter](https://docs.itk.org/projects/doxygen/en/v5.4.0/classitk_1_1MaskedRankImageFilter.html)
and [FlatStructuringElement](https://docs.itk.org/projects/doxygen/en/v5.4.0/classitk_1_1FlatStructuringElement.html)
APIs. No reference implementation or fixture is copied. The own C++ harness
only configures these public APIs. Public rank is float; 0/.5/1 are exact.

Cases are fixed-order, unique and deterministic: 72 clipped cases combine
2D/3D, box/ellipsoid, all/checker/empty masks, ranks 0/.5/1 and integral/binary
fractional samples. Another 24 cases combine 2D/3D, box/ellipsoid, all/checker
masks, ranks 0/.5/1 and integral samples with replicated borders.
Dimensions are 7×6×1 / 7×6×5, spacing (.5,1,1.5)mm, radii (.75,1,1.5)mm.
Samples are `((7*x+13*y+19*z)%17)-8`; fractional cases divide by 8 and add 1/16.
Checker validity is `(x+2*y+3*z)%4!=0`, empty fill is −99.5. Total output
cardinality is 12096. Exact finite binary32 bits and validity are required,
without error tolerances, omitted cases or fixture-selected parameters.

The physical kernel bitmap is explicitly authored from binary64 center-distance
membership. ITK's default Ball half-voxel bias is not used. ITK's masked-center
behavior corresponds to RequireValidCenter. Nonfinite and derived-center
policies are independently analytic, not public equivalence claims.
The sealed spacings/radii have exactly represented division quotients, so their
authored bounds and bitmaps remain unchanged by the product's corrected
binary64 axial search. An independent `0.1`/`4.3`/offset43 regression covers
the rounded endpoint that a quotient floor would miss; no public expected
output is adjusted to remedy that product defect.
For Replicate, workunit export pads samples and validity by the same kernel
bounds using edge replication and configures native ITK clipped filtering.
The harness reads the original interior only. This is a declared oracle
adapter, not hidden product padding or native ITK boundary parity.

Harness `defaults2`/`defaults3` returns getters without Update. Workunit mode
records defaults before setting mask1/background0, output mask=true, exact
rank/finite fill/custom kernel and work units=1, then Update. Outputs contain
finite value bits and validity. The SDK/config/loaded archives/harness/binary
were pinned and hashed. Execution used two build workers, a 120s build/defaults
deadline and a separate 60s campaign including finalization, 96 one-run cases
in fixed order, 2s/case, sampled group RSS1GiB and log1MiB. The finite runner
admitted outputs before work and recorded parent reap, known empty groups and
reaped census subprocesses. Preflight took 2.692464s and the public campaign
4.634570s; those are correctness-run durations, not performance measurements.
The Go public-corpus test requires all 12096 finite binary32 bits and validity
entries to match exactly. Benchmarks remain separately pending; no measured
gain or clinical qualification is asserted.
