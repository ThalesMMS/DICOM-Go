# Prepared deformable inverse

`Registration.PrepareInverse(options)` snapshots Pre/Post matrices, grid
geometry and all float32 displacement vectors. Construction computes the two
affine inverses and their constant linear/determinant parts once. Subsequent
`PreparedInverse.InvertPoint(ctx, source)` and `InvertPoints(ctx, points)` use
that generation without a dense inverse field, previous-point initial guess,
shared mutable cache or alternate inversion algorithm. Existing
`Registration.InvertPoint` and `MapSourceToRegistered` remain available.

Preparation requires the caller not to mutate the registration/grid during
construction. Mutation after return cannot change the prepared snapshot.
Callers must keep their batch input points stable until the call returns.
Independent operations may run concurrently. Do not copy a PreparedInverse;
its pointer is the owner. Close is idempotent, waits for active operations,
drops copied vectors and rejects later queries with ErrInverseClosed.

The fixed-point iteration, step-halving, residual convention, trilinear
sampling and determinant check retain the original definitions. Maximum
iterations, tolerance and maximum residual retain InverseOptions' existing
normalization. Folding, outside/undefined domain, invalid matrices and
non-convergence remain failures; a prepared evaluator does not make a
previously rejected solution acceptable. Numerical point errors retain the
original diagnostic InverseResult. InverseBatchError identifies only the
failing index/result and unwraps the original error; no prior successful
batch results are published. Cancellation returns nil for the batch and the
context error (possibly wrapped at the failing point); partially built results
are discarded. Point cancellation returns a zero result.

Batch execution is serial, with context checks before allocation, between
points and during fixed-point iterations, and before successful publication.
There are no worker goroutines or background tasks. These are work checks,
not a wall-clock guarantee: allocation, scheduling, lock acquisition and a
caller-supplied Context implementation can delay completion. Close cannot
force a Context implementation to return or cancel its callers.

Retention includes a full owned copy of the original float32 field: 12 bytes
per grid vector, plus metadata/matrices/mutex/allocator overhead.
RetainedGridBytes reports only that logical vector payload and becomes zero
after Close. Batch results require one InverseResult per point (use
unsafe.Sizeof on the target architecture when budgeting). These logical
payloads are neither cumulative B/op nor whole-process peak RSS. Caller
admission remains necessary; this API does not create an unbounded dense
inverse cache or promise a fixed process-memory quota.

Prepared independent/frozen-original tests cover identity/affine, known
constant/linear and smooth fields, anisotropic pre/post transforms, query
reordering, exact residual/iterations/determinants, failed inverses,
post-preparation mutation, concurrent read batches, cancel without partial
publication and Close waiting for the snapshot lease. The fixed 11 focal cases passed on darwin/arm64. The original existing package cases remain affected callers.

The comparative benchmark separates original matrix preparation, prepared
construction/copy/Close, evaluation, and total construction-plus-queries.
It includes point-loop and batch consumption, sizes1/16/4096, default/strict
options and small/large fields. Original benchmark modes never allocate a
prepared snapshot even during setup. The actual fixed 576-run comparison is reported below. Twin adoption requires measured net benefit including the
snapshot cost and retention in one demonstrated consumer; otherwise its
existing mapper remains. No UI, GPU rendering policy or clinical scope changes.


## Actual comparative measurements

Go 1.27.1, darwin/arm64, GOMAXPROCS=2, GOMEMLIMIT=1GiB (soft runtime
setting), one shared host with the root coordinating the quiet Go/GUI window.
Library generation 4 restores the original point API implementation exactly;
its mathematical kernels and benchmark fixtures/options were frozen before
this campaign. All 192 cells have three independent runs (576 total), with
exact original/prepared result, residual, iteration and determinant oracles.
Generation 5 adds only `Grid.VectorBytes`, its storage-accessor test, and this
documentation; none of those measured kernels changed. This read-only accessor
reports actual logical private-vector bytes even if public Dimensions were
modified; it requires stable source ownership and excludes header/capacity/RSS.

The 4096-point current/original total median ratios are 0.965–1.021. Prepared
point-loop totals for deformable 4096-point cases are 0.834–0.870 of original,
including snapshot construction and Close. Smaller operations can be slower;
construction and retained-field costs are present. These are descriptive
host-specific results, not a universal speedup, crossover or statistical claim.
Each table cell below is median [minimum, maximum]; B/op is cumulative Go
allocation, RSS is the whole process including fixtures and validation, and
logical copied vectors/returned results exclude metadata and allocator costs.
The original source baseline is c6f51ddac9f0e6ccc1cf913644b1b21bccc875a8.

