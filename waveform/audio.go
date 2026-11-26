package waveform

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

// MaxAudioRangeFrames bounds both the encoded scratch and PCM result of one
// range query. At two channels the total is at most 256 KiB, independent of
// recording duration. Sources and file-parser metadata have separate budgets.
const MaxAudioRangeFrames = 32768

var ErrAudioRange = errors.New("waveform: invalid audio PCM range")

// IsAudioStorageSOPClass identifies only Basic Voice and General Audio. It
// never turns a physiologic waveform or an ultrasound trial into audio.
func IsAudioStorageSOPClass(uid string) bool {
	uid = strings.TrimSpace(strings.TrimRight(uid, "\x00"))
	return uid == uidBasicVoiceAudio || uid == uidGeneralAudio
}

func audioProfileError(ds *object.Object, uid, reason string) error {
	return &UnsupportedError{Fallback: RawFallback{SOPClassUID: uid, GroupIndex: 0, RawBytes: rawWaveformByteCount(ds), Reason: "audio profile: " + reason}}
}

// Validate the audio IOD layout before a source or channel array is allocated.
// The implemented playback profile requires one aligned multiplex group with
// no padding/skew. Calibration remains metadata; output is recorded PCM.
func validateAudioProfile(ds *object.Object, uid string, items []*object.Object) error {
	fail := func(reason string) error { return audioProfileError(ds, uid, reason) }
	if optionalString(ds, core.NewTag(0x0008, 0x0060)) != "AU" {
		return fail("Modality must be AU")
	}
	if len(items) != 1 {
		return fail("exactly one multiplex group is required")
	}
	i := items[0]
	n, err := requiredPositiveInt(i, tagNumberOfWaveformChannels, "channels")
	if err != nil || n > 2 {
		return fail("one or two audio channels are required")
	}
	f, err := requiredPositiveFloat(i, tagSamplingFrequency, "frequency")
	if err != nil || f > 44100 || (uid == uidBasicVoiceAudio && f != 8000) {
		return fail("Basic Voice requires 8000 Hz; General Audio requires positive frequency at most 44100 Hz")
	}
	interpretation := strings.ToUpper(optionalString(i, tagWaveformSampleInterpretation))
	if err := validateSOPSampleInterpretation(uid, interpretation); err != nil {
		return fail(err.Error())
	}
	if _, present := i.Get(tagWaveformPaddingValue); present {
		return fail("padding samples are outside the audio playback profile")
	}
	channels, ok := i.GetSequence(tagChannelDefinitionSequence)
	if !ok || len(channels) != n {
		return fail("channel definitions must match the encoded channel count")
	}
	for _, ch := range channels {
		if interpretation == "AB" || interpretation == "MB" {
			bits, e := requiredPositiveInt(ch, tagWaveformBitsStored, "companded bits stored")
			if e != nil || bits != 8 {
				return fail("G.711 audio requires 8 bits stored")
			}
		}
		for _, tag := range []core.Tag{tagChannelTimeSkew, tagChannelSampleSkew, tagChannelOffset} {
			if _, present := ch.Get(tag); present {
				v, _, e := lookupOptionalFloat(ch, tag)
				if e != nil || v != 0 {
					return fail("channel skew/offset must be zero for aligned audio")
				}
			}
		}
	}
	return nil
}

// AudioPCM returns signed 16-bit, frame-interleaved PCM in the requested
// channel order (zero-based). It reads only this interval from the retained
// source, builds no envelope index and returns no partial output on failure or
// cancellation. Integer encodings use the declared bits-stored range; UB is
// centered at its unsigned midpoint. G.711 uses the DICOM code representation.
func (r *Recording) AudioPCM(ctx context.Context, start int64, frames int, channels []int) ([]int16, error) {
	if r == nil {
		return nil, ErrClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return nil, ErrClosed
	}
	if err := operationErr(ctx, r.closeCh); err != nil {
		return nil, err
	}
	if !IsAudioStorageSOPClass(r.sopClassUID) || len(r.groups) != 1 {
		return nil, &UnsupportedError{Fallback: RawFallback{SOPClassUID: r.sopClassUID, Reason: "recording is not a supported audio IOD"}}
	}
	g := r.groups[0]
	if channels == nil {
		channels = []int{0}
		if len(g.info.Channels) == 2 {
			channels = []int{0, 1}
		}
	}
	if start < 0 || start > g.info.SampleCount || frames < 0 || frames > MaxAudioRangeFrames || int64(frames) > g.info.SampleCount-start || len(channels) < 1 || len(channels) > 2 {
		return nil, ErrAudioRange
	}
	for j, ch := range channels {
		if ch < 0 || ch >= len(g.info.Channels) || (j > 0 && ch == channels[0]) {
			return nil, ErrAudioRange
		}
	}
	stride := len(g.info.Channels) * g.decoder.width
	raw := make([]byte, frames*stride)
	if len(raw) > 0 {
		n, err := g.source.ReadAt(raw, start*int64(stride))
		if err != nil || n != len(raw) {
			return nil, fmt.Errorf("audio range source: %w", errors.Join(io.ErrUnexpectedEOF, err))
		}
	}
	pcm := make([]int16, frames*len(channels))
	for frame := 0; frame < frames; frame++ {
		if frame%256 == 0 {
			if err := operationErr(ctx, r.closeCh); err != nil {
				return nil, err
			}
		}
		for j, ch := range channels {
			at := frame*stride + ch*g.decoder.width
			value, _, err := g.decoder.decodeBytes(raw[at:at+g.decoder.width], g.info.Channels[ch].BitsStored)
			if err != nil {
				return nil, err
			}
			if g.decoder.law == "" {
				bits := g.info.Channels[ch].BitsStored
				if !g.decoder.signed {
					value -= float64(uint64(1) << (bits - 1))
				}
				value *= float64(uint64(1) << (16 - bits))
			}
			pcm[frame*len(channels)+j] = int16(value)
		}
	}
	if err := operationErr(ctx, r.closeCh); err != nil {
		return nil, err
	}
	return pcm, nil
}

// DICOM A-law omits telephone-network even-bit inversion (PS3.3 C.10.9.1.5).
// The mu-law character retains the G.711 magnitude/sign coding. The equations
// reconstruct segment midpoints; no external codec source or tables are copied.
func decodeDICOMLaw(code byte, law string) int16 {
	if law == "AB" {
		segment := int(code>>4) & 7
		magnitude := int(code&15)*16 + 8
		if segment > 0 {
			magnitude += 256
			magnitude <<= segment - 1
		}
		if code&128 == 0 {
			magnitude = -magnitude
		}
		return int16(magnitude)
	}
	inverted := ^code
	segment := int(inverted>>4) & 7
	magnitude := ((int(inverted&15)*8 + 132) << segment) - 132
	if inverted&128 != 0 {
		magnitude = -magnitude
	}
	return int16(magnitude)
}
