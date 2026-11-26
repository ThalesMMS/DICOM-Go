package hl7v3

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"mime"
	"net/http"
	"strings"
)

func readXMLResponse(ctx context.Context, resp *http.Response, limit int64) ([]byte, error) {
	if resp.ContentLength > limit {
		return nil, ErrResponseTooLarge
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, contextOr(ctx, ErrTransport)
	}
	if int64(len(body)) > limit {
		return nil, ErrResponseTooLarge
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(body) == 0 {
		return nil, nil
	}

	encodings := resp.Header.Values("Content-Encoding")
	if len(encodings) > 1 || (len(encodings) == 1 && !strings.EqualFold(strings.TrimSpace(encodings[0]), "identity")) {
		return nil, ErrResponse
	}
	contentTypes := resp.Header.Values("Content-Type")
	if len(contentTypes) != 1 {
		return nil, ErrResponse
	}
	mediaType, params, err := mime.ParseMediaType(contentTypes[0])
	if err != nil || (mediaType != "application/xml" && mediaType != "text/xml" && !strings.HasSuffix(mediaType, "+xml")) {
		return nil, ErrResponse
	}
	if charset, present := params["charset"]; present && !strings.EqualFold(charset, "utf-8") {
		return nil, ErrResponse
	}
	if err := validateXMLResponse(ctx, withoutXMLBOM(body)); err != nil {
		return nil, err
	}
	return body, nil
}

func validateXMLResponse(ctx context.Context, data []byte) error {
	const maxDepth, maxTokens = 64, 100_000
	decoder := xml.NewDecoder(bytes.NewReader(data))
	sawRoot := false
	depth := 0
	for count := 0; ; count++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		offset := decoder.InputOffset()
		token, err := decoder.Token()
		if err == io.EOF && sawRoot && depth == 0 {
			return ctx.Err()
		}
		if err != nil {
			return ErrResponse
		}
		if count >= maxTokens {
			return ErrResponseTooLarge
		}
		switch value := token.(type) {
		case xml.StartElement:
			if depth == 0 {
				if sawRoot {
					return ErrResponse
				}
				sawRoot = true
			}
			depth++
			if depth > maxDepth {
				return ErrResponseTooLarge
			}
			if duplicateXMLAttributes(value.Attr) {
				return ErrResponse
			}
		case xml.EndElement:
			depth--
		case xml.CharData:
			if depth == 0 && len(bytes.Trim(value, " \t\r\n")) != 0 {
				return ErrResponse
			}
		case xml.Directive:
			return ErrResponse
		case xml.ProcInst:
			if value.Target != "xml" || offset != 0 {
				return ErrResponse
			}
		}
	}
}

func duplicateXMLAttributes(attributes []xml.Attr) bool {
	if len(attributes) < 2 {
		return false
	}
	seen := make(map[xml.Name]struct{}, len(attributes))
	for _, attribute := range attributes {
		if _, duplicate := seen[attribute.Name]; duplicate {
			return true
		}
		seen[attribute.Name] = struct{}{}
	}
	return false
}

func withoutXMLBOM(data []byte) []byte {
	return bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
}
