# File CLI startup and dictionary lookup

`dcmdump` uses a file diagnostic classifier and a tag parser that depend on
file parsing, dictionaries and transfer syntax primitives. They do not import
DICOM association, DIMSE or pixel codec packages. The complete diagnostic
classifier remains available to tools that use networking or codecs. It keeps
the priority of file, parser, transfer syntax, media, codec and network errors,
including wrapped and combined errors.

The generated standard dictionary looks up sorted tags and UID strings with
binary search. Each keyword has a generated index into the first matching
entry in the same sorted source order used by the previous map initializer.
Tag and UID lookup choose the last equal entry, preserving that initializer's
duplicate identity policy. Case conversion and UID padding normalization are
unchanged. The dictionary source, contextual VR choices and generated edition
remain unchanged.

The generated keyword indexes and tag/UID tables replace eager startup maps.
The contextual VR map still exists. Keyword case conversion can allocate, and
UID category lookup still allocates a caller-owned snapshot and scans the UID
table twice. Binary lookup changes the lookup tradeoff; removing startup maps
does not establish a universal lookup speed improvement.

## Measurement scope


A timer wraps process launch and blocking wait for `/usr/bin/time` plus the
tool. The measured interval excludes the timer's Python initialization,
metadata serialization, monitor polling and group cleanup. It includes
process launch, the `/usr/bin/time` wrapper, tool initialization, parsing and
printing for the selected case, and scheduling. It does not isolate an
internal Go initialization phase.

BSD `/usr/bin/time -l` records the tool's absolute maximum resident set size.
This is whole-process RSS, including the executable, Go runtime, dictionaries,
input, allocator and operating system accounting. It is not live Go heap or a
clinical workload measurement. A separate Darwin process-group guard sums RSS
for the timer, time wrapper and tool every 20 ms. Shared pages may be counted
more than once; sampling can miss short peaks. Its 1 GiB cap is an operational
guard rather than a kernel hard memory ceiling or an application quota.

The campaign stops on any error, missing metric, output difference, source
change, timeout or cap, preserving the partial evidence. Each invocation has
a 5-second limit, the whole run has 120 seconds, and stdout/stderr each have a
4 MiB limit. Each process group must be empty and the direct child reaped
before metrics are admitted or another invocation starts. There are no retries,
omitted cells or partial successful results.

## Results

The single reserved campaign completed all 40 tool calls and 20 pairs in
2.875934 seconds. Whole stdout, stderr and exit status were identical within
each case. All 39 sealed inputs matched before and after the campaign, and
all 40 direct children were reaped with their process groups empty. No cell
was retried or omitted. The candidate's 14 code and test files remained
unchanged through the final documentation update.

The tables report medians and full minimum-to-maximum ranges across all ten
samples in each case. Paired reduction is baseline minus candidate for the
same sample; its median is computed directly from the ten paired differences,
rather than subtracting the two absolute medians.

| Case | Baseline launch + wait, ms | Candidate launch + wait, ms | Paired reduction, ms |
| --- | ---: | ---: | ---: |
| Help | 9.9538745 [9.273500, 333.412208] | 5.789625 [5.438250, 291.660833] | 4.087167 [3.468500, 41.751375] |
| Synthetic Part 10 | 9.8205625 [9.495458, 10.209333] | 5.705417 [5.470750, 6.712209] | 4.0255415 [3.172083, 4.517791] |

| Case | Baseline maximum RSS, MiB | Candidate maximum RSS, MiB | Paired reduction, MiB |
| --- | ---: | ---: | ---: |
| Help | 20.2421875 [19.812500, 20.500000] | 8.953125 [8.921875, 9.109375] | 11.3046875 [10.875000, 11.531250] |
| Synthetic Part 10 | 20.4296875 [19.968750, 20.671875] | 9.203125 [9.125000, 9.375000] | 11.234375 [10.796875, 11.406250] |

Help sample zero had longer absolute durations in both versions: 333.412208
and 291.660833 ms, respectively, for a paired reduction of 41.751375 ms.
That sample is retained in every median and range; the campaign does not
establish its cause. Both cases exceeded the planned paired median reduction
of one millisecond per process and reduced RSS by several MiB. These are
local measurements of the selected invocations, without a statistical
confidence, cold-cache, Windows or clinical-workload claim.

## Compatibility checks

The public lookup oracle produced identical 10,450,121-byte results for all
14,486 source tags and 12 UID categories. Each dictionary was regenerated
twice from its unchanged pinned source, producing bytes identical to the
candidate. The production `dcmdump` dependency inventory on this toolchain
fell from 312 total packages (76 library packages) to 172 (21 library packages),
with no library network or pixel codec package in the final dependency graph.
The executable shrank from 12,115,634 to 9,766,706 bytes. These inventories
and sizes describe the qualified build rather than every platform or build
configuration.

The seven affected dictionary, generator, tag, file and dump packages passed
native tests, race tests, Go 1.22.12 tests and vet. The race and Go 1.22 JSON
logs each contained 109 PASS events. Two packages had no tests, and the one
optional external GDCM corpus test was skipped. The existing complete
diagnostic classifier tests also passed, preserving priority across joined
file, media, codec and network errors.