| Field | Points | Strict | Mode | ns/op | B/op | allocs/op | Copied vectors B | Returned results B | Whole-process peak RSS B |
|---|---:|---|---|---:|---:|---:|---:|---:|---:|
| identity | 1 | False | original-constants | 229.2 [208.3, 266.7] | 0 [0, 0] | 0 [0, 0] | 0 [0, 0] | 0 [0, 0] | 23953408 [23855104, 24133632] |
| identity | 1 | False | prepared-construction | 779.1 [612.5, 800] | 1120 [1120, 1120] | 2 [2, 2] | 0 [0, 0] | 0 [0, 0] | 24018944 [23789568, 24231936] |
| identity | 1 | False | original-total | 483.3 [450, 500] | 48 [48, 48] | 1 [1, 1] | 0 [0, 0] | 48 [48, 48] | 23789568 [23773184, 24182784] |
| identity | 1 | False | current-total | 470.9 [429.2, 483.4] | 48 [48, 48] | 1 [1, 1] | 0 [0, 0] | 48 [48, 48] | 24084480 [23724032, 24100864] |
| identity | 1 | False | prepared-evaluation | 125 [125, 129.2] | 48 [48, 48] | 1 [1, 1] | 0 [0, 0] | 48 [48, 48] | 24117248 [24002560, 24215552] |
| identity | 1 | False | prepared-total | 662.5 [612.5, 912.5] | 1168 [1168, 1168] | 3 [3, 3] | 0 [0, 0] | 48 [48, 48] | 24051712 [23953408, 24166400] |
| identity | 1 | False | prepared-point-evaluation | 137.5 [129.2, 166.7] | 48 [48, 48] | 1 [1, 1] | 0 [0, 0] | 48 [48, 48] | 23953408 [23773184, 24150016] |
| identity | 1 | False | prepared-point-total | 658.4 [595.8, 662.5] | 1168 [1168, 1168] | 3 [3, 3] | 0 [0, 0] | 48 [48, 48] | 23887872 [23789568, 23969792] |
| identity | 1 | True | original-constants | 179.2 [179.2, 187.5] | 0 [0, 0] | 0 [0, 0] | 0 [0, 0] | 0 [0, 0] | 23789568 [23740416, 23904256] |
| identity | 1 | True | prepared-construction | 541.7 [475, 575] | 1120 [1120, 1120] | 2 [2, 2] | 0 [0, 0] | 0 [0, 0] | 24117248 [24018944, 24182784] |
| identity | 1 | True | original-total | 458.4 [445.8, 487.5] | 48 [48, 48] | 1 [1, 1] | 0 [0, 0] | 48 [48, 48] | 23920640 [23773184, 24084480] |
| identity | 1 | True | current-total | 437.5 [366.7, 462.5] | 48 [48, 48] | 1 [1, 1] | 0 [0, 0] | 48 [48, 48] | 24100864 [23969792, 24182784] |
| identity | 1 | True | prepared-evaluation | 116.7 [83.3, 125] | 48 [48, 48] | 1 [1, 1] | 0 [0, 0] | 48 [48, 48] | 24150016 [24100864, 24166400] |
| identity | 1 | True | prepared-total | 691.7 [675, 929.2] | 1168 [1168, 1168] | 3 [3, 3] | 0 [0, 0] | 48 [48, 48] | 23871488 [23789568, 23986176] |
| identity | 1 | True | prepared-point-evaluation | 120.8 [108.3, 150] | 48 [48, 48] | 1 [1, 1] | 0 [0, 0] | 48 [48, 48] | 23871488 [23756800, 24002560] |
| identity | 1 | True | prepared-point-total | 841.6 [829.2, 1325] | 1168 [1168, 1168] | 3 [3, 3] | 0 [0, 0] | 48 [48, 48] | 24084480 [23805952, 24264704] |
| identity | 16 | False | original-constants | 175 [175, 179.2] | 0 [0, 0] | 0 [0, 0] | 0 [0, 0] | 0 [0, 0] | 23969792 [23953408, 23969792] |
| identity | 16 | False | prepared-construction | 604.2 [595.8, 616.7] | 1120 [1120, 1120] | 2 [2, 2] | 0 [0, 0] | 0 [0, 0] | 24002560 [23789568, 24051712] |
| identity | 16 | False | original-total | 3275 [2758, 3462] | 768 [768, 768] | 1 [1, 1] | 0 [0, 0] | 768 [768, 768] | 24051712 [23838720, 24117248] |
| identity | 16 | False | current-total | 2888 [2600, 2962] | 768 [768, 768] | 1 [1, 1] | 0 [0, 0] | 768 [768, 768] | 23838720 [23805952, 23871488] |
| identity | 16 | False | prepared-evaluation | 879.1 [833.3, 1004] | 768 [768, 768] | 1 [1, 1] | 0 [0, 0] | 768 [768, 768] | 23822336 [23789568, 23920640] |
| identity | 16 | False | prepared-total | 1488 [1479, 1588] | 1888 [1888, 1888] | 3 [3, 3] | 0 [0, 0] | 768 [768, 768] | 23838720 [23789568, 23904256] |
| identity | 16 | False | prepared-point-evaluation | 1229 [1179, 1238] | 768 [768, 768] | 1 [1, 1] | 0 [0, 0] | 768 [768, 768] | 24231936 [23920640, 24313856] |
| identity | 16 | False | prepared-point-total | 1738 [1604, 1958] | 1888 [1888, 1889] | 3 [3, 3] | 0 [0, 0] | 768 [768, 768] | 23953408 [23822336, 24166400] |
| identity | 16 | True | original-constants | 179.1 [175, 200] | 0 [0, 0] | 0 [0, 0] | 0 [0, 0] | 0 [0, 0] | 24051712 [23986176, 24166400] |
| identity | 16 | True | prepared-construction | 737.5 [666.7, 916.7] | 1120 [1120, 1120] | 2 [2, 2] | 0 [0, 0] | 0 [0, 0] | 23986176 [23822336, 24100864] |
| identity | 16 | True | original-total | 5888 [3312, 6179] | 768 [768, 768] | 1 [1, 1] | 0 [0, 0] | 768 [768, 768] | 23773184 [23756800, 23855104] |
| identity | 16 | True | current-total | 3267 [2879, 7467] | 768 [768, 768] | 1 [1, 1] | 0 [0, 0] | 768 [768, 768] | 24018944 [23986176, 24248320] |
| identity | 16 | True | prepared-evaluation | 945.9 [904.2, 1350] | 768 [768, 768] | 1 [1, 1] | 0 [0, 0] | 768 [768, 768] | 24035328 [23773184, 24084480] |
| identity | 16 | True | prepared-total | 1879 [1654, 1917] | 1888 [1888, 1888] | 3 [3, 3] | 0 [0, 0] | 768 [768, 768] | 24002560 [23969792, 24100864] |
| identity | 16 | True | prepared-point-evaluation | 1158 [1042, 1242] | 768 [768, 768] | 1 [1, 1] | 0 [0, 0] | 768 [768, 768] | 23822336 [23691264, 23986176] |
| identity | 16 | True | prepared-point-total | 1625 [1579, 1808] | 1888 [1888, 1888] | 3 [3, 3] | 0 [0, 0] | 768 [768, 768] | 23937024 [23822336, 24150016] |
| identity | 4096 | False | original-constants | 195.8 [162.5, 195.8] | 0 [0, 0] | 0 [0, 0] | 0 [0, 0] | 0 [0, 0] | 23805952 [23707648, 23953408] |
| identity | 4096 | False | prepared-construction | 633.4 [583.4, 745.8] | 1120 [1120, 1120] | 2 [2, 2] | 0 [0, 0] | 0 [0, 0] | 23920640 [23887872, 24002560] |
| identity | 4096 | False | original-total | 545592 [535079, 564638] | 196608 [196608, 196608] | 1 [1, 1] | 0 [0, 0] | 196608 [196608, 196608] | 26214400 [25985024, 26230784] |
| identity | 4096 | False | current-total | 534767 [532600, 556871] | 196608 [196608, 196608] | 1 [1, 1] | 0 [0, 0] | 196608 [196608, 196608] | 26116096 [26034176, 26181632] |
| identity | 4096 | False | prepared-evaluation | 141838 [140838, 145454] | 196608 [196608, 196608] | 1 [1, 1] | 0 [0, 0] | 196608 [196608, 196608] | 26411008 [26214400, 26443776] |
| identity | 4096 | False | prepared-total | 142675 [140404, 143067] | 197728 [197728, 197728] | 3 [3, 3] | 0 [0, 0] | 196608 [196608, 196608] | 25985024 [25935872, 26165248] |
| identity | 4096 | False | prepared-point-evaluation | 177917 [174488, 178271] | 196608 [196608, 196608] | 1 [1, 1] | 0 [0, 0] | 196608 [196608, 196608] | 26066944 [26066944, 26263552] |
| identity | 4096 | False | prepared-point-total | 177238 [176529, 181233] | 197728 [197728, 197728] | 3 [3, 3] | 0 [0, 0] | 196608 [196608, 196608] | 26099712 [25903104, 26148864] |
| identity | 4096 | True | original-constants | 162.5 [158.3, 175] | 0 [0, 0] | 0 [0, 0] | 0 [0, 0] | 0 [0, 0] | 23871488 [23838720, 23871488] |
| identity | 4096 | True | prepared-construction | 708.3 [666.7, 775] | 1120 [1120, 1120] | 2 [2, 2] | 0 [0, 0] | 0 [0, 0] | 24117248 [23920640, 24117248] |
| identity | 4096 | True | original-total | 548658 [539425, 558225] | 196608 [196608, 196608] | 1 [1, 1] | 0 [0, 0] | 196608 [196608, 196608] | 25968640 [25935872, 26361856] |
| identity | 4096 | True | current-total | 529650 [529062, 576658] | 196608 [196608, 196608] | 1 [1, 1] | 0 [0, 0] | 196608 [196608, 196608] | 26181632 [26066944, 26214400] |
| identity | 4096 | True | prepared-evaluation | 137929 [137688, 143179] | 196608 [196608, 196608] | 1 [1, 1] | 0 [0, 0] | 196608 [196608, 196608] | 26165248 [25968640, 26181632] |
| identity | 4096 | True | prepared-total | 142062 [141083, 143267] | 197728 [197728, 197728] | 3 [3, 3] | 0 [0, 0] | 196608 [196608, 196608] | 26083328 [25722880, 26132480] |
| identity | 4096 | True | prepared-point-evaluation | 179567 [176325, 180508] | 196608 [196608, 196608] | 1 [1, 1] | 0 [0, 0] | 196608 [196608, 196608] | 26066944 [25870336, 26198016] |
| identity | 4096 | True | prepared-point-total | 175175 [173975, 176825] | 197728 [197728, 197728] | 3 [3, 3] | 0 [0, 0] | 196608 [196608, 196608] | 25985024 [25935872, 26263552] |
| affine | 1 | False | original-constants | 179.1 [166.7, 212.5] | 0 [0, 0] | 0 [0, 0] | 0 [0, 0] | 0 [0, 0] | 24051712 [23904256, 24199168] |
| affine | 1 | False | prepared-construction | 579.1 [550, 1092] | 1120 [1120, 1120] | 2 [2, 2] | 0 [0, 0] | 0 [0, 0] | 24068096 [24002560, 24199168] |
| affine | 1 | False | original-total | 462.5 [400, 470.9] | 48 [48, 48] | 1 [1, 1] | 0 [0, 0] | 48 [48, 48] | 23937024 [23838720, 23953408] |
| affine | 1 | False | current-total | 454.2 [416.7, 479.2] | 48 [48, 48] | 1 [1, 1] | 0 [0, 0] | 48 [48, 48] | 24068096 [24068096, 24084480] |
| affine | 1 | False | prepared-evaluation | 116.7 [108.3, 141.7] | 48 [48, 48] | 1 [1, 1] | 0 [0, 0] | 48 [48, 48] | 23855104 [23805952, 24084480] |
| affine | 1 | False | prepared-total | 720.8 [716.6, 745.9] | 1168 [1168, 1168] | 3 [3, 3] | 0 [0, 0] | 48 [48, 48] | 23937024 [23773184, 24018944] |
| affine | 1 | False | prepared-point-evaluation | 158.4 [112.5, 162.5] | 48 [48, 48] | 1 [1, 1] | 0 [0, 0] | 48 [48, 48] | 24133632 [23789568, 24363008] |
| affine | 1 | False | prepared-point-total | 745.9 [729.2, 829.2] | 1168 [1168, 1168] | 3 [3, 3] | 0 [0, 0] | 48 [48, 48] | 23904256 [23707648, 23920640] |
| affine | 1 | True | original-constants | 162.5 [158.3, 204.2] | 0 [0, 0] | 0 [0, 0] | 0 [0, 0] | 0 [0, 0] | 23740416 [23724032, 24215552] |
| affine | 1 | True | prepared-construction | 679.2 [625, 866.6] | 1120 [1120, 1120] | 2 [2, 2] | 0 [0, 0] | 0 [0, 0] | 23953408 [23789568, 24002560] |
| affine | 1 | True | original-total | 395.9 [379.2, 408.4] | 48 [48, 48] | 1 [1, 1] | 0 [0, 0] | 48 [48, 48] | 23887872 [23576576, 24051712] |
| affine | 1 | True | current-total | 462.5 [400, 1371] | 48 [48, 48] | 1 [1, 1] | 0 [0, 0] | 48 [48, 48] | 24084480 [23871488, 24100864] |
| affine | 1 | True | prepared-evaluation | 150 [104.2, 154.1] | 48 [48, 48] | 1 [1, 1] | 0 [0, 0] | 48 [48, 48] | 23822336 [23707648, 23904256] |
| affine | 1 | True | prepared-total | 770.8 [770.8, 812.5] | 1168 [1168, 1168] | 3 [3, 3] | 0 [0, 0] | 48 [48, 48] | 23691264 [23625728, 23969792] |
| affine | 1 | True | prepared-point-evaluation | 137.5 [108.4, 150] | 48 [48, 48] | 1 [1, 1] | 0 [0, 0] | 48 [48, 48] | 23838720 [23773184, 23904256] |
| affine | 1 | True | prepared-point-total | 725 [629.2, 833.3] | 1168 [1168, 1168] | 3 [3, 3] | 0 [0, 0] | 48 [48, 48] | 23822336 [23707648, 24150016] |
| affine | 16 | False | original-constants | 179.2 [166.6, 187.5] | 0 [0, 0] | 0 [0, 0] | 0 [0, 0] | 0 [0, 0] | 23805952 [23642112, 23937024] |
| affine | 16 | False | prepared-construction | 775 [666.7, 870.8] | 1120 [1120, 1120] | 2 [2, 2] | 0 [0, 0] | 0 [0, 0] | 23969792 [23707648, 24150016] |
| affine | 16 | False | original-total | 3612 [3421, 3625] | 768 [768, 768] | 1 [1, 1] | 0 [0, 0] | 768 [768, 768] | 24068096 [23937024, 24150016] |
| affine | 16 | False | current-total | 3225 [3188, 3708] | 768 [768, 768] | 1 [1, 1] | 0 [0, 0] | 768 [768, 768] | 24035328 [23740416, 24248320] |
| affine | 16 | False | prepared-evaluation | 1017 [758.4, 1179] | 768 [768, 768] | 1 [1, 1] | 0 [0, 0] | 768 [768, 768] | 23953408 [23887872, 24199168] |
| affine | 16 | False | prepared-total | 1658 [1638, 1833] | 1888 [1888, 1888] | 3 [3, 3] | 0 [0, 0] | 768 [768, 768] | 23986176 [23625728, 24018944] |
| affine | 16 | False | prepared-point-evaluation | 1408 [1025, 1579] | 768 [768, 769] | 1 [1, 1] | 0 [0, 0] | 768 [768, 768] | 24051712 [23691264, 24150016] |
| affine | 16 | False | prepared-point-total | 1783 [1696, 2004] | 1888 [1888, 1888] | 3 [3, 3] | 0 [0, 0] | 768 [768, 768] | 24018944 [23691264, 24199168] |
| affine | 16 | True | original-constants | 183.4 [179.2, 200] | 0 [0, 0] | 0 [0, 0] | 0 [0, 0] | 0 [0, 0] | 23724032 [23724032, 24035328] |
| affine | 16 | True | prepared-construction | 783.4 [654.2, 795.8] | 1120 [1120, 1120] | 2 [2, 2] | 0 [0, 0] | 0 [0, 0] | 23986176 [23789568, 24084480] |
| affine | 16 | True | original-total | 3292 [2954, 3367] | 768 [768, 768] | 1 [1, 1] | 0 [0, 0] | 768 [768, 768] | 23871488 [23756800, 23986176] |
| affine | 16 | True | current-total | 3367 [3321, 3396] | 768 [768, 768] | 1 [1, 1] | 0 [0, 0] | 768 [768, 768] | 23855104 [23707648, 24084480] |
| affine | 16 | True | prepared-evaluation | 1021 [1008, 1046] | 768 [768, 768] | 1 [1, 1] | 0 [0, 0] | 768 [768, 768] | 23953408 [23937024, 24117248] |
| affine | 16 | True | prepared-total | 1642 [1417, 1842] | 1888 [1888, 1888] | 3 [3, 3] | 0 [0, 0] | 768 [768, 768] | 23937024 [23740416, 24051712] |
| affine | 16 | True | prepared-point-evaluation | 1062 [1021, 1146] | 768 [768, 768] | 1 [1, 1] | 0 [0, 0] | 768 [768, 768] | 23937024 [23822336, 24051712] |
| affine | 16 | True | prepared-point-total | 1592 [1575, 1746] | 1888 [1888, 1888] | 3 [3, 3] | 0 [0, 0] | 768 [768, 768] | 23838720 [23838720, 24018944] |
| affine | 4096 | False | original-constants | 187.5 [162.5, 191.7] | 0 [0, 0] | 0 [0, 0] | 0 [0, 0] | 0 [0, 0] | 23887872 [23822336, 24117248] |
| affine | 4096 | False | prepared-construction | 608.3 [554.2, 708.3] | 1120 [1120, 1120] | 2 [2, 2] | 0 [0, 0] | 0 [0, 0] | 23953408 [23937024, 24264704] |
| affine | 4096 | False | original-total | 539529 [531717, 552225] | 196608 [196608, 196608] | 1 [1, 1] | 0 [0, 0] | 196608 [196608, 196608] | 26296320 [26181632, 26345472] |
| affine | 4096 | False | current-total | 539467 [538854, 542446] | 196608 [196608, 196608] | 1 [1, 1] | 0 [0, 0] | 196608 [196608, 196608] | 26165248 [26083328, 26279936] |
| affine | 4096 | False | prepared-evaluation | 140342 [140304, 143496] | 196608 [196608, 196608] | 1 [1, 1] | 0 [0, 0] | 196608 [196608, 196608] | 26198016 [26001408, 26296320] |
| affine | 4096 | False | prepared-total | 141704 [139679, 142992] | 197728 [197728, 197728] | 3 [3, 3] | 0 [0, 0] | 196608 [196608, 196608] | 26116096 [25935872, 26296320] |
| affine | 4096 | False | prepared-point-evaluation | 176367 [173821, 178571] | 196608 [196608, 196608] | 1 [1, 1] | 0 [0, 0] | 196608 [196608, 196608] | 26050560 [26034176, 26230784] |
| affine | 4096 | False | prepared-point-total | 176888 [175583, 180883] | 197728 [197728, 197728] | 3 [3, 3] | 0 [0, 0] | 196608 [196608, 196608] | 25985024 [25772032, 26263552] |
| affine | 4096 | True | original-constants | 158.3 [154.1, 158.3] | 0 [0, 0] | 0 [0, 0] | 0 [0, 0] | 0 [0, 0] | 24133632 [24018944, 24133632] |
| affine | 4096 | True | prepared-construction | 737.5 [654.1, 1112] | 1120 [1120, 1120] | 2 [2, 2] | 0 [0, 0] | 0 [0, 0] | 24150016 [24051712, 24199168] |
| affine | 4096 | True | original-total | 543788 [540912, 587546] | 196608 [196608, 196608] | 1 [1, 1] | 0 [0, 0] | 196608 [196608, 196608] | 26034176 [25985024, 26132480] |
| affine | 4096 | True | current-total | 542483 [539129, 555654] | 196608 [196608, 196608] | 1 [1, 1] | 0 [0, 0] | 196608 [196608, 196608] | 25985024 [25870336, 26148864] |
| affine | 4096 | True | prepared-evaluation | 142104 [138467, 144946] | 196608 [196608, 196608] | 1 [1, 1] | 0 [0, 0] | 196608 [196608, 196608] | 25903104 [25853952, 26443776] |
| affine | 4096 | True | prepared-total | 142075 [141721, 142571] | 197728 [197728, 197728] | 3 [3, 3] | 0 [0, 0] | 196608 [196608, 196608] | 26132480 [25968640, 26214400] |
| affine | 4096 | True | prepared-point-evaluation | 174917 [172512, 175938] | 196608 [196608, 196608] | 1 [1, 1] | 0 [0, 0] | 196608 [196608, 196608] | 26099712 [25935872, 26214400] |
| affine | 4096 | True | prepared-point-total | 176517 [174429, 196388] | 197728 [197728, 197728] | 3 [3, 3] | 0 [0, 0] | 196608 [196608, 196608] | 26116096 [25935872, 26148864] |
| smooth | 1 | False | original-constants | 170.8 [162.5, 212.5] | 0 [0, 0] | 0 [0, 0] | 0 [0, 0] | 0 [0, 0] | 24903680 [24854528, 24903680] |
| smooth | 1 | False | prepared-construction | 3317 [2917, 4321] | 115968 [115968, 115968] | 4 [4, 4] | 111132 [111132, 111132] | 0 [0, 0] | 25182208 [25018368, 25231360] |
| smooth | 1 | False | original-total | 1046 [1017, 1150] | 48 [48, 48] | 1 [1, 1] | 0 [0, 0] | 48 [48, 48] | 24952832 [24920064, 25165824] |
| smooth | 1 | False | current-total | 1188 [1183, 1221] | 48 [48, 48] | 1 [1, 1] | 0 [0, 0] | 48 [48, 48] | 25034752 [25001984, 25051136] |
| smooth | 1 | False | prepared-evaluation | 754.2 [725, 841.6] | 48 [48, 48] | 1 [1, 1] | 111132 [111132, 111132] | 48 [48, 48] | 25067520 [25034752, 25182208] |
| smooth | 1 | False | prepared-total | 4029 [3892, 4092] | 116016 [116016, 116016] | 5 [5, 5] | 111132 [111132, 111132] | 48 [48, 48] | 25149440 [25100288, 25165824] |
| smooth | 1 | False | prepared-point-evaluation | 837.5 [833.4, 925] | 48 [48, 48] | 1 [1, 1] | 111132 [111132, 111132] | 48 [48, 48] | 25001984 [24969216, 25083904] |
| smooth | 1 | False | prepared-point-total | 4688 [4617, 7675] | 116016 [116016, 116017] | 5 [5, 5] | 111132 [111132, 111132] | 48 [48, 48] | 25427968 [25116672, 25477120] |
| smooth | 1 | True | original-constants | 179.1 [175, 187.5] | 0 [0, 0] | 0 [0, 0] | 0 [0, 0] | 0 [0, 0] | 25231360 [25034752, 25280512] |
| smooth | 1 | True | prepared-construction | 3800 [3688, 5450] | 115968 [115968, 115968] | 4 [4, 4] | 111132 [111132, 111132] | 0 [0, 0] | 25198592 [25001984, 25477120] |
| smooth | 1 | True | original-total | 1267 [1154, 1412] | 48 [48, 48] | 1 [1, 1] | 0 [0, 0] | 48 [48, 48] | 25034752 [25018368, 25165824] |
| smooth | 1 | True | current-total | 1300 [1283, 1467] | 48 [48, 48] | 1 [1, 1] | 0 [0, 0] | 48 [48, 48] | 25067520 [24788992, 25100288] |
| smooth | 1 | True | prepared-evaluation | 995.8 [912.5, 1033] | 48 [48, 48] | 1 [1, 1] | 111132 [111132, 111132] | 48 [48, 48] | 25100288 [24887296, 25100288] |
| smooth | 1 | True | prepared-total | 4283 [3946, 7117] | 116016 [116016, 116016] | 5 [5, 5] | 111132 [111132, 111132] | 48 [48, 48] | 25198592 [24952832, 25444352] |
| smooth | 1 | True | prepared-point-evaluation | 1046 [1021, 1158] | 48 [48, 48] | 1 [1, 1] | 111132 [111132, 111132] | 48 [48, 48] | 25034752 [24920064, 25067520] |
| smooth | 1 | True | prepared-point-total | 4479 [4425, 4800] | 116016 [116016, 116016] | 5 [5, 5] | 111132 [111132, 111132] | 48 [48, 48] | 25198592 [25067520, 25395200] |
| smooth | 16 | False | original-constants | 204.2 [191.6, 204.2] | 0 [0, 0] | 0 [0, 0] | 0 [0, 0] | 0 [0, 0] | 25100288 [24952832, 25133056] |
| smooth | 16 | False | prepared-construction | 4075 [3904, 6446] | 115968 [115968, 115969] | 4 [4, 4] | 111132 [111132, 111132] | 0 [0, 0] | 25214976 [25083904, 25427968] |
| smooth | 16 | False | original-total | 10833 [10038, 11692] | 768 [768, 768] | 1 [1, 1] | 0 [0, 0] | 768 [768, 768] | 24969216 [24887296, 25051136] |
| smooth | 16 | False | current-total | 10358 [10275, 11192] | 768 [768, 769] | 1 [1, 1] | 0 [0, 0] | 768 [768, 768] | 25067520 [24920064, 25411584] |
| smooth | 16 | False | prepared-evaluation | 9462 [9183, 9475] | 768 [768, 768] | 1 [1, 1] | 111132 [111132, 111132] | 768 [768, 768] | 25034752 [25001984, 25346048] |
| smooth | 16 | False | prepared-total | 12267 [12062, 14462] | 116736 [116736, 116736] | 5 [5, 5] | 111132 [111132, 111132] | 768 [768, 768] | 25198592 [25149440, 25296896] |
| smooth | 16 | False | prepared-point-evaluation | 9479 [9325, 9492] | 768 [768, 768] | 1 [1, 1] | 111132 [111132, 111132] | 768 [768, 768] | 25083904 [24870912, 25149440] |
| smooth | 16 | False | prepared-point-total | 12825 [12812, 13546] | 116736 [116736, 116736] | 5 [5, 5] | 111132 [111132, 111132] | 768 [768, 768] | 25296896 [25247744, 25296896] |
| smooth | 16 | True | original-constants | 200 [191.7, 204.1] | 0 [0, 0] | 0 [0, 0] | 0 [0, 0] | 0 [0, 0] | 24870912 [24870912, 25001984] |
| smooth | 16 | True | prepared-construction | 4450 [4296, 4858] | 115968 [115968, 115968] | 4 [4, 4] | 111132 [111132, 111132] | 0 [0, 0] | 25247744 [25182208, 25427968] |
| smooth | 16 | True | original-total | 14046 [14025, 15450] | 768 [768, 768] | 1 [1, 1] | 0 [0, 0] | 768 [768, 768] | 25149440 [24985600, 25444352] |
| smooth | 16 | True | current-total | 14100 [12608, 15588] | 768 [768, 769] | 1 [1, 1] | 0 [0, 0] | 768 [768, 768] | 25165824 [25100288, 25280512] |
| smooth | 16 | True | prepared-evaluation | 10762 [10475, 11950] | 768 [768, 768] | 1 [1, 1] | 111132 [111132, 111132] | 768 [768, 768] | 25100288 [25001984, 25198592] |
| smooth | 16 | True | prepared-total | 13646 [13358, 18038] | 116736 [116736, 116737] | 5 [5, 5] | 111132 [111132, 111132] | 768 [768, 768] | 25231360 [25051136, 25231360] |
| smooth | 16 | True | prepared-point-evaluation | 12071 [11838, 12521] | 768 [768, 768] | 1 [1, 1] | 111132 [111132, 111132] | 768 [768, 768] | 25133056 [24838144, 25395200] |
| smooth | 16 | True | prepared-point-total | 16700 [14579, 17817] | 116736 [116736, 116737] | 5 [5, 5] | 111132 [111132, 111132] | 768 [768, 768] | 25149440 [24985600, 25329664] |
| smooth | 4096 | False | original-constants | 175 [170.8, 183.3] | 0 [0, 0] | 0 [0, 0] | 0 [0, 0] | 0 [0, 0] | 25198592 [24887296, 25247744] |
| smooth | 4096 | False | prepared-construction | 5800 [3512, 7942] | 115969 [115968, 115969] | 4 [4, 4] | 111132 [111132, 111132] | 0 [0, 0] | 25198592 [25100288, 25378816] |
| smooth | 4096 | False | original-total | 2502525 [2472979, 2579929] | 196609 [196608, 196609] | 1 [1, 1] | 0 [0, 0] | 196608 [196608, 196608] | 26558464 [26148864, 26722304] |
| smooth | 4096 | False | current-total | 2555167 [2487396, 2556767] | 196609 [196608, 196609] | 1 [1, 1] | 0 [0, 0] | 196608 [196608, 196608] | 26525696 [26443776, 26525696] |
| smooth | 4096 | False | prepared-evaluation | 2023404 [2015054, 2088650] | 196609 [196609, 196609] | 1 [1, 1] | 111132 [111132, 111132] | 196608 [196608, 196608] | 26361856 [26247168, 26673152] |
| smooth | 4096 | False | prepared-total | 2042725 [2020629, 2063004] | 312576 [312576, 312577] | 5 [5, 5] | 111132 [111132, 111132] | 196608 [196608, 196608] | 27377664 [27230208, 27590656] |
| smooth | 4096 | False | prepared-point-evaluation | 2078438 [2072704, 2087279] | 196609 [196608, 196609] | 1 [1, 1] | 111132 [111132, 111132] | 196608 [196608, 196608] | 26492928 [26411008, 26591232] |
| smooth | 4096 | False | prepared-point-total | 2085888 [2075262, 2091938] | 312577 [312576, 312577] | 5 [5, 5] | 111132 [111132, 111132] | 196608 [196608, 196608] | 27443200 [27279360, 27475968] |
| smooth | 4096 | True | original-constants | 175 [158.4, 179.2] | 0 [0, 0] | 0 [0, 0] | 0 [0, 0] | 0 [0, 0] | 25067520 [24952832, 25329664] |
| smooth | 4096 | True | prepared-construction | 4083 [3092, 6775] | 115968 [115968, 115968] | 4 [4, 4] | 111132 [111132, 111132] | 0 [0, 0] | 25378816 [25296896, 25427968] |
| smooth | 4096 | True | original-total | 3199683 [3176575, 3248546] | 196608 [196608, 196609] | 1 [1, 1] | 0 [0, 0] | 196608 [196608, 196608] | 26361856 [26148864, 26476544] |
| smooth | 4096 | True | current-total | 3197154 [3189975, 3222783] | 196608 [196608, 196609] | 1 [1, 1] | 0 [0, 0] | 196608 [196608, 196608] | 26476544 [26361856, 26591232] |
| smooth | 4096 | True | prepared-evaluation | 2721375 [2708025, 2728879] | 196609 [196609, 196609] | 1 [1, 1] | 111132 [111132, 111132] | 196608 [196608, 196608] | 26624000 [26574848, 26689536] |
| smooth | 4096 | True | prepared-total | 2743604 [2713296, 2840742] | 312577 [312576, 312577] | 5 [5, 5] | 111132 [111132, 111132] | 196608 [196608, 196608] | 27410432 [27312128, 27426816] |
| smooth | 4096 | True | prepared-point-evaluation | 2769504 [2755462, 2810425] | 196609 [196608, 196609] | 1 [1, 1] | 111132 [111132, 111132] | 196608 [196608, 196608] | 26804224 [26361856, 26869760] |
| smooth | 4096 | True | prepared-point-total | 2757750 [2757721, 2765675] | 312576 [312576, 312577] | 5 [5, 5] | 111132 [111132, 111132] | 196608 [196608, 196608] | 27492352 [27475968, 27672576] |
| large | 1 | False | original-constants | 179.2 [175, 200] | 0 [0, 0] | 0 [0, 0] | 0 [0, 0] | 0 [0, 0] | 30097408 [29999104, 30195712] |
| large | 1 | False | prepared-construction | 94925 [87529, 97417] | 828672 [828672, 828694] | 4 [4, 4] | 827052 [827052, 827052] | 0 [0, 0] | 30965760 [30916608, 33619968] |
| large | 1 | False | original-total | 941.6 [895.9, 1042] | 48 [48, 48] | 1 [1, 1] | 0 [0, 0] | 48 [48, 48] | 30081024 [29081600, 30113792] |
| large | 1 | False | current-total | 1104 [1021, 1508] | 48 [48, 48] | 1 [1, 1] | 0 [0, 0] | 48 [48, 48] | 30179328 [30031872, 30195712] |
| large | 1 | False | prepared-evaluation | 750 [612.5, 829.2] | 48 [48, 48] | 1 [1, 1] | 827052 [827052, 827052] | 48 [48, 48] | 30113792 [29245440, 30261248] |
| large | 1 | False | prepared-total | 88796 [84746, 97008] | 828720 [828720, 828731] | 5 [5, 5] | 827052 [827052, 827052] | 48 [48, 48] | 33390592 [31129600, 33521664] |
| large | 1 | False | prepared-point-evaluation | 666.7 [641.7, 733.3] | 48 [48, 48] | 1 [1, 1] | 827052 [827052, 827052] | 48 [48, 48] | 30310400 [30081024, 30457856] |
| large | 1 | False | prepared-point-total | 94433 [86892, 100338] | 828720 [828720, 828731] | 5 [5, 5] | 827052 [827052, 827052] | 48 [48, 48] | 33439744 [32538624, 34308096] |
| large | 1 | True | original-constants | 175 [166.7, 179.2] | 0 [0, 0] | 0 [0, 0] | 0 [0, 0] | 0 [0, 0] | 30179328 [30097408, 30195712] |
| large | 1 | True | prepared-construction | 94254 [93767, 98771] | 828672 [828672, 828683] | 4 [4, 4] | 827052 [827052, 827052] | 0 [0, 0] | 32882688 [32079872, 33767424] |
| large | 1 | True | original-total | 1188 [1162, 1346] | 48 [48, 48] | 1 [1, 1] | 0 [0, 0] | 48 [48, 48] | 30179328 [29999104, 30195712] |
| large | 1 | True | current-total | 1354 [1317, 1667] | 48 [48, 48] | 1 [1, 1] | 0 [0, 0] | 48 [48, 48] | 30195712 [30081024, 30310400] |
| large | 1 | True | prepared-evaluation | 1017 [970.8, 1025] | 48 [48, 48] | 1 [1, 1] | 827052 [827052, 827052] | 48 [48, 48] | 30064640 [29966336, 30294016] |
| large | 1 | True | prepared-total | 101662 [89958, 102300] | 828720 [828720, 828721] | 5 [5, 5] | 827052 [827052, 827052] | 48 [48, 48] | 32620544 [31932416, 33832960] |
| large | 1 | True | prepared-point-evaluation | 1012 [966.6, 1079] | 48 [48, 48] | 1 [1, 1] | 827052 [827052, 827052] | 48 [48, 48] | 30195712 [30097408, 30228480] |
| large | 1 | True | prepared-point-total | 89950 [88804, 102979] | 828720 [828720, 828720] | 5 [5, 5] | 827052 [827052, 827052] | 48 [48, 48] | 31735808 [31014912, 33357824] |
| large | 16 | False | original-constants | 187.5 [166.7, 208.4] | 0 [0, 0] | 0 [0, 0] | 0 [0, 0] | 0 [0, 0] | 30081024 [30064640, 30392320] |
| large | 16 | False | prepared-construction | 87629 [84671, 90125] | 828672 [828672, 828683] | 4 [4, 4] | 827052 [827052, 827052] | 0 [0, 0] | 31850496 [31064064, 33505280] |
| large | 16 | False | original-total | 10958 [10062, 11917] | 768 [768, 768] | 1 [1, 1] | 0 [0, 0] | 768 [768, 768] | 30228480 [30130176, 30244864] |
| large | 16 | False | current-total | 10583 [10408, 11112] | 768 [768, 768] | 1 [1, 1] | 0 [0, 0] | 768 [768, 768] | 30244864 [30081024, 30277632] |
| large | 16 | False | prepared-evaluation | 9683 [8350, 9921] | 768 [768, 768] | 1 [1, 1] | 827052 [827052, 827052] | 768 [768, 768] | 30113792 [30097408, 30310400] |
| large | 16 | False | prepared-total | 101367 [96138, 102475] | 829440 [829440, 829440] | 5 [5, 5] | 827052 [827052, 827052] | 768 [768, 768] | 31784960 [30932992, 32030720] |
| large | 16 | False | prepared-point-evaluation | 8554 [8438, 8600] | 768 [768, 768] | 1 [1, 1] | 827052 [827052, 827052] | 768 [768, 768] | 30277632 [30244864, 30294016] |
| large | 16 | False | prepared-point-total | 98779 [94917, 99283] | 829440 [829440, 829441] | 5 [5, 5] | 827052 [827052, 827052] | 768 [768, 768] | 31014912 [30998528, 32768000] |
| large | 16 | True | original-constants | 158.3 [133.4, 175] | 0 [0, 0] | 0 [0, 0] | 0 [0, 0] | 0 [0, 0] | 29966336 [29147136, 30113792] |
| large | 16 | True | prepared-construction | 93558 [93288, 106325] | 828672 [828672, 828683] | 4 [4, 4] | 827052 [827052, 827052] | 0 [0, 0] | 32571392 [31916032, 32899072] |
| large | 16 | True | original-total | 13508 [13371, 14412] | 768 [768, 768] | 1 [1, 1] | 0 [0, 0] | 768 [768, 768] | 30130176 [30081024, 30359552] |
| large | 16 | True | current-total | 14446 [13792, 15025] | 768 [768, 768] | 1 [1, 1] | 0 [0, 0] | 768 [768, 768] | 30277632 [30261248, 30457856] |
| large | 16 | True | prepared-evaluation | 11658 [10942, 12596] | 768 [768, 768] | 1 [1, 1] | 827052 [827052, 827052] | 768 [768, 768] | 30162944 [30130176, 30212096] |
| large | 16 | True | prepared-total | 108346 [98083, 115042] | 829440 [829440, 829440] | 5 [5, 5] | 827052 [827052, 827052] | 768 [768, 768] | 31686656 [31031296, 31883264] |
| large | 16 | True | prepared-point-evaluation | 11200 [11008, 12125] | 768 [768, 768] | 1 [1, 1] | 827052 [827052, 827052] | 768 [768, 768] | 30261248 [30228480, 30425088] |
| large | 16 | True | prepared-point-total | 108362 [95479, 118500] | 829440 [829440, 829440] | 5 [5, 5] | 827052 [827052, 827052] | 768 [768, 768] | 33390592 [31244288, 33505280] |
| large | 4096 | False | original-constants | 179.2 [170.8, 179.2] | 0 [0, 0] | 0 [0, 0] | 0 [0, 0] | 0 [0, 0] | 30228480 [30195712, 30261248] |
| large | 4096 | False | prepared-construction | 91483 [88421, 114879] | 828672 [828672, 828672] | 4 [4, 4] | 827052 [827052, 827052] | 0 [0, 0] | 33636352 [30932992, 34308096] |
| large | 4096 | False | original-total | 2515662 [2493529, 2551512] | 196608 [196608, 196608] | 1 [1, 1] | 0 [0, 0] | 196608 [196608, 196608] | 30294016 [30048256, 30310400] |
| large | 4096 | False | current-total | 2522004 [2501883, 2557717] | 196608 [196608, 196608] | 1 [1, 1] | 0 [0, 0] | 196608 [196608, 196608] | 29966336 [29458432, 30326784] |
| large | 4096 | False | prepared-evaluation | 2017538 [2015679, 2034667] | 196608 [196608, 196608] | 1 [1, 1] | 827052 [827052, 827052] | 196608 [196608, 196608] | 30081024 [29392896, 30113792] |
| large | 4096 | False | prepared-total | 2093792 [2092504, 2107079] | 1025280 [1025280, 1025291] | 5 [5, 5] | 827052 [827052, 827052] | 196608 [196608, 196608] | 31490048 [31408128, 31604736] |
| large | 4096 | False | prepared-point-evaluation | 2080296 [2069025, 2090571] | 196608 [196608, 196609] | 1 [1, 1] | 827052 [827052, 827052] | 196608 [196608, 196608] | 30310400 [30294016, 30326784] |
| large | 4096 | False | prepared-point-total | 2115904 [2105138, 2150571] | 1025280 [1025280, 1025280] | 5 [5, 5] | 827052 [827052, 827052] | 196608 [196608, 196608] | 31342592 [31244288, 31752192] |
| large | 4096 | True | original-constants | 166.6 [133.4, 183.3] | 0 [0, 0] | 0 [0, 0] | 0 [0, 0] | 0 [0, 0] | 30130176 [30015488, 30228480] |
| large | 4096 | True | prepared-construction | 92471 [87267, 96292] | 828672 [828672, 828672] | 4 [4, 4] | 827052 [827052, 827052] | 0 [0, 0] | 33521664 [32718848, 33718272] |
| large | 4096 | True | original-total | 3190717 [3164250, 3222104] | 196608 [196608, 196608] | 1 [1, 1] | 0 [0, 0] | 196608 [196608, 196608] | 30162944 [30064640, 30244864] |
| large | 4096 | True | current-total | 3178983 [3143121, 3204396] | 196608 [196608, 196608] | 1 [1, 1] | 0 [0, 0] | 196608 [196608, 196608] | 30261248 [30244864, 30490624] |
| large | 4096 | True | prepared-evaluation | 2676971 [2645742, 2733817] | 196608 [196608, 196608] | 1 [1, 1] | 827052 [827052, 827052] | 196608 [196608, 196608] | 30162944 [30146560, 30900224] |
| large | 4096 | True | prepared-total | 2775521 [2714650, 2801479] | 1025280 [1025280, 1025280] | 5 [5, 5] | 827052 [827052, 827052] | 196608 [196608, 196608] | 31457280 [31408128, 31801344] |
| large | 4096 | True | prepared-point-evaluation | 2780400 [2769488, 2806488] | 196608 [196608, 196608] | 1 [1, 1] | 827052 [827052, 827052] | 196608 [196608, 196608] | 30294016 [30179328, 30441472] |
| large | 4096 | True | prepared-point-total | 2775850 [2765583, 2780262] | 1025280 [1025280, 1025281] | 5 [5, 5] | 827052 [827052, 827052] | 196608 [196608, 196608] | 31571968 [31358976, 31916032] |


