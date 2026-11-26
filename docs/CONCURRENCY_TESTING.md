# Concurrency testing

The default `make check` remains the fast baseline. Run the focused concurrency
gates independently when changing networking, asynchronous operations,
registries, subscriptions, dispatchers or shutdown behavior:

```sh
make race
make leak
```

`make race` runs the race detector over `net/dicomweb`, `net/ul`, `net/dimse`,
`net/telemetry`, `transfer` and `ups`. `make leak` repeats the selected lifecycle
tests that explicitly wait for handlers, associations, sessions, queues,
dispatchers and deliveries to finish after cancellation, timeout, error and
shutdown. Override `RACE_COUNT`, `LEAK_COUNT` or `LEAK_TIMEOUT` for a longer
local stress run.

Both targets use loopback listeners, in-process peers and synthetic data. They
do not require a PACS, internet service, patient data, display server, native
codec or GPU runtime. External interoperability tests are excluded from the
race target. Run these targets only on a host supported by the Go race detector;
the repository CI uses Linux amd64 and records the expanded package list in the
job summary before testing.

The lifecycle gate intentionally relies on explicit completion channels and
joins instead of polling the process-wide goroutine count. A leaked operation
therefore fails at the bounded test or package timeout without depending on
unrelated runtime goroutines.

## Render row scheduling

CPU renderers in `render` (VR ray casting, the VR iso-surface, MPR, MIP, slab,
CPR and fusion) share one row scheduler in `render/parallel.go`. The calling
goroutine plus helpers from the shared concurrency budget (below), `GOMAXPROCS`
workers when nothing else is running, claim small contiguous row chunks, about
eight per worker, from an atomic cursor. When an object covers only a band of the viewport,
workers that draw cheap rows keep claiming work instead of idling behind a
fixed share. Per-worker scratch, such as the VR gradient cell, is created once
per worker. Every row is rendered exactly once, so the output is byte-identical
to a serial render. A canceled context stops workers before their next row.

When changing the scheduler or a row renderer, run the scheduler, equivalence
and cancellation tests with the race detector, then compare the balanced and
unbalanced benchmarks:

```sh
go test -race ./render -run 'TestParallel|TestRenderVRContextCancels'
go test ./render -run '^$' -bench 'RenderVRUnbalanced|RenderVRRaycast|ParallelIssue314' -count 6
```

## Shared concurrency budget

The CPU-heavy kernels of `imagefilter` (N4 included), `registration`, `mesh`,
`roi` level sets and `render` take their extra goroutines from one
process-wide budget in `internal/parallel`, after the global thread
maximum of ITK's multithreader. Each call works on its calling goroutine and
reserves helpers without waiting; the budget, `GOMAXPROCS-1` helpers by
default, bounds the helpers of all concurrent calls together. A call that
finds the budget taken runs on its own goroutine, so nested and concurrent
calls cannot deadlock, and a helper returns its slot as soon as it runs out of
work. A kernel's `Workers` option still bounds that kernel's own workers.
Items are claimed dynamically from a shared cursor and reductions run over
fixed blocks in order, so results do not depend on how many helpers a call
got: an N4 correction and a run of volume renders started together give the
same voxels and pixels as each alone, while `parallel.Peak` stays within the
budget.

Background calls (`RunBackground`, `DoBackground`), used by volume decoding,
yield to interactive work. A caller marks interactive work with
`render.BeginInteractiveWork`; while it is held, and for 150 ms after, a
background call's helpers return their slots and only its calling goroutine
continues, then it takes helpers again. The calling goroutine never pauses:
interactive work may be waiting for the background result (a 2D reslice
render waits for its volume), and pausing it would stall that render. Every
item still runs exactly once. `BenchmarkInteractiveUnderBackgroundLoad` times
an interactive call of 64 items while a background call keeps the budget
busy: 65 to 76 ms when the background holds its helpers and 2.1 to 2.4 ms when
it yields them (i7-14700K, GOMAXPROCS 28, machine shared with other builds).

```sh
go test -race ./internal/parallel ./render -run 'Budget|Run|Reserve|Nested|Concurrent|Background'
go test ./imagefilter -run TestN4HonorsWorkers
go test ./internal/parallel -run '^$' -bench 'RunOverhead|InteractiveUnderBackgroundLoad'
```
