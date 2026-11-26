# Opt-in topological fast marching

`render.FastMarchingConstrained` adds candidate admission to the existing
first-order upwind Eikonal solver and indexed trial heap. The original
`FastMarching`, `FastMarchingFixedBuffers`, CPR, minimal paths and ROI level sets
keep their existing APIs and unconstrained behavior. `render` does not depend on
`roi`; no solver or application workflow is duplicated.

## Modes and geometry

| Mode | Contract |
|---|---|
| `FastMarchingTopologyNone` | Delegates directly to `FastMarching`, preserving fields, stopping and errors. Topology quotas do not apply; report reason is `disabled`. |
| `FastMarchingTopologyStrict` | Preserves initial foreground component, handle and enclosed-background cavity counts. |
| `FastMarchingTopologyNoHandles` | Permits distinct-component mergers while preserving handles and cavities. Its cavity rule is conservative: it also refuses filling an existing cavity. |

The constrained domain requires `Nx, Ny >= 2`. `Nz=1` denotes XY 2D and `Nz>=2`
denotes XYZ 3D. All spacings must be positive and finite. The foreground is the
union of closed pixels/voxel cubes: connectivity is 8 in 2D and 26 in 3D.
Background uses 4/6 connectivity, with exterior-of-grid positions treated as
background. Corner contact therefore joins foreground components. Handles are
the first Betti number; cavities are the second Betti number, absent in 2D.
Positive anisotropic scaling preserves these combinatorial invariants while
arrival times use physical spacing.

This contract does not guarantee a well-composed manifold boundary or the genus
of a subsequently generated Marching Cubes surface. It does not repair initial
topology or choose a clinically appropriate topology.

## Initial foreground and speed masks

The distinct seed voxels define the initial foreground. Constrained seeds must
be inside the grid, on positive finite speeds, have the same finite arrival
value and lie at or before any positive `StoppingValue`. Duplicate seeds are
allowed and remain present in the returned seed list. Existing handles or
cavities in the seeds are preserved rather than diagnosed as new events.

Zero, negative, NaN and infinite speeds are unavailable propagation nodes and
background, not foreground. Seeds on these barriers, unequal-time seeds and
malformed geometry produce `ErrFastMarchingInput`. All supplied buffers must
remain stable until return.

Constrained seed trial values remain fixed. Target-based stopping waits until
every distinct seed has frozen, including when floating-point roundoff makes an
updated neighbor's arrival slightly smaller than the supplied seed value. A
successful result always contains the complete declared initial foreground.
The existing `None` seed semantics are retained.

## Admission and incomplete results

A call-owned disjoint-set structure tracks foreground components. For a
non-seed candidate, let `d` be the number of distinct neighboring foreground
roots. Strict requires `d=1`; NoHandles permits `d>=1`.

In 3D, a bounded face-neighbor flood requires one nonempty background component
touching the candidate's faces in its punctured 3×3×3 neighborhood. This
sufficient local condition prevents splitting or removing a global background
component and preserves cavities. It can reject additions that a more expensive
global analysis would allow. In 2D no cavity invariant exists, so this background
test is unnecessary: two distinct arms may merge safely even when their local
background is split.

The exact change in cubical Euler characteristic is computed from newly added
vertices, edges, faces and cube. Since `chi = beta0 - beta1 + beta2`, the exact
component change `1-d` and unchanged cavities preserve handles when
`deltaChi = 1-d`. Strict additionally prevents component mergers. This rule
works for the declared cubical complex, including diagonal contacts.

Rejected candidates are terminal, keep `+Inf` arrival times and are never
reconsidered as later fronts grow. The deterministic rule can leave available
voxels unvisited; it does not claim that no other admissible completion exists.
The report counts rejected candidates and the first failed condition:
`RejectedMerge`, `RejectedBackground` or `RejectedHandle` (Euler).

`AvailableUnreached` counts all positive-finite-speed nodes without a final
arrival time, including disconnected nodes, rejected nodes and early stopping.
`Unavailable` counts barriers. `Complete` means every available node has a final
time; it is not a numerical-convergence claim. Successful stop reasons are
`front-exhausted`, `topology-rejections`, `stopping-value` and `targets`.
Targets retain the existing two-step physical padding and `TargetsReached`.

## Resource bounds and errors

Constrained calls use fixed N-node heap capacities and the existing
`MaxFastMarchingNodes`, `MaxFrozen` and `MaxTrial` limits. The initial seed queue
also obeys `MaxTrial`. Topology defaults are 512 MiB logical bytes and one
billion charged work units. Explicit positive quotas override these defaults.
Logical-byte admission, checked before controller/solver allocation, is:

```text
30*N + 64*suppliedSeeds + 32*targets + 8192
```

The per-node ledger includes borrowed speed values, owned times/state, heap
positions/nodes and component parents/ranks. Seed and target entries include
both borrowed and retained lists; the constant covers bounded local scratch.
This is a conservative logical payload, not a process RSS or cumulative
allocation bound.

Initialization, final state cleanup, target validation and final reporting are
charged per visited voxel/target. Seed unions and candidate checks charge 8192
units for bounded neighborhoods and component/heap operations. Target searches
charge per visited target. Work units bound algorithm visits, not nanoseconds
or processor instructions. Context polling accompanies charged work and
initial validation; a caller deadline bounds wall time. The implementation is
serial and creates no workers or goroutines.