## Twin transient label-transfer adoption

The complete prepare operation was compared for binary SEG, RTSTRUCT and both,
using nearest or Gaussian interpolation (sigma 1 mm, existing thresholds).
The measured small target is 4×4×3 (48 queries per label pass); large is
24×24×12 (6912 per pass). Both use the same 21³-vector linear inverse field,
111132 logical copied bytes. SEG has two label passes, RTSTRUCT one; combined
has three. Existing Gaussian, fractional, registration, output and error
contracts remain unchanged. Exact output fingerprints include masks,
metadata and provenance except validated generated SOP/series UID identity.
Independent analytic nearest/Gaussian label oracles and the original sparse
small Gaussian no-output control also passed. Construction and Close are
inside each whole-operation timed interval; fixtures/reference checking are
outside equally, but included in native whole-process RSS.

All 24 strategy cells below have three fresh-process runs (72 total). Prepared
large nearest medians improve 11.09%, 15.96%, 14.33% for SEG, RTSTRUCT, both,
with each prepared maximum below its original minimum. Gaussian ranges can
overlap, small profiles can regress, and prepared whole-process RSS medians
are higher in all twelve pairs. Allocation increases roughly 116 KB per
operation; combined large nearest increases 123600 B/op. Thus preparation
is admitted only for a direct inverse deformable registration, nearest binary
SEG or valid nonempty RTSTRUCT, at least 6912 queries in that pass and at most
111132 actual private-vector bytes. One operation reserves that copy budget
before constructing, shares one snapshot across eligible passes, and releases
it after worker completion, including failures/cancellation. Existing output
voxel/sample quotas are checked first. Invalid/empty, fractional/labelmap,
Gaussian, smaller, over-budget and inconsistent dimension/payload inputs
retain the original mapper without copying. Persistent selection is unchanged.

