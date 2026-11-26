# Bounded DICOM audio waveform profile

`waveform.Open` and `Recording.AudioPCM` decode recorded audio independently of
the application's audio device. The supported SOP Classes are:

| SOP Class | UID suffix | Multiplex groups | Channels | Sampling frequency | Interpretation |
|---|---|---|---|---|---|
| Basic Voice Audio Waveform Storage | `.9.4.1` | One | One or two | 8000 Hz | UB, MB, AB |
| General Audio Waveform Storage | `.9.4.2` | One | One or two | Positive, at most 44100 Hz | SB, SS |

Both require `Modality=AU`. Integer samples use the declared bits-stored range;
UB is centered at the unsigned midpoint. Signed values are scaled to PCM16.
The decoder respects DICOM byte order. G.711 MB/AB require eight allocated and
stored bits. DICOM AB omits the telephone even-bit inversion. Channel order is
the encoded multiplex order; selection uses zero-based indices, including an
explicit reversed stereo order.

The profile rejects padding and nonzero channel time/sample skew or offset.
Malformed counts, channel definitions, frequency, sample interpretation or
truncated data return `*waveform.UnsupportedError` (`errors.Is(err,
waveform.ErrUnsupported)`), with raw fallback metadata. The two classes enter
the default DIMSE supported waveform UID list. Retired trial and industrial
Ultrasound Waveform remain unsupported. Physiologic waveforms are never
automatically converted to recorded audio.

## Reading and ownership

`recording.AudioPCM(ctx, startFrame, frameCount, channels)` returns owned,
frame-interleaved `[]int16`. A nil channel selection means all channels. The
method reads exactly the requested encoded interval from its retained source,
without an envelope index. `MaxAudioRangeFrames=32768`: encoded scratch plus
PCM output is at most 256 KiB for this profile, independent of recording length.
Parser metadata, caller-owned in-memory data and source budgets are separate.
Use deferred Waveform Data or a bounded `SampleSource` for long recordings.

Invalid intervals or duplicate/out-of-range channel indices return
`waveform.ErrAudioRange`. Cancellation or a read/decode failure returns no
partial PCM result. Context checks surround the source read and repeat during
conversion. A synchronous `ReaderAt` call itself cannot be interrupted; its
result is discarded after cancellation. Close the recording after playback
has stopped; `Recording.Close` retains its existing source ownership rules.
Physical channel sensitivity/calibration remains metadata and is not applied
as an output gain. There is no diagnostic or clinical interpretation.

## Independent evidence

The [oracle provenance](../waveform/testdata/README.md) records exhaustive
CPython `audioop` G.711 outputs for all 256 codes of each law. Integer vectors
cover both byte orders, midpoint/sign scaling and channel ordering. Whole and
split interval reads match exactly. Invalid profiles, cancellation, closure,
and a virtual 2 GiB deferred source verify failure and memory-bound behavior;
the virtual source is not a measured 2 GiB physical-file workload.

```sh
go test ./waveform -count=1
go test -race ./waveform -count=1
python waveform/testdata/verify_audio_oracle.py
```

The Python verifier requires CPython 3.12 with `audioop`; Go tests use the
checked-in independent outputs and do not require Python. These are focused
decoder/profile tests, not full IOD conformance or clinical interoperability.

Primary references: [Basic Voice constraints](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_A.34.2.4.html),
[General Audio constraints](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_A.34.10.4.html),
[Waveform encoding](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.10.9.html),
and [ITU-T G.711](https://www.itu.int/rec/T-REC-G.711).
