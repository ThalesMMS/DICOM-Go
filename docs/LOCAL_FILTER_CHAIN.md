# Local filter chains on demand

`imagefilter.NewLocalChain` composes exactly two or three existing bilateral/NLM filters for a requested XYZ output region. It is an opt-in CPU preprocessing API with an owned result and a bounded cache. It does not change the dense filters or add a viewer adapter. See [regional filtering](REGIONAL_FILTER_STREAMING.md) for the reused kernels' border, NLM centre-grid and scratch contracts.

Each stage uses original global dimensions and spacing while its buffers use compact strides. Its input region is the clipped expansion of the required output region by its own halo. Propagating those regions backward supplies transitive halos; intermediate float32 conversion and the global NLM block grid/terminal centres match sequential dense filtering. Tile hints partition each stage without restarting the grid at ROI boundaries. The tests compare every float32 bit across anisotropic impulses, borders, single-axis grids, off-grid block centres, two/three-stage permutations, worker counts and partitions.

Only `BilateralOptions` and `NonLocalMeansOptions` values or pointers are accepted. The constructor and `SetStage` copy parameters and normalize documented scalar defaults and workers. Identical snapshots after that normalization are a no-op. Radius vectors preserve the caller's millimetre representation: changing all-zero NLM radius defaults to explicit equivalent radii can cancel active work and invalidate descendants even when the current kernel produces identical bits. This preserves the parameter snapshot rather than adding a general semantic kernel identity. Other changes likewise invalidate that stage and its descendants. Upstream entries remain valid; enlarging a halo can nevertheless require a different exact upstream region, causing a cache miss. Matching containing regions and merging partially overlapping requests are outside this profile.

Median is refused because its current histogram/selection semantics require a global source classification pass. Recursive Gaussian, global-statistics operations and N4 are also refused. To use one, materialize its complete output outside the chain and bind that immutable volume as a new source. This is an explicit caller-owned barrier, with external cost and storage; the chain does not pretend that these operations have a finite local halo.

`LocalChainIdentity` binds caller-supplied source and geometry IDs, a generation, dimensions and spacing. Keys include that complete identity, the exact output region and parameter versions through the cached stage. Identity cannot be inferred from a reader pointer. The caller must keep a binding immutable and use `ReplaceSource` for content, geometry or generation changes. Every rebind clears all dependent entries, including an explicit rebind with reused IDs. Rebinding cancels and fences an active request; the caller must await that request before mutating or closing its old backing source. `Close` cancels and fences active computation, drops cache and source references on the handle, and does not close caller handles. Active snapshots remain alive until their request joins.

`LocalChainOptions.MaxWorkingBytes` reserves one conservative logical bound before source I/O or request data allocation: configured cache payload and metadata, the complete transitive source window, every stage output, every regional kernel reservation and the detached requested result. Summing stage reservations intentionally overestimates their sequential coexistence. A cache reference held by an active request remains charged even if another goroutine clears its entry. Kernel parameter arrays, input/output tile arrays and worker scratch are already part of each regional reservation. A failed admission does no source I/O. `MaxReadBytes`, when nonzero, also bounds the single composed source-window read. Tile hints are reduced by each regional plan; this first profile can reject a conservative aggregate plan that a more complex lifetime planner could fit.

Zero working bytes selects 256 MiB. Zero cache bytes disables caching; enabled caches default to eight entries and admit at most 128. Payload and entry-count limits are independent, with deterministic LRU eviction. Complete intermediate buffers transfer into the cache without an additional payload copy. `CacheBytes()` reports exact currently retained payload. The quota excludes caller-owned source storage, results retained after return, reader internals, Go allocator/runtime overhead and goroutine stacks. It is a logical array bound, not a process RSS limit. Serial context-aware request admission bounds active chain workspace; canceled queued requests allocate no request arrays. Holding many returned results is an external caller cost.

All stages write only into private buffers. A read/kernel/cancellation error publishes neither a result nor newly completed cache entries. A source or parameter change fences both return publication and cache insertion. Existing valid upstream entries survive failed descendant work. `Run` returns `nil` on failure and an owned x-fastest `Data` slice on success; mutating it never changes source or cache. There is no caller writer transaction to partially commit. Readers must honor context, fully populate each requested buffer and release it after return; the chain cannot interrupt a reader that blocks without honoring cancellation.

For example, bind an immutable source whose dimensions and spacing agree with the reader, choose explicit source/geometry IDs, then request a region:

```go
source := imagefilter.LocalChainSource{
    Identity: imagefilter.LocalChainIdentity{
        SourceID: sourceHash, GeometryID: geometryHash, Generation: generation,
        Dims: volume.Dims, SpacingMM: volume.SpacingMM,
    },
    Reader: volume,
}
chain, err := imagefilter.NewLocalChain(source, []imagefilter.SlabFilter{
    imagefilter.BilateralOptions{SigmaMM: [3]float64{.3, .6, .9}, RangeSigma: 10, Workers: 2},
    imagefilter.NonLocalMeansOptions{NoiseSigma: 5, PatchRadiusMM: [3]float64{.7, 0, 0}, SearchRadiusMM: [3]float64{.7, 1.3, 0}, BlockStep: 2, Workers: 2},
}, imagefilter.LocalChainOptions{MaxWorkingBytes: 8 << 20, CacheBytes: 64 << 10})
if err != nil { return err }
defer chain.Close()
result, err := chain.Run(ctx, imagefilter.Region{
    Origin: [3]int{53, 25, 3}, Size: [3]int{16, 12, 2},
})
```


Replay focused contracts and opt-in measurement from `dicom-go`:

```sh
GOMAXPROCS=2 go test -p 2 ./imagefilter -run '^TestLocalChain' -count=1
GOMAXPROCS=2 go test -p 2 -race ./imagefilter -run '^TestLocalChain' -count=1
GOMAXPROCS=2 go test -p 2 -c -o /tmp/local-chain.test ./imagefilter
GOMAXPROCS=2 /tmp/local-chain.test -test.run '^$' -test.bench '^BenchmarkLocalChainCampaign/' -test.benchtime 200ms -test.count 3 -test.benchmem
LOCAL_CHAIN_RESOURCE_PHASE=cold_roi GOMAXPROCS=2 /usr/bin/time -l /tmp/local-chain.test -test.run '^TestLocalChainMeasuredResources$' -test.count 1 -test.v
```

Use each of `materialized`, `cold_roi`, `warm_roi`, `tail_edit_roi` and `whole_demand` for the original resource profiles, plus `materialized_whole` for the separately preregistered matched-output supplement. The ROI profile passed the frozen gates; the matched whole-demand profile measured more time/allocation bytes than dense whole materialization, so it does not qualify whole-volume routing. `TestLocalChainMeasuredResources` explicitly skips without its environment variable. The benchmark performs a dense bit preflight for every selected cell; the separate resource process avoids that oracle allocation so its RSS describes fixture/runtime/setup and the chosen phase, including warmup where required. Darwin `/usr/bin/time -l` reports maximum RSS in bytes; use the platform's units when replaying elsewhere.