The admission floor is an engineering bound tied to this demonstrated domain,
not a fitted crossover. Larger query counts or smaller fields are conservative
extensions of that bound, not directly measured universal benefits. The copy
reservation excludes metadata, outputs and runtime; it is not a hard RSS limit.
The admission path passed getter1/Twin15 and separate public18; the
72-run table remains the historical original-versus-unconditional-prepared
comparison and is not relabeled as a measurement of the new public policy.

| Target | Objects | Gaussian | Strategy | ns/op | B/op | allocs/op | Copied vectors B | After Close B | Returned mask runs B | Whole-process peak RSS B |
|---|---|---|---|---:|---:|---:|---:|---:|---:|---:|
| small | seg | False | original | 116750 [105708, 161667] | 35736 [35736, 35736] | 278 [278, 278] | 0 [0, 0] | 0 [0, 0] | 384 [384, 384] | 32964608 [32833536, 33095680] |
| small | seg | False | prepared | 112084 [109667, 416958] | 151736 [151736, 151736] | 283 [283, 283] | 111132 [111132, 111132] | 0 [0, 0] | 384 [384, 384] | 33128448 [32997376, 33128448] |
| small | seg | True | original | 135417 [134500, 162458] | 36944 [36944, 36944] | 293 [293, 293] | 0 [0, 0] | 0 [0, 0] | 384 [384, 384] | 32899072 [32849920, 32931840] |
| small | seg | True | prepared | 157833 [146000, 191875] | 152944 [152944, 152944] | 298 [298, 298] | 111132 [111132, 111132] | 0 [0, 0] | 384 [384, 384] | 33062912 [32980992, 33079296] |
| small | rtstruct | False | original | 55792 [55750, 58542] | 23336 [23336, 23336] | 222 [222, 222] | 0 [0, 0] | 0 [0, 0] | 192 [192, 192] | 32768000 [32768000, 32915456] |
| small | rtstruct | False | prepared | 74875 [73791, 83500] | 139336 [139336, 139336] | 227 [227, 227] | 111132 [111132, 111132] | 0 [0, 0] | 192 [192, 192] | 32866304 [32849920, 33046528] |
| small | rtstruct | True | original | 77084 [73291, 88500] | 23960 [23960, 23960] | 230 [230, 230] | 0 [0, 0] | 0 [0, 0] | 192 [192, 192] | 32784384 [32768000, 32833536] |
| small | rtstruct | True | prepared | 96125 [96042, 97583] | 139960 [139960, 139960] | 235 [235, 235] | 111132 [111132, 111132] | 0 [0, 0] | 192 [192, 192] | 32948224 [32882688, 33013760] |
| small | both | False | original | 166542 [152917, 171708] | 54528 [54528, 54528] | 469 [469, 469] | 0 [0, 0] | 0 [0, 0] | 576 [576, 576] | 33079296 [32931840, 33161216] |
| small | both | False | prepared | 146250 [144125, 148708] | 170528 [170528, 170528] | 474 [474, 474] | 111132 [111132, 111132] | 0 [0, 0] | 576 [576, 576] | 33325056 [33112064, 33505280] |
| small | both | True | original | 191667 [188208, 218333] | 56360 [56360, 56360] | 492 [492, 492] | 0 [0, 0] | 0 [0, 0] | 576 [576, 576] | 32899072 [32866304, 33046528] |
| small | both | True | prepared | 201083 [197292, 229791] | 172360 [172360, 172360] | 497 [497, 497] | 111132 [111132, 111132] | 0 [0, 0] | 576 [576, 576] | 33308672 [33161216, 33325056] |
| large | seg | False | original | 5336500 [5268292, 5400459] | 218560 [210400, 225952] | 2313 [2296, 2327] | 0 [0, 0] | 0 [0, 0] | 8448 [8448, 8448] | 34209792 [34144256, 34258944] |
| large | seg | False | prepared | 4744542 [4600875, 5102959] | 334112 [333856, 337216] | 2322 [2320, 2325] | 111132 [111132, 111132] | 0 [0, 0] | 8448 [8448, 8448] | 34390016 [34177024, 34471936] |
| large | seg | True | original | 12821208 [12524750, 12848667] | 221792 [215016, 230008] | 2360 [2338, 2375] | 0 [0, 0] | 0 [0, 0] | 8448 [8448, 8448] | 34127872 [33947648, 34193408] |
| large | seg | True | prepared | 12111000 [12089542, 12604958] | 337624 [329672, 337728] | 2364 [2341, 2364] | 111132 [111132, 111132] | 0 [0, 0] | 8448 [8448, 8448] | 34177024 [34144256, 34242560] |
| large | rtstruct | False | original | 2947375 [2611000, 2949084] | 149600 [145792, 149632] | 1779 [1773, 1781] | 0 [0, 0] | 0 [0, 0] | 4032 [4032, 4032] | 33357824 [33325056, 33521664] |
| large | rtstruct | False | prepared | 2476875 [2317042, 2566625] | 265632 [265632, 265632] | 1786 [1786, 1786] | 111132 [111132, 111132] | 0 [0, 0] | 4032 [4032, 4032] | 33554432 [33472512, 33669120] |
| large | rtstruct | True | original | 7341542 [7308542, 7655417] | 150952 [149032, 151016] | 1798 [1794, 1799] | 0 [0, 0] | 0 [0, 0] | 4032 [4032, 4032] | 33243136 [33226752, 33292288] |
| large | rtstruct | True | prepared | 7064167 [6923542, 7421625] | 267152 [267016, 267152] | 1805 [1804, 1805] | 111132 [111132, 111132] | 0 [0, 0] | 4032 [4032, 4032] | 33570816 [33538048, 33685504] |
| large | both | False | original | 8209959 [8053375, 8217958] | 349296 [342640, 353504] | 4040 [4031, 4053] | 0 [0, 0] | 0 [0, 0] | 12480 [12480, 12480] | 34750464 [34603008, 34783232] |
| large | both | False | prepared | 7033625 [6953000, 7078500] | 472896 [458640, 477552] | 4065 [4036, 4074] | 111132 [111132, 111132] | 0 [0, 0] | 12480 [12480, 12480] | 34865152 [34848768, 34930688] |
| large | both | True | original | 20120334 [19922583, 20331125] | 352912 [346608, 354064] | 4099 [4092, 4100] | 0 [0, 0] | 0 [0, 0] | 12480 [12480, 12480] | 34553856 [34504704, 34816000] |
| large | both | True | prepared | 19649125 [19446042, 20437083] | 470064 [469728, 475392] | 4112 [4105, 4116] | 111132 [111132, 111132] | 0 [0, 0] | 12480 [12480, 12480] | 34979840 [34783232, 35045376] |


