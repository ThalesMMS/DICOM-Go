package waveform

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestReferenceWaveformPart10MultiplexGroups(t *testing.T) {
	for _, syntax := range []transfer.Syntax{
		transfer.ImplicitVRLittleEndian, transfer.ExplicitVRLittleEndian, transfer.ExplicitVRBigEndian,
	} {
		for _, length := range []struct {
			name   string
			policy object.SequenceLengthPolicy
		}{
			{"defined", object.SequenceLengthDefined},
			{"undefined", object.SequenceLengthUndefined},
		} {
			t.Run(syntax.UID+"/"+length.name, func(t *testing.T) {
				order := syntax.ByteOrder
				first := ssTestGroup(3, 2)
				first.frequency, first.label = 250, "First"
				first.channels[0].number, first.channels[1].number = 41, 7
				first.raw = encodeSigned(order, 16, 1200, -257, -701, 321, -32767, 32766)
				second := ssTestGroup(2, 1)
				second.frequency, second.label, second.timeOffsetMS = 500, "Second", 12.5
				second.channels[0].number = 9
				second.raw = encodeSigned(order, 16, 8193, -20480)
				marker := annotationElement("POINT", []uint16{2, 0}, []uint32{2}, nil)
				file := testFile(uidGeneralECG, order, []core.Element{marker}, first, second)
				recording := roundTripWaveformPart10(t, file, syntax, length.policy)

				groups := recording.Groups()
				if len(groups) != 2 || groups[0].Label != "First" || groups[1].Label != "Second" ||
					groups[0].Duration != 12*time.Millisecond || groups[1].Duration != 4*time.Millisecond ||
					groups[1].TimeOffset != 12500*time.Microsecond {
					t.Fatalf("group order or time changed: %+v", groups)
				}
				if firstGroup := groups[0].Channels; len(firstGroup) != 2 || firstGroup[0].Number != 41 || firstGroup[1].Number != 7 {
					t.Fatalf("encoded channel order changed: %+v", firstGroup)
				}
				for _, point := range []struct {
					group, channel int
					at             time.Duration
					want           float64
				}{
					{0, 0, 0, 1200}, {0, 1, 0, -257}, {0, 1, 8 * time.Millisecond, 32766},
					{1, 0, 0, 8193}, {1, 0, 2 * time.Millisecond, -20480},
				} {
					got, err := recording.ValueAt(context.Background(), point.group, point.channel, point.at)
					if err != nil || !got.Valid || got.Time != point.at || got.Raw != point.want {
						t.Fatalf("group %d channel %d at %v = %+v, %v, want %v", point.group, point.channel, point.at, got, err, point.want)
					}
				}
				markers := recording.Annotations()
				if len(markers) != 1 || markers[0].TemporalRangeType != "POINT" ||
					!slices.Equal(markers[0].Channels, []ChannelReference{{GroupNumber: 2, ChannelNumber: 0}}) ||
					!slices.Equal(markers[0].ReferencedSamplePositions, []uint64{2}) {
					t.Fatalf("annotation reference changed: %+v", markers)
				}
			})
		}
	}
}

func TestReferenceWaveformBasicVoiceContainer(t *testing.T) {
	for _, syntax := range []transfer.Syntax{transfer.ImplicitVRLittleEndian, transfer.ExplicitVRLittleEndian} {
		t.Run(syntax.UID, func(t *testing.T) {
			gain, correction, offset := 0.5, 1.0, 0.0
			units := CodedConcept{Value: "V", Scheme: "UCUM", Meaning: "volt"}
			file := testFile(uidBasicVoiceAudio, binary.LittleEndian, audioTestExtra(), testGroup{
				frequency: 8000, bits: 8, interpretation: "UB", samples: 5,
				channels: []testChannel{{
					bitsStored: 8, source: CodedConcept{Value: "voice", Scheme: "99TEST", Meaning: "Synthetic voice"},
					sensitivity: &gain, correction: &correction, baseline: &offset, units: &units,
				}},
				raw: []byte{17, 128, 230, 201, 32},
			})
			recording := roundTripWaveformPart10(t, file, syntax, object.SequenceLengthUndefined)
			groups := recording.Groups()
			if recording.SOPClassUID() != uidBasicVoiceAudio || len(groups) != 1 || len(groups[0].Channels) != 1 ||
				groups[0].SamplingFrequencyHz != 8000 || groups[0].Duration != 625*time.Microsecond ||
				groups[0].SampleCount != 5 || groups[0].Channels[0].Calibration.Units != units ||
				groups[0].Channels[0].Source.Meaning != "Synthetic voice" {
				t.Fatalf("audio metadata changed: %+v", groups)
			}
			got, err := recording.AudioPCM(context.Background(), 0, 5, nil)
			want := []int16{-28416, 0, 26112, 18688, -24576}
			if err != nil || !slices.Equal(got, want) {
				t.Fatalf("PCM samples = %v, %v, want %v", got, err, want)
			}
			if pcm, err := recording.AudioPCM(context.Background(), 5, 1, nil); !errors.Is(err, ErrAudioRange) || pcm != nil {
				t.Fatalf("value padding became a sample: %v, %v", pcm, err)
			}
		})
	}
}

