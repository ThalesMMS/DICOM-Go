# Selective frame access against DCMTK

This comparison exercises dicom-go's FrameReader and DCMTK's
DcmPixelData::getUncompressedFrame in persistent processes. Its synthetic
Enhanced CT instances contain 64 frames of 256 × 256 signed 16-bit
MONOCHROME2 pixels, with one or two even-length Items per JPEG-LS frame and
one Item per RLE frame. The geometry is a regular 1 mm grid. The fixtures
are generated locally and contain no patient data.

The [recorded Windows amd64 comparison](evidence/windows-amd64/README.md)
includes all timing observations and the JPEG-LS caller-buffer result.

## Reproduce on Windows

Use PowerShell 7, Go and MSYS2 UCRT64 development packages for GCC, DCMTK
and OpenSSL on PATH. The script compiles the adapters, validates the
fixtures, alternates execution order and writes CSV files in a fresh
temporary output directory. Each timing trial uses one new process.

```powershell
pwsh -NoProfile -File examples/frame-access-bench/compare.ps1
```

Pass -VerifyOnly to verify the control cases without running timing
trials. Pass -Baseline the path to a Go adapter built from the same
harness against the previous library. On Windows, from that older
dicom-go-dev module, compile main.go and clock_windows.go from this
directory together with go build -o <path-to-baseline.exe>. Use the same
toolchain and record its version, the DCMTK libraries and their hashes.


```powershell
$env:GOMAXPROCS = '2'
go test -p 1 -count=1 -v ./internal/ui/macstyle/viewer ./internal/ui/windowsstyle -run '^Test(Mac|Windows)StyleSelectiveFramesDuringVolumePreparation$'
```

These tests use the frontend test driver with each style's scheduled
2D navigation snapshot. They check analytically calculated display
samples and physical frame identity on first output, seek and return
while the same instance feeds progressive stored-volume preparation.
They record when preparation was pending and require it to complete.
The test driver qualifies the frontend paths and their shared volume
preparation, with no GPU requirement for this acquired 2D workflow.

## Verification and measurement

- The digest covers row-major raw little-endian output. Both adapters
  independently read Rows, Columns, Samples per Pixel, 16 Bits
  Allocated, 16 Bits Stored, High Bit 15, signed Pixel Representation
  and MONOCHROME2. The Go adapter additionally compares against the
  analytical fixture frame. The DCMTK adapter checks its reported output
  color model. Frame 0, frame 1, frame 63 and a return to frame 0
  each get a separate verification record.
- DCMTK starts with startFragment=0, supplies the returned next
  Item to frame 1, and resets the hint before the jump and return.
  The one-Item layout uses the fixed Item-per-frame rule. Split JPEG-LS
  with a valid Basic Offset Table (BOT) permits the unaided jump.
  Split JPEG-LS with an empty BOT permits sequential access and
  rejects the unaided jump in DCMTK. Dicom-go scans markers during
  indexing when the BOT is empty and retains the frame ranges for future
  seeks. Single-Item RLE also has an empty-BOT control.
- A resident input has all encoded Items in process memory before the
  timer. Deferred input starts with file-backed fragment Items in
  DCMTK and uses a counted ReaderAt in Go. DCMTK's RLE codec drops
  its selected Item after the call. For JPEG-LS, the harness records Item
  loading and calls compact on loaded fragments after each operation to
  restore deferred state. The file and OS cache remain warm, no disk
  access latency is inferred.
- Reader setup, warm-up, hashing, timing reports, JPEG-LS compact and
  the allocation of the reused 131,072-byte destination are outside the
  per-frame windows. Go's allocation counters are process totals for
  an identical frame loop without per-operation clock queries, divided by
  the operation count. DCMTK's
  allocation count and exact RLE read bytes are left empty in the CSV.
  JPEG-LS DCMTK payload bytes count newly loaded Items in deferred mode.
  The CSV's Go payload bytes record ReaderAt output. Resident Go copies
  are described separately in source analysis.
- go-locate times RawFrameLength using the prepared index. go-raw
  times RawFrame, including indexing lookup and assembled/owned encoded
  bytes. go-frame returns an owned output. go-into writes a reusable
  destination like the DCMTK adapter. The four phase times are per-trial
  means. The first phase is frame zero of a warm reused reader.
  Verification logs also record the first access to a newly opened reader.
- Setup counters include Go ReadAt bytes for the index, including the
  empty-BOT marker scan when necessary. They exclude parsing reads
  through Read. With a valid BOT, Go checks that each deferred decode
  reads exactly the requested encoded frame size. DCMTK verification
  records fragments loaded from any other frame for JPEG-LS. Timing
  covers valid BOT cases; empty-BOT controls report verification and
  resource behavior separately.

Pass a CPU-profile path as the last argument to the Go adapter to
profile one mode without mixing the results with timing trials. Example
from the output directory.

```powershell
$env:GOMAXPROCS = '1'
.\frame-access-go.exe fixture\jls-bot.dcm deferred into 600 jls-into.cpu
go tool pprof -top .\frame-access-go.exe jls-into.cpu
```

The existing caller-buffer benchmark additionally measures pure Go on
matching allocating and caller-owned destinations while exercising a
32-frame, 256 × 256 fixture.

```powershell
$env:GOMAXPROCS = '1'
go test -p 1 ./pixeldata -run '^$' -bench '^BenchmarkFrameReaderCallerBuffers/(rle-direct|jls-copy|jls-direct)/frame$' -benchmem -count 5 -benchtime 200ms
```

DCMTK API and source paths consulted are the
[getUncompressedFrame contract](https://github.com/DCMTK/dcmtk/blob/a435c17e0fb44ffbabfca5a377a8491e8838b207/dcmdata/include/dcmtk/dcmdata/dcpixel.h#L655),
[fragment lookup](https://github.com/DCMTK/dcmtk/blob/a435c17e0fb44ffbabfca5a377a8491e8838b207/dcmdata/libsrc/dccodec.cc#L238),
[RLE decoder](https://github.com/DCMTK/dcmtk/blob/a435c17e0fb44ffbabfca5a377a8491e8838b207/dcmdata/libsrc/dcrleccd.cc#L490)
and [JPEG-LS decoder](https://github.com/DCMTK/dcmtk/blob/a435c17e0fb44ffbabfca5a377a8491e8838b207/dcmjpls/libsrc/djcodecd.cc#L335).