## Actual public-policy adoption

The new admission path passed its actual storage getter (1 top-level test) and
15 affected Twin top-level tests on the same native toolchain. The fixed six
public profiles each ran three times (18 complete operations) without retry,
filtering, threshold changes or fixture changes. All full-output and atomicity
oracles passed; full 7907-file source guards remained exact. This separate
public campaign does not replace or relabel the historical 576/72 campaigns.

Compared with the bound original G3 window, the three large nearest public
median times are lower by 7.48%, 14.98%, 8.92% for SEG, RTSTRUCT and both. SEG
and RTSTRUCT ranges overlap the original ranges; only both has all three
public times below the original minimum in this new window. Allocation costs
increase by 115376/115824/114448 B/op. Whole-process RSS medians change by
−81920/+32768/0 bytes. Thus the admitted consumer demonstrates descriptive
median total-time benefit including snapshot and Close, with additional
allocation and no consistent RSS reduction. Historical paired unconditional
comparison already demonstrated all-three range separation; public results
are a separate host window, not an isolated paired statistical claim.

Three excluded controls copy no field: small Gaussian SEG, small nearest
RTSTRUCT and large Gaussian both. Their public time medians are respectively
7.78%, 13.44%, 4.06% higher than historical original; B/op increases 80/80/16
bytes. Their original numerical mapping/output remains unchanged, but the
operation-local policy has overhead and cross-window timing varies. No claim
that excluded operations are faster or that all overhead vanished is made.
The empirical floor/budget was fixed before public measurement and retained
unchanged afterward. There is no claimed crossover, universal speedup,
clinical qualification or blanket memory benefit. Larger-query/smaller-field
engineering extensions remain disclosed rather than directly measured.