func roundTripWaveformPart10(t *testing.T, file *object.File, syntax transfer.Syntax, policy object.SequenceLengthPolicy) *Recording {
	t.Helper()
	file.TransferSyntax = syntax
	file.Dataset.Put(stringElement(core.NewTag(0x0008, 0x0018), core.VRUI, "2.25.419398"))
	var encoded bytes.Buffer
	if err := object.WriteFileWithOptions(&encoded, file, object.WriteFileOptions{SequenceLength: policy}); err != nil {
		t.Fatal(err)
	}
	parsed, err := object.ReadFile(&encoded)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = parsed.Close() })
	if parsed.TransferSyntax.UID != syntax.UID {
		t.Fatalf("read Transfer Syntax = %s, want %s", parsed.TransferSyntax.UID, syntax.UID)
	}
	recording, err := Open(parsed, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recording.Close() })
	return recording
}

func TestReferenceWaveformCalibratedEnvelope(t *testing.T) {
	const padding = 16383
	const step = 2 * time.Millisecond
	stored := []int64{-1000, 14, 3, padding, padding, -21, 7, -4, 1500}
	// Independent physical values at each valid acquisition sample. Calibration
	// reverses the stored-value order, and padding never contributes an extreme.
	physical := []float64{2005, -23, -1, 0, 0, 47, -9, 13, -2995}
	sensitivity, correction, baseline := -4.0, 0.5, 5.0
	pad := uint64(padding)
	file := testFile(uidGeneralECG, binary.LittleEndian, nil, testGroup{
		frequency: 500, bits: 16, interpretation: "SS", samples: int64(len(stored)),
		channels: []testChannel{{
			bitsStored: 16, sensitivity: &sensitivity, correction: &correction, baseline: &baseline,
			units: &CodedConcept{Value: "uV", Scheme: "UCUM", Meaning: "microvolt"},
		}},
		raw: encodeSigned(binary.LittleEndian, 16, stored...), padding: &pad,
	})
	recording, err := Open(file, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recording.Close() })
	for _, window := range []struct {
		name       string
		start, end int
	}{
		{"full", 0, len(stored)}, {"interior", 1, 8}, {"starts_at_padding", 3, 7}, {"only_padding", 3, 5},
	} {
		for _, width := range []int{1, 3, 15} {
			t.Run(fmt.Sprintf("%s/width_%d", window.name, width), func(t *testing.T) {
				start := time.Duration(window.start) * step
				duration := time.Duration(window.end-window.start) * step
				got, err := recording.Envelope(context.Background(), 0, nil, start, duration, width)
				if err != nil || len(got) != 1 || len(got[0].Buckets) == 0 || len(got[0].Buckets) > width {
					t.Fatalf("envelope = %+v, %v", got, err)
				}
				next := int64(window.start)
				for _, bucket := range got[0].Buckets {
					if bucket.StartSample != next || bucket.EndSample <= next || bucket.EndSample > int64(window.end) {
						t.Fatalf("unexpected bucket interval at sample %d: %+v", next, bucket)
					}
					minIndex, maxIndex := int64(-1), int64(-1)
					for i := bucket.StartSample; i < bucket.EndSample; i++ {
						if stored[i] == padding {
							continue
						}
						if minIndex < 0 || physical[i] < physical[minIndex] {
							minIndex = i
						}
						if maxIndex < 0 || physical[i] > physical[maxIndex] {
							maxIndex = i
						}
					}
					if bucket.Valid != (minIndex >= 0) {
						t.Fatalf("bucket validity does not match recorded samples: %+v", bucket)
					}
					if bucket.Valid {
						for _, bound := range []struct {
							got  Sample
							want int64
						}{{bucket.Min, minIndex}, {bucket.Max, maxIndex}} {
							want := Sample{SampleIndex: bound.want, Time: time.Duration(bound.want) * step,
								Raw: float64(stored[bound.want]), Value: physical[bound.want], Valid: true, Calibrated: true}
							if bound.got != want {
								t.Fatalf("envelope extreme = %+v, want %+v", bound.got, want)
							}
						}
					}
					next = bucket.EndSample
				}
				if next != int64(window.end) {
					t.Fatalf("envelope ends at %d, want %d", next, window.end)
				}
			})
		}
	}
}

