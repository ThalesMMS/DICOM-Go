package dicomweb

import (
	"bufio"
	"bytes"
	"net/textproto"
)

const multipartResponseHeaderFields = 10_000

// Read the raw section before textproto allocates MIME headers. The bytes
// beyond the blank line stay in the shared wire buffer for the part reader.
func (r *multipartResponseReader) readHeaders() (textproto.MIMEHeader, error) {
	var raw bytes.Buffer
	fields := 0
	continued := false
	for {
		line, err := r.wire.ReadSlice('\n')
		if len(line) > r.maxHeaderBytes-raw.Len() {
			return nil, &ResponseDecodeError{Kind: ResponseDecodeHeaderBytes, Limit: int64(r.maxHeaderBytes), Value: int64(r.maxHeaderBytes) + 1}
		}
		_, _ = raw.Write(line)
		if !continued && err == nil && (bytes.Equal(line, []byte("\r\n")) || bytes.Equal(line, []byte("\n"))) {
			header := bufio.NewReaderSize(bytes.NewReader(raw.Bytes()), min(raw.Len(), multipartResponseLineBytes))
			return textproto.NewReader(header).ReadMIMEHeader()
		}
		if !continued && len(line) > 0 && line[0] != ' ' && line[0] != '\t' {
			fields++
			if fields > multipartResponseHeaderFields {
				return nil, &ResponseDecodeError{Kind: ResponseDecodeMalformedMultipart}
			}
		}
		if err != nil && err != bufio.ErrBufferFull {
			return nil, multipartIncomplete(err)
		}
		continued = err == bufio.ErrBufferFull
	}
}
