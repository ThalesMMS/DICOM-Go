package jpip

import (
	"context"
	"errors"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/dictionary/tags"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// DecodedFrame is the native frame produced from one complete JPIP response.
type DecodedFrame struct {
	Metadata    pixeldata.Metadata
	Data        []byte
	ContentType string
	CacheHit    bool
	// Partial reports a frame rebuilt from a JPP/JPT response whose view
	// window did not hold every data-bin of the image (a reduced fsiz, a
	// region, or fewer layers): it has the full geometry but less detail.
	Partial bool
}

// DecodeFrame retrieves and decodes one complete JPEG 2000 or HTJ2K response.
// A JPP/JPT data-bin stream is rebuilt into a codestream when it ends with an
// End Of Response message saying the image or the requested window is done;
// a stream cut short (byte or response limits, no EOR, malformed messages)
// is the typed ErrIncrementalStream limitation, since finishing it needs a
// session that keeps requesting.
func (c *Client) DecodeFrame(ctx context.Context, request Request, dataset *object.Object) (DecodedFrame, error) {
	if dataset == nil {
		return DecodedFrame{}, &Error{Kind: ErrorKindInvalidRequest, Operation: "decode", Err: ErrInvalidRequest}
	}
	if request.Range != nil {
		return DecodedFrame{}, &Error{Kind: ErrorKindInvalidRequest, Operation: "decode", Err: ErrInvalidRequest}
	}
	response, err := c.Retrieve(ctx, request)
	if err != nil {
		return DecodedFrame{}, err
	}
	partial := false
	switch response.ContentType {
	case "image/jpp-stream", "image/jpt-stream":
		rebuilt, err := rebuildStream(response, uint64(request.Frame)+1)
		if err != nil {
			return DecodedFrame{}, &Error{
				Kind: ErrorKindIncrementalStream, Operation: "decode",
				ContentType: response.ContentType, Err: errors.Join(ErrIncrementalStream, err),
			}
		}
		response.Data, partial = rebuilt.Codestream, !rebuilt.Complete
	default:
		if !contentTypeMatchesReference(response.ContentType, request.Reference.HTJ2K) {
			return DecodedFrame{}, &Error{
				Kind: ErrorKindDecode, Operation: "decode",
				ContentType: response.ContentType, Err: errors.Join(ErrDecode, ErrUnsupportedContentType),
			}
		}
	}
	if c.registry == nil {
		return DecodedFrame{}, &Error{Kind: ErrorKindDecode, Operation: "decode", Err: errors.Join(ErrDecode, pixeldata.ErrCodecRegistryNil)}
	}

	// A JPIP stream query selects one one-based code stream. Override only the
	// cloned facade so codecs validate this response as one frame while the
	// caller's DICOM metadata remains unchanged.
	decoderDataset := object.FromElements(dataset.Elements(), std.Dictionary)
	decoderDataset.Put(core.NewRawElement(tags.NumberOfFrames, core.VRIS, []byte("1 ")))
	uid := transfer.JPEG2000.UID
	if request.Reference.HTJ2K {
		uid = transfer.HTJ2K.UID
	}
	frames, err := c.registry.DecodeFrames(uid, pixeldata.PixelData{
		Encapsulated: true,
		Sequence: core.FragmentSequence{
			Fragments: [][]byte{response.Data},
		},
	}, decoderDataset)
	if err != nil {
		return DecodedFrame{}, &Error{Kind: ErrorKindDecode, Operation: "decode", ContentType: response.ContentType, Err: errors.Join(ErrDecode, err)}
	}
	if len(frames.Data) != 1 || len(frames.Data[0]) == 0 {
		return DecodedFrame{}, &Error{Kind: ErrorKindCorruptResponse, Operation: "decode", ContentType: response.ContentType, Err: ErrCorruptResponse}
	}
	metadata, err := pixeldata.ExtractMetadata(decoderDataset)
	if err != nil {
		return DecodedFrame{}, &Error{Kind: ErrorKindDecode, Operation: "decode metadata", Err: errors.Join(ErrDecode, err)}
	}
	metadata.NumberOfFrames = 1
	return DecodedFrame{
		Metadata: metadata, Data: frames.Data[0],
		ContentType: response.ContentType, CacheHit: response.CacheHit, Partial: partial,
	}, nil
}

// rebuildStream turns one finished JPP/JPT response into a codestream.
func rebuildStream(response Response, stream uint64) (Reconstruction, error) {
	parsed, err := ParseStream(response.Data)
	if err != nil {
		return Reconstruction{}, err
	}
	if !parsed.HasEOR || parsed.EOR != EORImageDone && parsed.EOR != EORWindowDone {
		return Reconstruction{}, errors.New("dicom: JPIP response ended before its window was done")
	}
	set := NewDataBinSet(int64(len(response.Data)) + 1)
	if err := set.AddStream(parsed); err != nil {
		return Reconstruction{}, err
	}
	return set.Codestream(stream)
}

func contentTypeMatchesReference(contentType string, htj2k bool) bool {
	if htj2k {
		return contentType == "image/jph" || contentType == "image/jphc"
	}
	return contentType == "image/jp2"
}
