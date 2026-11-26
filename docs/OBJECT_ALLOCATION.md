# Object and VR allocation costs


The capacity hint is the length of the strictly increasing tag prefix, a proven lower bound on distinct tags. Ordered input reserves its entire size; the scan stops at the first duplicate or descending tag. It never reserves by the raw occurrence count after that point. Minimal hints (0 or 1) use ordinary slice growth, avoiding an additional growth step caused by reserving capacity 1. The existing duplicate, deferred-value, private-creator, charset, item-offset, sequence-cache and copy-on-write behavior is unchanged.

## Measurement


Object results below are medians of three interleaved A/B runs at 100 ms per benchmark. VR and scan microbenchmarks use three 100 ms runs from the earlier isolated campaign; those functions and fixtures did not change afterward. Timings are a local observation from short runs, not a general latency guarantee. B/op measures cumulative allocated bytes, not retained heap.

Stable Object inputs contain 8, 128 or 1024 attributes. Sorted input is increasing, unsorted input is reversed, and dense duplicates cycle through four tags. Element/value construction happens outside timing. Metadata reads parse generated Explicit VR Little Endian Part 10 bytes. Existing GetSequence and ReadFile fixtures are controls.

| Workload | ns/op baseline → candidate | B/op baseline → candidate | allocs/op baseline → candidate |
| --- | ---: | ---: | ---: |
| ReadFile/minimal | 5,878 → 5,732 | 8,424 → 8,349 | 126 → 111 |
| ReadFile/with_pixel_data | 8,999 → 8,765 | 13,128 → 13,117 | 200 → 175 |
| ReadFile/with_pixel_data_streaming_threshold | 9,444 → 9,258 | 13,792 → 13,781 | 204 → 179 |
| ReadFile/with_sequences | 8,010 → 7,679 | 9,960 → 9,872 | 197 → 175 |
| Construct/sorted/attributes_8 | 391.1 → 401 | 1,032 → 1,008 | 6 → 4 |
| Construct/unsorted/attributes_8 | 388.2 → 424.3 | 1,032 → 1,032 | 6 → 6 |
| Construct/dense_duplicates/attributes_8 | 416.2 → 438.1 | 1,000 → 992 | 5 → 4 |
| Construct/sorted/attributes_128 | 11,042 → 5,259 | 33,904 → 17,432 | 21 → 6 |
| Construct/unsorted/attributes_128 | 11,133 → 10,983 | 33,904 → 33,904 | 21 → 21 |
| Construct/dense_duplicates/attributes_128 | 3,595 → 3,667 | 1,000 → 992 | 5 → 4 |
| Construct/sorted/attributes_1024 | 85,074 → 41,390 | 275,328 → 135,744 | 34 → 8 |
| Construct/unsorted/attributes_1024 | 84,249 → 85,955 | 275,328 → 275,328 | 34 → 34 |
| Construct/dense_duplicates/attributes_1024 | 23,376 → 23,716 | 1,000 → 992 | 5 → 4 |
| ReadMetadata/attributes_8 | 6,454 → 6,276 | 8,480 → 8,392 | 144 → 125 |
| ReadMetadata/attributes_128 | 36,610 → 29,591 | 62,089 → 45,312 | 883 → 731 |
| ReadMetadata/attributes_1024 | 248,609 → 195,174 | 453,023 → 311,341 | 6,275 → 5,216 |
| GetSequenceCold/items_32 | 14,982 → 14,720 | 39,760 → 38,992 | 165 → 101 |
| GetSequenceCold/items_256 | 116,389 → 114,117 | 315,984 → 309,840 | 1,285 → 773 |
| GetSequenceRepeated/items_32 | 2,850 → 2,816 | 14,592 → 14,592 | 2 → 2 |
| GetSequenceRepeated/items_256 | 21,443 → 21,488 | 116,992 → 116,992 | 2 → 2 |

