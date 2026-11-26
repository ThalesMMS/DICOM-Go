package object

import (
	"bufio"
	"compress/flate"
	"compress/zlib"
	"io"
	"sync"

	"github.com/ThalesMMS/dicom-go/parser"
)

// inflatedDataSet wraps a Deflated Explicit VR Little Endian data set stream.
// PS3.5 A.5 requires raw deflate (RFC 1951). With allowZlib, a stream that
// starts with a valid zlib header (RFC 1950) is read through compress/zlib,
// which also checks the Adler-32 trailer; zlib reports whether that happened.
func inflatedDataSet(r io.Reader, allowZlib bool) (inflated io.ReadCloser, zlibWrapped bool, err error) {
	if !allowZlib {
		return newRawDataSetInflater(r), false, nil
	}
	br := bufio.NewReader(r)
	head, _ := br.Peek(2)
	if !isZlibHeader(head) {
		return newRawDataSetInflater(br), false, nil
	}
	zr, err := zlib.NewReader(br)
	if err != nil {
		return nil, false, err
	}
	return zr, true, nil
}

type resettableInflater interface {
	io.ReadCloser
	flate.Resetter
}

type rawDataSetInflater struct{ resettableInflater }

var rawDataSetInflaters sync.Pool

func newRawDataSetInflater(source io.Reader) *rawDataSetInflater {
	if cached := rawDataSetInflaters.Get(); cached != nil {
		inflater := cached.(*rawDataSetInflater)
		if err := inflater.Reset(source, nil); err == nil {
			return inflater
		}
	}
	return &rawDataSetInflater{flate.NewReader(source).(resettableInflater)}
}

func (d *rawDataSetInflater) Close() error {
	err := d.resettableInflater.Close()
	// Retain the decoder's work buffers without retaining the caller's stream.
	if d.Reset(emptyDeflateSource{}, nil) == nil {
		rawDataSetInflaters.Put(d)
	}
	return err
}

type emptyDeflateSource struct{}

func (emptyDeflateSource) Read([]byte) (int, error) { return 0, io.EOF }
func (emptyDeflateSource) ReadByte() (byte, error)  { return 0, io.EOF }

// isZlibHeader reports whether head starts with an RFC 1950 header using
// deflate with a window of at most 32 KiB and no preset dictionary. Raw
// deflate cannot start this way unless an encoder wrote nonzero padding
// after an empty-header stored block.
func isZlibHeader(head []byte) bool {
	if len(head) < 2 {
		return false
	}
	cmf, flg := head[0], head[1]
	return cmf&0x0f == 8 && cmf>>4 <= 7 && flg&0x20 == 0 && (uint16(cmf)<<8|uint16(flg))%31 == 0
}

// paddedDeflater writes one raw deflate (RFC 1951) bitstream for a Deflated
// Explicit VR Little Endian data set. PS3.5 A.5: "If the Deflate algorithm
// produces an odd number of bytes then a single trailing NULL byte shall be
// added after the last byte of the deflated bit stream." Close appends that
// pad, which also keeps every DIMSE P-DATA fragment of the data set even
// (PS3.8 E.1); readers stop at the final deflate block and
// never see it.
type paddedDeflater struct {
	destination io.Writer
	written     int64
	deflater    *flate.Writer
}

func newPaddedDeflater(destination io.Writer) (*paddedDeflater, error) {
	return newPaddedDeflaterWithLevel(destination, flate.DefaultCompression)
}

func newPaddedDeflaterWithLevel(destination io.Writer, level int) (*paddedDeflater, error) {
	d := &paddedDeflater{destination: destination}
	deflater, err := flate.NewWriter(countingDestination{d}, level)
	if err != nil {
		return nil, err
	}
	d.deflater = deflater
	return d, nil
}

func (d *paddedDeflater) Write(p []byte) (int, error) {
	return d.deflater.Write(p)
}

// Close terminates the bitstream and pads it to an even length.
func (d *paddedDeflater) Close() error {
	if err := d.deflater.Close(); err != nil {
		return err
	}
	if d.written%2 == 0 {
		return nil
	}
	_, err := countingDestination{d}.Write([]byte{0})
	return err
}

type countingDestination struct{ d *paddedDeflater }

func (c countingDestination) Write(p []byte) (int, error) {
	n, err := c.d.destination.Write(p)
	c.d.written += int64(n)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	return n, err
}

func zlibDeflateRecovery(offset int64) parser.BoundaryRecovery {
	return parser.BoundaryRecovery{
		Path: "(dataset)", Offset: offset, DeclaredLength: -1, AvailableLength: -1,
		Reason: parser.BoundaryZlibDeflate,
	}
}
