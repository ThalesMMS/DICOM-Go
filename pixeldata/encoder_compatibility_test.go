package pixeldata

import (
	"context"
	"errors"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/transfer"
	"slices"
	"testing"
)

func compatibilityTestCapabilities() EncoderCapabilities {
	caps := testEncoderCapabilities()
	caps.TransferSyntaxUID = transfer.JPEGExtended.UID
	caps.Lossless = false
	caps.LossyMethod = "ISO_10918_1"
	caps.CompatibilityProfiles = []EncoderCompatibilityProfile{EncoderCompatibilityJPEGExtendedColor, EncoderCompatibilityJPEGExtendedReducedPrecision}
	return caps
}
func TestEncoderCompatibilityRegistrationAndOwnership(t *testing.T) {
	for _, change := range []func(*EncoderCapabilities){
		func(c *EncoderCapabilities) { c.CompatibilityProfiles = []EncoderCompatibilityProfile{"unknown"} },
		func(c *EncoderCapabilities) {
			c.CompatibilityProfiles = append(c.CompatibilityProfiles, c.CompatibilityProfiles[0])
		},
		func(c *EncoderCapabilities) { c.TransferSyntaxUID = transfer.RLELossless.UID },
	} {
		caps := compatibilityTestCapabilities()
		change(&caps)
		if err := NewMemoryEncoderRegistry().RegisterEncoder(caps.TransferSyntaxUID, &testFrameEncoder{capabilities: caps}); !errors.Is(err, ErrEncoderCapabilitiesInvalid) {
			t.Fatal("invalid profiles accepted", err)
		}
	}
	caps := compatibilityTestCapabilities()
	encoder := &testFrameEncoder{capabilities: caps, encoded: EncodedFrame{Data: []byte{1}, CompatibilityProfile: EncoderCompatibilityJPEGExtendedColor}}
	registry := NewMemoryEncoderRegistry()
	if err := registry.RegisterEncoder(caps.TransferSyntaxUID, encoder); err != nil {
		t.Fatal(err)
	}
	caps.CompatibilityProfiles[0] = "corrupted"
	wrapper, ok := registry.GetEncoder(transfer.JPEGExtended.UID)
	if !ok {
		t.Fatal("missing")
	}
	returned := wrapper.Capabilities()
	returned.CompatibilityProfiles[0] = "changed"
	want := []EncoderCompatibilityProfile{EncoderCompatibilityJPEGExtendedColor, EncoderCompatibilityJPEGExtendedReducedPrecision}
	if !slices.Equal(wrapper.Capabilities().CompatibilityProfiles, want) {
		t.Fatal("profile slice aliased")
	}
	if got, err := registry.EncodeFrame(context.Background(), transfer.JPEGExtended.UID, []byte{1, 2, 3, 4}, testEncoderMetadata()); err != nil || got.CompatibilityProfile != EncoderCompatibilityJPEGExtendedColor {
		t.Fatal(got, err)
	}
}
func TestEncoderCompatibilityRejectsUndeclaredOutputAtBothBoundaries(t *testing.T) {
	for _, profile := range []EncoderCompatibilityProfile{EncoderCompatibilityJPEGExtendedColor, "unknown"} {
		caps := compatibilityTestCapabilities()
		caps.CompatibilityProfiles = nil
		encoder := &testFrameEncoder{capabilities: caps, encoded: EncodedFrame{Data: []byte{1}, CompatibilityProfile: profile}}
		registry := NewMemoryEncoderRegistry()
		if err := registry.RegisterEncoder(caps.TransferSyntaxUID, encoder); err != nil {
			t.Fatal(err)
		}
		wrapper, _ := registry.GetEncoder(caps.TransferSyntaxUID)
		for _, call := range []func() (EncodedFrame, error){
			func() (EncodedFrame, error) {
				return registry.EncodeFrame(context.Background(), caps.TransferSyntaxUID, []byte{1, 2, 3, 4}, testEncoderMetadata())
			},
			func() (EncodedFrame, error) {
				return wrapper.EncodeFrame(context.Background(), []byte{1, 2, 3, 4}, testEncoderMetadata())
			},
		} {
			if got, err := call(); !errors.Is(err, ErrEncoderOutputInvalid) || len(got.Data) != 0 {
				t.Fatal("undeclared profile published", got, err)
			}
		}
	}
}
func TestTranscodeEncoderCompatibilityRequiresConsistentFrames(t *testing.T) {
	for _, second := range []EncoderCompatibilityProfile{"", EncoderCompatibilityJPEGExtendedReducedPrecision} {
		source := transcodeNativeObject([]byte{1, 2, 3, 4, 5, 6, 7, 8})
		source.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0008), VR: core.VRIS}, Value: core.StringValue{"2"}})
		calls := 0
		caps := compatibilityTestCapabilities()
		encoder := &stageFrameEncoder{capabilities: caps, encode: func(context.Context, []byte, Metadata) (EncodedFrame, error) {
			profile := EncoderCompatibilityJPEGExtendedColor
			if calls > 0 {
				profile = second
			}
			calls++
			return EncodedFrame{Data: []byte{1, 2}, CompatibilityProfile: profile}, nil
		}}
		registry := NewMemoryEncoderRegistry()
		if err := registry.RegisterEncoder(caps.TransferSyntaxUID, encoder); err != nil {
			t.Fatal(err)
		}
		out, _, err := TranscodeDataSet(context.Background(), source, transfer.ExplicitVRLittleEndian, transfer.JPEGExtended, TranscodeOptions{EncoderRegistry: registry, AllowLossy: true})
		if !errors.Is(err, ErrEncoderOutputInvalid) || out != nil || calls != 2 {
			t.Fatal("inconsistent profile published", out, err, calls)
		}
		raw, err := ExtractNativeFrames(source)
		if err != nil || len(raw.Data) != 2 || raw.Data[1][0] != 5 {
			t.Fatal("source mutated", err)
		}
	}
}
