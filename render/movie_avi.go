package render

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"io"
	"math"
)

// Pure-Go Motion-JPEG AVI output (docs/MOVIE_EXPORT.md). The writer emits a
// classic AVI 1.0 RIFF file with one video stream:
//
//	RIFF 'AVI '
//	  LIST 'hdrl'
//	    'avih'  MainAVIHeader (56 bytes, AVIF_HASINDEX)
//	    LIST 'strl'
//	      'strh' AVIStreamHeader (56 bytes, 'vids'/'MJPG', rate = fps)
//	      'strf' BITMAPINFOHEADER (40 bytes, 24-bit, 'MJPG')
//	  LIST 'movi'
//	    '00dc' baseline JPEG frame (padded to an even size) ...
//	  'idx1' one AVIINDEXENTRY per frame (keyframe, offset from 'movi')
//
// Every frame is an independent baseline JFIF image written by image/jpeg, so
// every frame is a keyframe. OpenDML (AVI 2.0 'AVIX'/'indx') extensions are not
// written; instead the whole file is bounded by MaxAVIBytes (1 GiB by default)
// so the 32-bit RIFF sizes and idx1 offsets stay valid for every AVI 1.0
// reader. Output that would cross the bound is refused with ErrAVITooLarge
// before the offending frame is written.

const (
	// MaxAVIBytes is the default and largest accepted AVI 1.0 file size. The
	// RIFF size field is 32 bits, many readers treat it as signed, and the
	// OpenDML specification recommends closing the first RIFF chunk at 1 GiB;
	// larger movies would need the OpenDML AVIX/indx extension this writer
	// deliberately does not implement.
	MaxAVIBytes int64 = 1 << 30
	// DefaultAVIJPEGQuality is the image/jpeg quality used when AVIOptions
	// leaves Quality at zero.
	DefaultAVIJPEGQuality = 90
	// MaxAVIFPS bounds the frame rate recorded in the stream header.
	MaxAVIFPS = 240

	aviHeaderBytes     = 224 // RIFF header through the 'movi' list type
	aviMoviFourCCPos   = 220 // file offset of the 'movi' list type FOURCC
	aviIndexEntryBytes = 16
	aviChunkHeader     = 8
	aviFlagHasIndex    = 0x00000010
	aviFlagInterleaved = 0x00000100
	aviIndexKeyframe   = 0x00000010
	aviMaxDimension    = 65535
)

var (
	// ErrAVITooLarge reports that another frame would push the file beyond the
	// AVI 1.0 size bound. Frames already accepted still form a valid file once
	// the writer is closed.
	ErrAVITooLarge = errors.New("render: AVI output would exceed the AVI 1.0 size bound")
	// ErrAVIFrameSize reports a frame whose dimensions differ from the first.
	ErrAVIFrameSize = errors.New("render: AVI frame dimensions differ from the first frame")
	// ErrAVIClosed reports use of a closed AVI writer.
	ErrAVIClosed = errors.New("render: AVI writer is closed")
)

// AVIOptions configures Motion-JPEG AVI output.
type AVIOptions struct {
	// FPS is the playback rate in frames per second; values outside
	// [1, MaxAVIFPS] are rejected.
	FPS int
	// Quality is the JPEG quality (1-100); zero selects DefaultAVIJPEGQuality.
	Quality int
	// MaxBytes lowers the file-size bound; zero or values above MaxAVIBytes
	// select MaxAVIBytes.
	MaxBytes int64
}

func (o AVIOptions) normalized() (AVIOptions, error) {
	if o.FPS < 1 || o.FPS > MaxAVIFPS {
		return o, fmt.Errorf("render: AVI frame rate %d outside 1..%d", o.FPS, MaxAVIFPS)
	}
	if o.Quality == 0 {
		o.Quality = DefaultAVIJPEGQuality
	}
	if o.Quality < 1 || o.Quality > 100 {
		return o, fmt.Errorf("render: AVI JPEG quality %d outside 1..100", o.Quality)
	}
	if o.MaxBytes <= 0 || o.MaxBytes > MaxAVIBytes {
		o.MaxBytes = MaxAVIBytes
	}
	return o, nil
}