Ordered construction at 128/1024 tags allocated about 49%/51% fewer bytes. Metadata reads allocated about 27%/31% fewer bytes and 17% fewer allocations. Cold GetSequence reduced allocations from 165 to 101 and from 1285 to 773; repeated cached calls retained two allocations and the same byte counts. Reversed input retained the baseline allocation/byte counts. Small constructor timings were higher in this run (about 2.5%–9.3% at eight attributes); the change does not improve every workload.

| ParseVR input | ns/op baseline → candidate | B/op baseline → candidate | allocs/op baseline → candidate |
| --- | ---: | ---: | ---: |
| Existing stable string | 5.987 → 2.076 | 0 → 0 | 0 → 0 |
| Temporary string from `[2]byte` | 12.30 → 4.711 | 2 → 0 | 1 → 0 |

The former ParseVR did **not** allocate for an already stable string. The temporary-header benchmark keeps the `[2]byte` conversion in the measured loop and calls each implementation directly so an indirect function call cannot force both strings to escape. The real ReadFile controls capture the complete parser/object effect.

The capacity scan itself allocated zero bytes and zero objects in all cases. Increasing input cost approximately 6.7/94.2/719.6 ns for 8/128/1024 attributes; reversed input stopped after its first comparison at approximately 1.5 ns, and four-tag dense duplicates stopped after the first cycle at approximately 3.5 ns.

Reproduce against separately prepared baseline/candidate snapshots with the same new benchmark files. The baseline needs the pure `elementCapacityHint` helper in its benchmark file solely to compile the scan microbenchmark; its Object constructors remain unchanged. Do not benchmark against a checkout containing unrelated implementation changes.

```sh
GOMAXPROCS=1 go test -run '^$' -bench '^(BenchmarkObjectCapacity1301|BenchmarkReadMetadata1301|BenchmarkGetSequenceCold|BenchmarkGetSequenceRepeated|BenchmarkReadFile)$' -benchmem -benchtime=100ms -count=3 -cpu=1 ./object
GOMAXPROCS=1 go test -run '^$' -bench '^BenchmarkParseVR1301$' -benchmem -benchtime=100ms -count=3 -cpu=1 ./core
GOMAXPROCS=1 go test -run '^$' -bench '^BenchmarkElementCapacityScan1301$' -benchmem -benchtime=100ms -count=3 -cpu=1 ./object
```

For A/B timing, compile first and alternate the baseline and candidate binaries per repetition, rather than running two compilation/benchmark processes together.

## Focused verification

- Exhaustive VR validation covers all 65,536 two-byte codes plus invalid other lengths and the former error text.
- Constructor tests cover last-wins/last-position order, text options, offsets, duplicate private creators, deferred ambiguity and explicit repairs. Ordering/update/removal crosses the lazy order-index threshold. Dense duplicates retain a four-tag order slice rather than raw-input capacity.
- Existing sequence-cache independence/invalidation, charset, parsed deferred replay and private-scope tests also pass.
- `go test -race ./core ./parser ./object` and `go vet ./core ./parser ./object` pass on the isolated candidate.
- `GOTOOLCHAIN=go1.22.12 go test -ldflags=-linkmode=external ./core ./parser ./object` passes. This macOS aborts Go 1.22 parser/object executables made by its internal linker because they lack an `LC_UUID` load command; external linking resolves that host-toolchain limitation.

This is focused library verification and local synthetic measurement. No native Windows runtime, live PACS or full repository suite is claimed.

## Ordered-storage evaluation and decision

Production retains `elements map`, `order`, `orderIndex`, tombstones and the existing sequence templates. The short-term capacity/VR change meets the metadata/cold-sequence allocation acceptance criterion. Two isolated long-term prototypes were evaluated and rejected for lookup regressions above the predeclared limit; neither changed production storage.

The [ordered-storage evaluation](OBJECT_ORDERED_STORAGE_EVALUATION.md) records the candidates, semantic/copy-on-write checks, paired lookup measurements and final decision. Their construction and retained-memory campaigns were not executed after lookup rejection, so no memory gain is claimed for those prototypes.

