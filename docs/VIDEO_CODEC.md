# Bounded AVC decode and DICOM transcode

The video package supplies explicit AVC decoder and encoder interfaces.
DecodeAVCMP4 and TranscodeAVCMP4 require a caller-owned backend. The optional
[videoffmpeg module](../examples/codec-adapters/videoffmpeg/README.md) provides
a command adapter with a pinned executable. The base library and still-image
codec registries retain their existing dependencies and registration behavior.

## Delivered profile

| Operation | Input | Output |
|---|---|---|
| DecodeAVCMP4 | Video Endoscopic, Video Microscopic or Video Photographic DICOM with .102 AVC High Profile through Level 4.1 in one classic MP4 fragment | Owned presentation-order planar YUV420 bytes, 8-bit limited range |
| TranscodeAVCMP4 | Same decoded profile and explicit AllowLossy plus CRF 1–51 | New DICOM instance in the same Video VL SOP Class and .102 transfer syntax |
| TranscodeAVCMP4Path | Same profile from a regular Part 10 file | Atomic publication to a separate destination |
| Existing extraction/remux | Existing MPEG-2, AVC and HEVC media profiles | Existing unchanged-stream extraction/remux contracts |

The codec path accepts a single video track with progressive pictures, even
dimensions, square pixels, fixed configuration and constant presentation spacing.
It verifies the existing MP4 import parser's Level 4.1 restrictions, and checks
the presentation-time lattice when composition offsets are present. Audio,
edits, rotation, changing in-band parameters, fragmentation and unsupported
matrix/range changes are outside this profile. MPEG-2, HEVC and .102.1 decoding
remain outside the codec extension.

DICOM dimensions, frame count, frame time, Frame Increment Pointer, color layout,
precision and transfer syntax must agree with the inspected stream. The decoded
layout is Y followed by U then V within each frame. Chroma has half the width and
height of luma. This layout is a backend contract, independent of still-image
Pixel Data codecs. The only change to InspectSource is a shared internal parser
path. Its existing acceptance remains intact.

## Transcode policy

Transcode requires a caller-specified lossy CRF and keeps the geometry,
presentation rate and stream color description. The shared layer decodes the
source and verifies that the resulting byte count is exact. It independently
inspects and decodes the encoded output before constructing the result.

The source file stays unchanged. A new SOP Instance UID is generated, Image Type
becomes DERIVED/SECONDARY, File Meta is synchronized, and the source reference
and compression derivation are appended through the shared derivation API.
Lossy Image Compression is 01. Known compression history is extended in order.
When the predecessor ratio is unknown, it stays unspecified rather than being
replaced by a measured ratio for only the newest generation. Digital signatures,
encryption and functional-group video refuse because this codec path cannot
preserve their validity.

## Bounds and failures

Zero limits use 256 MiB input, 512 MiB projected sample work, 16 Mi pixels/frame,
1,024 frames and a two-minute operation deadline. Negative limits refuse.
Existing container limits still constrain table entries, NALs and fragments.
The output cap defaults to the input cap. MaxDuration can be shortened, and an
earlier caller deadline wins.

The current API owns a complete decoded frame buffer and an encoded container.
These limits bound admitted work and serialized bytes, not peak process RSS.
A caller-provided backend is responsible for its own internal memory behavior.
The FFmpeg adapter adds one thread, a 128 MiB individual allocation cap, local
protocol restrictions and a process tied to the operation context. Its encoder
uses a private local MP4 spool with a file-size cap. All errors, including
truncated decode, extra output frames, unsupported codec, cancellation and
excess output, discard the result. Path publication writes and syncs a private
temporary file beside the destination, verifies a separate regular target,
checks cancellation, and replaces the destination atomically.

## Evidence

Default video tests cover synthetic 96 by 64 AVC at six frames/25 fps, exact
metadata/frame-byte checks, derivation/history, a Part 10 reread, resource limits,
truncated output, cancellation before and inside the backend, unsupported
transfer syntax and preserving an existing destination.

The opt-in adapter test exercises the actual external decoder and encoder,
checks independent ffprobe metadata/timestamps, and can compare Microsoft's
Media Foundation decoder output byte-for-byte with FFmpeg on Windows.
The fixture and generation information are in
[video/testdata/README.md](../video/testdata/README.md).

The DICOM profile and loss history follow
[PS3.5 8.2.7](https://dicom.nema.org/medical/dicom/current/output/chtml/part05/sect_8.2.7.html)
and [PS3.3 C.7.6](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.7.6.html).