// aviLayout carries everything the fixed-size header and the index depend on.
type aviLayout struct {
	width, height int
	fps           int
	frameSizes    []uint32 // unpadded JPEG byte counts
	moviBytes     int64    // chunk headers + padded payloads inside 'movi'
}

func (l *aviLayout) add(size int) {
	l.frameSizes = append(l.frameSizes, uint32(size))
	l.moviBytes += aviChunkHeader + int64(padEven(size))
}

// projectedSize is the complete file size if one more frame of size bytes were
// appended now.
func (l *aviLayout) projectedSize(size int) int64 {
	frames := int64(len(l.frameSizes) + 1)
	return aviHeaderBytes + l.moviBytes + aviChunkHeader + int64(padEven(size)) +
		aviChunkHeader + frames*aviIndexEntryBytes
}

func (l *aviLayout) fileSize() int64 {
	return aviHeaderBytes + l.moviBytes + aviChunkHeader + int64(len(l.frameSizes))*aviIndexEntryBytes
}

func (l *aviLayout) maxFrame() uint32 {
	var m uint32
	for _, s := range l.frameSizes {
		if s > m {
			m = s
		}
	}
	return m
}

// header renders the RIFF/hdrl/movi-list preamble; its length is always
// aviHeaderBytes so it can be rewritten in place once the counts are known.
func (l *aviLayout) header() []byte {
	b := make([]byte, 0, aviHeaderBytes)
	le := binary.LittleEndian
	u32 := func(v uint32) { b = le.AppendUint32(b, v) }
	u16 := func(v uint16) { b = le.AppendUint16(b, v) }
	cc := func(s string) { b = append(b, s...) }

	frames := uint32(len(l.frameSizes))
	maxFrame := l.maxFrame()
	suggested := padEven(int(maxFrame)) + aviChunkHeader
	microsPerFrame := uint32(math.Round(1e6 / float64(l.fps)))
	maxBytesPerSec := uint64(suggested) * uint64(l.fps)
	if maxBytesPerSec > math.MaxUint32 {
		maxBytesPerSec = math.MaxUint32
	}

	cc("RIFF")
	u32(uint32(l.fileSize() - 8))
	cc("AVI ")

	cc("LIST")
	u32(4 + (8 + 56) + (8 + 4 + (8 + 56) + (8 + 40))) // 'hdrl' + avih + strl
	cc("hdrl")

	cc("avih")
	u32(56)
	u32(microsPerFrame)
	u32(uint32(maxBytesPerSec))
	u32(0) // dwPaddingGranularity
	u32(aviFlagHasIndex | aviFlagInterleaved)
	u32(frames)
	u32(0) // dwInitialFrames
	u32(1) // dwStreams
	u32(uint32(suggested))
	u32(uint32(l.width))
	u32(uint32(l.height))
	u32(0)
	u32(0)
	u32(0)
	u32(0)

	cc("LIST")
	u32(4 + (8 + 56) + (8 + 40))
	cc("strl")

	cc("strh")
	u32(56)
	cc("vids")
	cc("MJPG")
	u32(0) // dwFlags
	u16(0) // wPriority
	u16(0) // wLanguage
	u32(0) // dwInitialFrames
	u32(1) // dwScale
	u32(uint32(l.fps))
	u32(0) // dwStart
	u32(frames)
	u32(uint32(suggested))
	u32(math.MaxUint32) // dwQuality: driver default
	u32(0)              // dwSampleSize: variable-size video samples
	u16(0)              // rcFrame.left
	u16(0)              // rcFrame.top
	u16(uint16(l.width))
	u16(uint16(l.height))

	cc("strf")
	u32(40)
	u32(40) // biSize
	u32(uint32(l.width))
	u32(uint32(l.height)) // positive: bottom-up for raw DIBs; JPEG carries its own order
	u16(1)                // biPlanes
	u16(24)               // biBitCount
	cc("MJPG")
	u32(uint32(l.width * l.height * 3)) // biSizeImage (decoded size)
	u32(0)
	u32(0)
	u32(0)
	u32(0)

	cc("LIST")
	u32(uint32(4 + l.moviBytes))
	cc("movi")
	return b
}