func TestReferenceWaveformRejectsPartialRecording(t *testing.T) {
	factoryFailure := errors.New("synthetic second source failure")
	for _, tc := range []struct {
		name        string
		mutate      func(*object.Object)
		annotation  bool
		sourceError error
		secondOwned bool
		wantErrorAt string
	}{
		{name: "missing sample count", mutate: func(group *object.Object) { group.Remove(tagNumberOfWaveformSamples) }},
		{name: "zero sample count", mutate: func(group *object.Object) { group.SetUL(tagNumberOfWaveformSamples, 0) }},
		{name: "channel count mismatch", mutate: func(group *object.Object) { group.SetUS(tagNumberOfWaveformChannels, 3) }},
		{name: "excess bits stored", mutate: func(group *object.Object) {
			channels, _ := group.GetSequence(tagChannelDefinitionSequence)
			channels[1].SetUS(tagWaveformBitsStored, 17)
			group.SetSequence(tagChannelDefinitionSequence, channels...)
		}},
		{name: "nonfinite frequency", mutate: func(group *object.Object) {
			group.Put(stringElement(tagSamplingFrequency, core.VRDS, "NaN"))
		}},
		{name: "short payload", mutate: func(group *object.Object) {
			group.Put(core.NewRawElement(tagWaveformData, core.VROW, make([]byte, 6)))
		}, secondOwned: true},
		{name: "long payload", mutate: func(group *object.Object) {
			group.Put(core.NewRawElement(tagWaveformData, core.VROW, make([]byte, 10)))
		}, secondOwned: true},
		{name: "padding VR contradicts payload VR", mutate: func(group *object.Object) {
			group.Put(core.NewRawElement(tagWaveformPaddingValue, core.VROB, []byte{0, 0}))
		}, secondOwned: true},
		{name: "second source fails", sourceError: factoryFailure},
		{name: "annotation names nonexistent channel", annotation: true, secondOwned: true, wantErrorAt: "parse annotations"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := testFile(uidGeneralECG, binary.LittleEndian, nil, ssTestGroup(2, 2), ssTestGroup(2, 2))
			if tc.mutate != nil {
				groups, _ := file.Dataset.GetSequence(tagWaveformSequence)
				tc.mutate(groups[1])
				file.Dataset.SetSequence(tagWaveformSequence, groups...)
			}
			if tc.annotation {
				file.Dataset.Put(annotationElement("POINT", []uint16{2, 3}, []uint32{1}, nil))
			}
			var sources [2]*observedWaveformSource
			recording, err := Open(file, Options{SourceFactory: func(index int, element core.Element) (SampleSource, error) {
				if index == 1 && tc.sourceError != nil {
					return nil, tc.sourceError
				}
				raw, ok := element.RawBytes()
				if !ok {
					t.Fatal("fixture Waveform Data has no bytes")
				}
				sources[index] = &observedWaveformSource{Reader: bytes.NewReader(raw)}
				return sources[index], nil
			}})
			if recording != nil {
				_ = recording.Close()
				t.Fatal("Open published a partial recording")
			}
			wantErrorAt := tc.wantErrorAt
			if wantErrorAt == "" {
				wantErrorAt = "multiplex group 2"
			}
			if err == nil || !strings.Contains(err.Error(), wantErrorAt) {
				t.Fatalf("Open error = %v, want failure at %s", err, wantErrorAt)
			}
			if tc.sourceError != nil && !errors.Is(err, tc.sourceError) {
				t.Fatalf("Open error lost source failure: %v", err)
			}
			for index, wantOwned := range []bool{true, tc.secondOwned} {
				source := sources[index]
				if (source != nil) != wantOwned {
					t.Fatalf("group %d acquired source = %t, want %t", index+1, source != nil, wantOwned)
				}
				if source != nil && (source.closes != 1 || source.reads != 0) {
					t.Fatalf("group %d source closes/reads = %d/%d, want 1/0", index+1, source.closes, source.reads)
				}
			}
		})
	}
}

type observedWaveformSource struct {
	*bytes.Reader
	reads  int
	closes int
}

func (s *observedWaveformSource) ReadAt(p []byte, off int64) (int, error) {
	s.reads++
	return s.Reader.ReadAt(p, off)
}

func (s *observedWaveformSource) Close() error {
	s.closes++
	return nil
}
