# Independent recorded audio vectors

These own binary outputs contain PCM16 little endian for encoded characters
0 through 255, one channel, in that order. They were generated and verified
with CPython 3.12.14's independent `audioop` decoder on Windows amd64. No codec
source or external tables are copied. `verify_audio_oracle.py` adapts DICOM AB
characters with XOR 0x55 for the telephone representation consumed by audioop;
MB characters pass through unchanged. The implementation under test does not
use the oracle code or outputs as its decoder.

| File | Bytes | SHA-256 |
|---|---:|---|
| audio-AB-pcm16le.bin | 512 | `7efdb7dabf62897e6ed84ab72a5b157aba20272f5657acef6ce77ad73bc154a7` |
| audio-MB-pcm16le.bin | 512 | `3dab54339e520bb2c924826e3b72a917a2b612e9fd12fc867500f1d983a75827` |

Run `python waveform/testdata/verify_audio_oracle.py` from the library root.
`--generate` regenerates the two files. Python 3.13 removes `audioop`, so use
3.12 for this optional verifier. The Go test reads the outputs directly.
PCM integer reference values and stereo layouts are independently specified
in `audio_test.go`. All fixtures are synthetic and contain no patient audio.