// index renders the 'idx1' chunk. Offsets are measured from the 'movi' list
// type FOURCC, the convention used by the AVI 1.0 reference readers.
func (l *aviLayout) index() []byte {
	b := make([]byte, 0, aviChunkHeader+len(l.frameSizes)*aviIndexEntryBytes)
	le := binary.LittleEndian
	b = append(b, "idx1"...)
	b = le.AppendUint32(b, uint32(len(l.frameSizes)*aviIndexEntryBytes))
	offset := uint32(4)
	for _, size := range l.frameSizes {
		b = append(b, "00dc"...)
		b = le.AppendUint32(b, aviIndexKeyframe)
		b = le.AppendUint32(b, offset)
		b = le.AppendUint32(b, size)
		offset += aviChunkHeader + uint32(padEven(int(size)))
	}
	return b
}

func padEven(n int) int { return n + n&1 }

// AVIWriter streams Motion-JPEG frames into an AVI 1.0 file. It writes a
// placeholder header first, appends one '00dc' chunk per frame, and on Close
// appends 'idx1' and rewrites the header with the final counts, so the
// destination must support seeking (an *os.File does).
type AVIWriter struct {
	w      io.WriteSeeker
	opts   AVIOptions
	layout aviLayout
	buf    bytes.Buffer
	err    error
	closed bool
	begun  bool
}

// NewAVIWriter validates options and returns a writer positioned at the start
// of w. Nothing is written until the first frame.
func NewAVIWriter(w io.WriteSeeker, opts AVIOptions) (*AVIWriter, error) {
	if w == nil {
		return nil, fmt.Errorf("render: nil AVI destination")
	}
	normalized, err := opts.normalized()
	if err != nil {
		return nil, err
	}
	return &AVIWriter{w: w, opts: normalized, layout: aviLayout{fps: normalized.FPS}}, nil
}

// Frames reports how many frames have been written.
func (a *AVIWriter) Frames() int { return len(a.layout.frameSizes) }

// WriteFrame JPEG-encodes img and appends it. The first frame fixes the movie
// dimensions; later frames must match (ErrAVIFrameSize). ErrAVITooLarge leaves
// the file closable as a valid, shorter movie.
func (a *AVIWriter) WriteFrame(img image.Image) error {
	if a.closed {
		return ErrAVIClosed
	}
	if a.err != nil {
		return a.err
	}
	if img == nil || img.Bounds().Empty() {
		return fmt.Errorf("render: empty AVI frame %d", a.Frames()+1)
	}
	b := img.Bounds()
	if !a.begun {
		if b.Dx() > aviMaxDimension || b.Dy() > aviMaxDimension {
			return fmt.Errorf("render: AVI frame %dx%d exceeds %d pixels per side", b.Dx(), b.Dy(), aviMaxDimension)
		}
		a.layout.width, a.layout.height = b.Dx(), b.Dy()
	} else if b.Dx() != a.layout.width || b.Dy() != a.layout.height {
		return fmt.Errorf("%w: frame %d is %dx%d, movie is %dx%d", ErrAVIFrameSize, a.Frames()+1, b.Dx(), b.Dy(), a.layout.width, a.layout.height)
	}
	a.buf.Reset()
	if err := encodeMJPEGFrame(&a.buf, img, a.opts.Quality); err != nil {
		return fmt.Errorf("render: encode AVI frame %d: %w", a.Frames()+1, err)
	}
	size := a.buf.Len()
	if a.layout.projectedSize(size) > a.opts.MaxBytes {
		return fmt.Errorf("%w (%d bytes; frame %d not written)", ErrAVITooLarge, a.opts.MaxBytes, a.Frames()+1)
	}
	if !a.begun {
		if _, err := a.w.Write(a.layout.header()); err != nil {
			a.err = err
			return err
		}
		a.begun = true
	}
	if err := writeAVIChunk(a.w, a.buf.Bytes()); err != nil {
		a.err = err
		return err
	}
	a.layout.add(size)
	return nil
}

