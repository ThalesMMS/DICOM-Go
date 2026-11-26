package waveform

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
)

func audioTestExtra() []core.Element {
	return []core.Element{stringElement(core.NewTag(0x0008, 0x0060), core.VRCS, "AU")}
}

func TestAudioPCMIndependentCompandingVectors(t *testing.T) {
	for _, law := range []string{"AB", "MB"} {
		t.Run(law, func(t *testing.T) {
			// Oracle: CPython audioop, all 256 codes. For AB the producer
			// adds the telephone even-bit inversion absent in DICOM.
			golden, err := os.ReadFile("testdata/audio-" + law + "-pcm16le.bin")
			if err != nil {
				t.Fatal(err)
			}
			raw := make([]byte, 256)
			for i := range raw {
				raw[i] = byte(i)
			}
			r, err := Open(testFile(uidBasicVoiceAudio, binary.LittleEndian, audioTestExtra(), testGroup{frequency: 8000, bits: 8, interpretation: law, samples: 256, channels: []testChannel{{bitsStored: 8}}, raw: raw}), Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			pcm, err := r.AudioPCM(context.Background(), 0, 256, nil)
			if err != nil || len(pcm)*2 != len(golden) {
				t.Fatal(len(pcm), err)
			}
			for i, v := range pcm {
				want := int16(binary.LittleEndian.Uint16(golden[2*i:]))
				if v != want {
					t.Fatalf("%s code=%02x got=%d want=%d", law, i, v, want)
				}
			}
		})
	}
}

func TestAudioPCMStereoRangesByteOrderAndSelection(t *testing.T) {
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		raw := encodeSigned(order, 16, -32768, 32767, 0, 100, -200, 300, 400, -500)
		r, err := Open(testFile(uidGeneralAudio, order, audioTestExtra(), testGroup{frequency: 44100, bits: 16, interpretation: "SS", samples: 4, channels: []testChannel{{bitsStored: 16}, {bitsStored: 16}}, raw: raw}), Options{})
		if err != nil {
			t.Fatal(err)
		}
		full, err := r.AudioPCM(context.Background(), 0, 4, nil)
		if err != nil || !reflect.DeepEqual(full, []int16{-32768, 32767, 0, 100, -200, 300, 400, -500}) {
			t.Fatal(full, err)
		}
		var split []int16
		for i := int64(0); i < 4; i++ {
			part, e := r.AudioPCM(context.Background(), i, 1, nil)
			if e != nil {
				t.Fatal(e)
			}
			split = append(split, part...)
		}
		if !reflect.DeepEqual(full, split) {
			t.Fatal("range boundaries changed channel alignment")
		}
		selected, e := r.AudioPCM(context.Background(), 1, 2, []int{1, 0})
		if e != nil || !reflect.DeepEqual(selected, []int16{100, 0, 300, -200}) {
			t.Fatal(selected, e)
		}
		g := r.Groups()[0]
		wantDuration := time.Duration(float64(time.Second) * 4 / g.SamplingFrequencyHz)
		if g.SamplingFrequencyHz != 44100 || g.SampleCount != 4 || g.Duration < wantDuration || g.Duration-wantDuration > time.Nanosecond {
			t.Fatal(g)
		}
		if _, e := r.AudioPCM(context.Background(), 0, MaxAudioRangeFrames+1, nil); !errors.Is(e, ErrAudioRange) {
			t.Fatal(e)
		}
		if _, e := r.AudioPCM(context.Background(), 3, 2, nil); !errors.Is(e, ErrAudioRange) {
			t.Fatal(e)
		}
		if _, e := r.AudioPCM(context.Background(), 0, 1, []int{0, 0}); !errors.Is(e, ErrAudioRange) {
			t.Fatal(e)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if p, e := r.AudioPCM(ctx, 0, 1, nil); !errors.Is(e, context.Canceled) || p != nil {
			t.Fatal(p, e)
		}
		if e := r.Close(); e != nil {
			t.Fatal(e)
		}
		if _, e := r.AudioPCM(context.Background(), 0, 1, nil); !errors.Is(e, ErrClosed) {
			t.Fatal(e)
		}
	}
}

func TestAudioPCMIntegerAmplitudeAndInvalidProfiles(t *testing.T) {
	for _, tc := range []struct {
		uid, interpretation string
		raw                 []byte
		want                []int16
	}{{uidBasicVoiceAudio, "UB", []byte{0, 128, 255}, []int16{-32768, 0, 32512}}, {uidGeneralAudio, "SB", []byte{128, 0, 127}, []int16{-32768, 0, 32512}}} {
		r, e := Open(testFile(tc.uid, binary.LittleEndian, audioTestExtra(), testGroup{frequency: 8000, bits: 8, interpretation: tc.interpretation, samples: 3, channels: []testChannel{{bitsStored: 8}}, raw: tc.raw}), Options{})
		if e != nil {
			t.Fatal(e)
		}
		p, e := r.AudioPCM(context.Background(), 0, 3, nil)
		_ = r.Close()
		if e != nil || !reflect.DeepEqual(p, tc.want) {
			t.Fatal(p, e)
		}
	}
	base := testGroup{frequency: 8000, bits: 8, interpretation: "AB", samples: 2, channels: []testChannel{{bitsStored: 8}}, raw: []byte{0, 128}}
	for _, name := range []string{"frequency", "channels", "bits", "skew", "padding", "truncated", "stored", "encoding", "modality", "groups"} {
		t.Run(name, func(t *testing.T) {
			g := base
			g.channels = append([]testChannel(nil), base.channels...)
			extra := audioTestExtra()
			uid := uidBasicVoiceAudio
			switch name {
			case "frequency":
				g.frequency = 44100
			case "channels":
				g.channels = append(g.channels, g.channels[0], g.channels[0])
			case "bits":
				g.bits = 16
			case "skew":
				s := .01
				g.channels[0].timeSkew = &s
			case "padding":
				p := uint64(0)
				g.padding = &p
			case "truncated":
				g.raw = []byte{128}
			case "stored":
				g.channels[0].bitsStored = 7
			case "encoding":
				uid = uidGeneralAudio
			case "modality":
				extra = nil
			}
			groups := []testGroup{g}
			if name == "groups" {
				groups = append(groups, g)
			}
			r, e := Open(testFile(uid, binary.LittleEndian, extra, groups...), Options{})
			var profile *UnsupportedError
			if r != nil || !errors.As(e, &profile) {
				t.Fatal(r, e)
			}
		})
	}
}

type virtualAudioSource struct {
	size, maximumRead int64
	reads             int
	cancel            context.CancelFunc
}

func (s *virtualAudioSource) Size() int64  { return s.size }
func (s *virtualAudioSource) Close() error { return nil }
func (s *virtualAudioSource) ReadAt(p []byte, offset int64) (int, error) {
	if offset < 0 || offset+int64(len(p)) > s.size {
		return 0, io.ErrUnexpectedEOF
	}
	s.reads++
	s.maximumRead = max(s.maximumRead, int64(len(p)))
	for i := range p {
		p[i] = byte((offset + int64(i)) % 256)
	}
	if s.cancel != nil {
		s.cancel()
	}
	return len(p), nil
}

func TestAudioPCMBoundedDeferredSourceAndCancelAfterRead(t *testing.T) {
	const frames = int64(1) << 30
	source := &virtualAudioSource{size: frames * 2}
	r, e := Open(testFile(uidGeneralAudio, binary.LittleEndian, audioTestExtra(), testGroup{frequency: 44100, bits: 16, interpretation: "SS", samples: frames, channels: []testChannel{{bitsStored: 16}}}), Options{SourceFactory: func(int, core.Element) (SampleSource, error) { return source, nil }})
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	if source.reads != 0 {
		t.Fatal("opening scanned audio")
	}
	p, e := r.AudioPCM(context.Background(), frames-128, 128, nil)
	if e != nil || len(p) != 128 || source.maximumRead != 256 || source.reads != 1 {
		t.Fatal(len(p), e, source)
	}
	ctx, cancel := context.WithCancel(context.Background())
	source.cancel = cancel
	if p, e := r.AudioPCM(ctx, 0, 128, nil); !errors.Is(e, context.Canceled) || p != nil {
		t.Fatal(p, e)
	}
}