Each numeric cell is median [minimum, maximum]. Original rows below are the
preserved historical G3 cells; public rows are actual newly measured calls of
PrepareDerivedTransferWithOptions. policy-admitted bytes (111132 or zero) are
an inferred fixed admission reservation checked by focal tests, not observed
scratch/retention/RSS instrumentation. Output run bytes are actually counted;
all three public repetitions equal their bound original output bytes.

| Target | Objects | Gaussian | Strategy/window | ns/op | B/op | allocs/op | Whole-process peak RSS B |
|---|---|---|---|---:|---:|---:|---:|
| large | seg | False | original / historical G3 | 5336500 [5268292, 5400459] | 218560 [210400, 225952] | 2313 [2296, 2327] | 34209792 [34144256, 34258944] |
| large | seg | False | public policy / new18 | 4937459 [4878708, 6421833] | 333936 [329872, 340880] | 2321 [2314, 2332] | 34127872 [33832960, 34127872] |
| large | rtstruct | False | original / historical G3 | 2947375 [2611000, 2949084] | 149600 [145792, 149632] | 1779 [1773, 1781] | 33357824 [33325056, 33521664] |
| large | rtstruct | False | public policy / new18 | 2505833 [2482458, 2717833] | 265424 [265424, 265712] | 1783 [1783, 1787] | 33390592 [33308672, 33456128] |
| large | both | False | original / historical G3 | 8209959 [8053375, 8217958] | 349296 [342640, 353504] | 4040 [4031, 4053] | 34750464 [34603008, 34783232] |
| large | both | False | public policy / new18 | 7477958 [7385750, 7524500] | 463744 [457280, 474016] | 4044 [4032, 4064] | 34750464 [34586624, 34766848] |
| small | seg | True | original / historical G3 | 135417 [134500, 162458] | 36944 [36944, 36944] | 293 [293, 293] | 32899072 [32849920, 32931840] |
| small | seg | True | public policy / new18 | 145958 [137542, 157417] | 37024 [37024, 37024] | 294 [294, 294] | 32489472 [32374784, 32620544] |
| small | rtstruct | False | original / historical G3 | 55792 [55750, 58542] | 23336 [23336, 23336] | 222 [222, 222] | 32768000 [32768000, 32915456] |
| small | rtstruct | False | public policy / new18 | 63292 [54666, 72041] | 23416 [23416, 23416] | 223 [223, 223] | 32571392 [32342016, 32604160] |
| large | both | True | original / historical G3 | 20120334 [19922583, 20331125] | 352912 [346608, 354064] | 4099 [4092, 4100] | 34553856 [34504704, 34816000] |
| large | both | True | public policy / new18 | 20937334 [20935583, 20981083] | 352928 [345728, 357200] | 4099 [4084, 4115] | 34422784 [34308096, 34521088] |