// Close writes the index and final header. A writer with no frames returns an
// error and writes nothing.
func (a *AVIWriter) Close() error {
	if a.closed {
		return ErrAVIClosed
	}
	a.closed = true
	if a.err != nil {
		return a.err
	}
	if a.Frames() == 0 {
		return fmt.Errorf("render: no frames to encode")
	}
	if _, err := a.w.Write(a.layout.index()); err != nil {
		return err
	}
	end, err := a.w.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	if _, err := a.w.Seek(end-a.layout.fileSize(), io.SeekStart); err != nil {
		return err
	}
	if _, err := a.w.Write(a.layout.header()); err != nil {
		return err
	}
	_, err = a.w.Seek(end, io.SeekStart)
	return err
}

// EncodeMJPEGAVI writes frames as a Motion-JPEG AVI to any io.Writer. The JPEG
// frames are encoded in memory first so the header can be written in a single
// pass; use AVIWriter to stream long movies into a seekable file.
func EncodeMJPEGAVI(w io.Writer, frames []image.Image, opts AVIOptions) error {
	if len(frames) == 0 {
		return fmt.Errorf("render: no frames to encode")
	}
	normalized, err := opts.normalized()
	if err != nil {
		return err
	}
	layout := aviLayout{fps: normalized.FPS}
	encoded := make([][]byte, 0, len(frames))
	for i, frame := range frames {
		if frame == nil || frame.Bounds().Empty() {
			return fmt.Errorf("render: empty AVI frame %d", i+1)
		}
		b := frame.Bounds()
		if i == 0 {
			if b.Dx() > aviMaxDimension || b.Dy() > aviMaxDimension {
				return fmt.Errorf("render: AVI frame %dx%d exceeds %d pixels per side", b.Dx(), b.Dy(), aviMaxDimension)
			}
			layout.width, layout.height = b.Dx(), b.Dy()
		} else if b.Dx() != layout.width || b.Dy() != layout.height {
			return fmt.Errorf("%w: frame %d is %dx%d, movie is %dx%d", ErrAVIFrameSize, i+1, b.Dx(), b.Dy(), layout.width, layout.height)
		}
		var buf bytes.Buffer
		if err := encodeMJPEGFrame(&buf, frame, normalized.Quality); err != nil {
			return fmt.Errorf("render: encode AVI frame %d: %w", i+1, err)
		}
		if layout.projectedSize(buf.Len()) > normalized.MaxBytes {
			return fmt.Errorf("%w (%d bytes at frame %d)", ErrAVITooLarge, normalized.MaxBytes, i+1)
		}
		layout.add(buf.Len())
		encoded = append(encoded, buf.Bytes())
	}
	if _, err := w.Write(layout.header()); err != nil {
		return err
	}
	for _, data := range encoded {
		if err := writeAVIChunk(w, data); err != nil {
			return err
		}
	}
	_, err = w.Write(layout.index())
	return err
}

func writeAVIChunk(w io.Writer, data []byte) error {
	var head [aviChunkHeader + 1]byte
	copy(head[:4], "00dc")
	binary.LittleEndian.PutUint32(head[4:8], uint32(len(data)))
	if _, err := w.Write(head[:aviChunkHeader]); err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	if len(data)&1 == 1 {
		if _, err := w.Write(head[aviChunkHeader:]); err != nil { // one zero pad byte
			return err
		}
	}
	return nil
}

// encodeMJPEGFrame writes a three-component (YCbCr 4:2:0) baseline JPEG.
// Grayscale sources are expanded to RGB first because several MJPEG decoders
// only accept three-component frames.
func encodeMJPEGFrame(w io.Writer, img image.Image, quality int) error {
	switch img.(type) {
	case *image.Gray, *image.Gray16:
		b := img.Bounds()
		rgba := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
		draw.Draw(rgba, rgba.Bounds(), img, b.Min, draw.Src)
		img = rgba
	}
	return jpeg.Encode(w, img, &jpeg.Options{Quality: quality})
}