Limits and cancellation return a nil result, never a partial successful arrival
field. `FastMarchingTopologyFailure` carries diagnostic progress and unwraps the
original typed/context error. Failure reasons distinguish `budget`, `canceled`,
`deadline`, `invalid-input` and `error`; every failure report has `Complete=false`.
Unreached/barrier counts can be partial if the final scan fails. Seed/component
counts on failed operations describe the admitted logical foreground rather
than a published partial mask. `None` preserves the original errors exactly.

## Independent tests

The tests use a separate global cubical-cell set for Euler characteristic,
full-neighbor foreground floods and padded face-neighbor background floods.
Analytic rings and shells validate this oracle. Tests cover exhaustive 256 XY
neighborhoods, deterministic 3D neighborhoods including boundaries and corner
contacts, bridge/nearby components, rings, extruded tunnels, initial cavities,
anisotropic spacing, partial availability, duplicate/invalid seeds, stopping,
byte/node/work budgets, deadline/cancellation and concurrent independent calls.
The legacy fast-marching and affected CPR/ROI level-set tests exercise callers.

From the library directory:

```sh
GOMAXPROCS=4 go test -p 2 ./render ./roi \
  -run '^(TestFastMarching.*|TestFindCPRAssistedPath.*|TestLevelSet(SegmentsSyntheticPhantoms|ErrorsAndLimits|CancelsDuringEvolution|SubVolumeIsBoundedInMillimetres|SkipsUnavailableVoxels))$' \
  -count=1
GOMAXPROCS=4 go test -p 2 -race ./render \
  -run '^TestFastMarchingTopology(OwnershipDeterminism|KernelCancellation|CancellationPublicationReport|TargetsRetainInitialSeedsUnderRoundoff)$' \
  -count=1
GOMAXPROCS=4 go test -p 2 ./render -run '^$' \
  -bench '^BenchmarkFastMarchingTopologyCost$' -benchtime=200ms -count=3
```

The benchmark reports operation cost, cumulative allocation and charged work on
a fixed 24×20×5 anisotropic grid. Constrained modes may reach different numbers
of nodes; the benchmark cannot establish equivalent-work improvements or
clinical performance. No CPU 3D rendering, UI exposure or evolving-level-set
topology guarantee is introduced.

## Opt-in ITK reference

The [ITK FastMarchingBase contract](https://docs.itk.org/projects/doxygen/en/stable/classitk_1_1FastMarchingBase.html)
describes Strict and NoHandles using well-composedness. The native API declares
its independent cubical/Euler contract above. Comparisons use identical
procedural domains and initial foreground under isotropic spacing; no equality
of arrival times or rejected masks is assumed.

The [independent C++ producers](../render/testdata/fast_marching_topology_itk/)
invoke ITK 5.4.5 APIs. The primary producer covers 15 cases: 2D bridge/ring and
3D bridge/tunnel/cavity in all three modes. Alive seed masks and adjacent trial
nodes are supplied explicitly, matching the public ITK initialization contract.
The companion diagnostic covers 12 direct/grown bridge cases and reads component
IDs without modifying or overriding the SDK algorithm.

Build in a distinct output directory against an existing ITK 5.4.5 SDK:

```sh
topology_build=$(mktemp -d)
cmake -S render/testdata/fast_marching_topology_itk -B "$topology_build" \
  -DITK_DIR="$ITK_DIR" -DCMAKE_BUILD_TYPE=Release
cmake --build "$topology_build" --parallel 2
"$topology_build/topology1501_oracle" > "$topology_build/cases.json"
"$topology_build/topology1501_diagnostic" > "$topology_build/diagnostic.json"
FAST_MARCHING_TOPOLOGY_ITK_FIXTURE="$topology_build/cases.json" \
FAST_MARCHING_TOPOLOGY_ITK_DIAGNOSTIC="$topology_build/diagnostic.json" \
GOMAXPROCS=4 go test -p 2 ./render \
  -run '^TestFastMarchingTopologyITK(OptIn|DiagnosticProof)$' -count=1 -v
```

Capture the producer commands, exit statuses, SDK/compiler version and hashes
of sources/executables/JSON alongside the Go comparison log. The Go validator
checks the bounded JSON schema, domains, invariants and companion diagnostic;
it cannot authenticate an arbitrary external file as an actual SDK execution.
With no fixture, the opt-in test skips. A supplied fixture without the companion
diagnostic, an altered schema/version or failed invariant is a failure.

The observed ITK 5.4.5 grown five-voxel bridge retains two foreground components
in NoHandles: endpoint component IDs are 1/2, grown neighbor IDs are 0/0 and the
center is refused. The direct three-voxel bridge merges. The comparator requires
this exact version/profile and companion diagnostic while requiring the native
solver to complete the bridge as one component without handles or cavities.
Strict and the other reference cases still enforce their invariants. The 3D
tunnel can differ by six accepted voxels while retaining equal invariants.
These observations do not establish a defect in every SDK version or mask/arrival
parity. Clinical, manifold-surface and UI/level-set qualification remain separate.
