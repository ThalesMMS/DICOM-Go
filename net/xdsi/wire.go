package xdsi

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"io"
	"mime"
	"mime/multipart"
	"net/textproto"
	"strings"
)

const (
	nsSOAP                 = "http://www.w3.org/2003/05/soap-envelope"
	nsWSA                  = "http://www.w3.org/2005/08/addressing"
	nsXDS                  = "urn:ihe:iti:xds-b:2007"
	nsRAD                  = "urn:ihe:rad:xdsi-b:2009"
	nsRIM                  = "urn:oasis:names:tc:ebxml-regrep:xsd:rim:3.0"
	nsLCM                  = "urn:oasis:names:tc:ebxml-regrep:xsd:lcm:3.0"
	nsRS                   = "urn:oasis:names:tc:ebxml-regrep:xsd:rs:3.0"
	nsXOP                  = "http://www.w3.org/2004/08/xop/include"
	actionPublish          = "urn:ihe:iti:2007:ProvideAndRegisterDocumentSet-b"
	actionPublishResponse  = "urn:ihe:iti:2007:ProvideAndRegisterDocumentSet-bResponse"
	actionRetrieve         = "urn:ihe:rad:2009:RetrieveImagingDocumentSet"
	actionRetrieveResponse = "urn:ihe:iti:2007:RetrieveDocumentSetResponse"
	statusSuccess          = "urn:oasis:names:tc:ebxml-regrep:ResponseStatusType:Success"
	statusPartial          = "urn:ihe:iti:2007:ResponseStatusType:PartialSuccess"
	statusFailure          = "urn:oasis:names:tc:ebxml-regrep:ResponseStatusType:Failure"
	anonymous              = "http://www.w3.org/2005/08/addressing/anonymous"
	maxSOAPBytes           = 2 << 20
)

type wireElement struct {
	Name     xml.Name
	Attrs    []xml.Attr
	Text     string
	Children []*wireElement
	text     strings.Builder
}

func element(ns, local, text string, attrs ...string) *wireElement {
	node := &wireElement{Name: xml.Name{Space: ns, Local: local}, Text: text}
	for i := 0; i+1 < len(attrs); i += 2 {
		node.Attrs = append(node.Attrs, xml.Attr{Name: xml.Name{Local: attrs[i]}, Value: attrs[i+1]})
	}
	return node
}

func (n *wireElement) MarshalXML(enc *xml.Encoder, _ xml.StartElement) error {
	start := xml.StartElement{Name: n.Name, Attr: n.Attrs}
	if err := enc.EncodeToken(start); err != nil {
		return err
	}
	if n.Text != "" {
		if err := enc.EncodeToken(xml.CharData(n.Text)); err != nil {
			return err
		}
	}
	for _, child := range n.Children {
		if err := enc.Encode(child); err != nil {
			return err
		}
	}
	return enc.EncodeToken(start.End())
}

func (n *wireElement) children(ns, local string) []*wireElement {
	var found []*wireElement
	for _, child := range n.Children {
		if child.Name.Space == ns && child.Name.Local == local {
			found = append(found, child)
		}
	}
	return found
}

func (n *wireElement) one(ns, local string) (*wireElement, error) {
	found := n.children(ns, local)
	if len(found) != 1 {
		return nil, ErrProtocol
	}
	return found[0], nil
}

func (n *wireElement) attr(local string) string {
	for _, attr := range n.Attrs {
		if attr.Name.Space == "" && attr.Name.Local == local {
			return attr.Value
		}
	}
	return ""
}

func parseXML(data []byte) (*wireElement, error) {
	if len(data) > maxSOAPBytes {
		return nil, ErrLimit
	}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	var root *wireElement
	var stack []*wireElement
	elements := 0
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, ErrProtocol
		}
		switch v := token.(type) {
		case xml.StartElement:
			elements++
			if elements > 20000 || len(stack) >= 24 || len(v.Attr) > 24 {
				return nil, ErrLimit
			}
			attributes := make(map[xml.Name]bool, len(v.Attr))
			for _, a := range v.Attr {
				if attributes[a.Name] {
					return nil, ErrProtocol
				}
				attributes[a.Name] = true
			}
			n := &wireElement{Name: v.Name, Attrs: append([]xml.Attr(nil), v.Attr...)}
			if len(stack) > 0 {
				parent := stack[len(stack)-1]
				parent.Children = append(parent.Children, n)
			} else {
				if root != nil {
					return nil, ErrProtocol
				}
				root = n
			}
			stack = append(stack, n)
		case xml.EndElement:
			if len(stack) == 0 {
				return nil, ErrProtocol
			}
			stack[len(stack)-1].Text = stack[len(stack)-1].text.String()
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) == 0 {
				if strings.TrimSpace(string(v)) != "" {
					return nil, ErrProtocol
				}
			} else {
				n := stack[len(stack)-1]
				n.text.Write(v)
			}
		case xml.Directive:
			return nil, ErrProtocol
		case xml.ProcInst:
			if v.Target != "xml" || root != nil {
				return nil, ErrProtocol
			}
		}
	}
	if root == nil || len(stack) != 0 {
		return nil, ErrProtocol
	}
	return root, nil
}

func newMessageID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value[:])
	return "urn:uuid:" + encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}

func envelope(action, to, messageID string, body *wireElement) *wireElement {
	header := element(nsSOAP, "Header", "")
	actionNode := element(nsWSA, "Action", action)
	actionNode.Attrs = []xml.Attr{{Name: xml.Name{Space: nsSOAP, Local: "mustUnderstand"}, Value: "1"}}
	toNode := element(nsWSA, "To", to)
	toNode.Attrs = actionNode.Attrs
	reply := element(nsWSA, "ReplyTo", "")
	reply.Children = []*wireElement{element(nsWSA, "Address", anonymous)}
	header.Children = []*wireElement{actionNode, element(nsWSA, "MessageID", messageID), reply, toNode}
	bodyNode := element(nsSOAP, "Body", "")
	bodyNode.Children = []*wireElement{body}
	root := element(nsSOAP, "Envelope", "")
	root.Children = []*wireElement{header, bodyNode}
	return root
}

func responseBody(root *wireElement, action, requestID string) (*wireElement, error) {
	if root.Name != (xml.Name{Space: nsSOAP, Local: "Envelope"}) || len(root.Children) != 2 {
		return nil, ErrProtocol
	}
	header, err := root.one(nsSOAP, "Header")
	if err != nil {
		return nil, err
	}
	for _, child := range header.Children {
		known := child.Name.Space == nsWSA && (child.Name.Local == "Action" || child.Name.Local == "RelatesTo" || child.Name.Local == "MessageID" || child.Name.Local == "To")
		for _, a := range child.Attrs {
			if a.Name == (xml.Name{Space: nsSOAP, Local: "mustUnderstand"}) && (a.Value == "1" || a.Value == "true") && !known {
				return nil, ErrProtocol
			}
		}
	}
	actionNode, err := header.one(nsWSA, "Action")
	if err != nil || len(actionNode.Children) != 0 || strings.TrimSpace(actionNode.Text) != action {
		return nil, ErrProtocol
	}
	relates, err := header.one(nsWSA, "RelatesTo")
	if err != nil || len(relates.Children) != 0 || strings.TrimSpace(relates.Text) != requestID {
		return nil, ErrProtocol
	}
	body, err := root.one(nsSOAP, "Body")
	if err != nil || len(body.Children) != 1 {
		return nil, ErrProtocol
	}
	if body.Children[0].Name == (xml.Name{Space: nsSOAP, Local: "Fault"}) {
		return nil, ErrProtocol
	}
	return body.Children[0], nil
}

type attachment struct {
	id, mediaType string
	data          []byte
}

func encodeMTOM(root *wireElement, action string, attachments []attachment) ([]byte, string, error) {
	soap, err := xml.Marshal(root)
	if err != nil {
		return nil, "", err
	}
	if len(soap) > maxSOAPBytes {
		return nil, "", ErrLimit
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := textproto.MIMEHeader{}
	header.Set("Content-Type", mime.FormatMediaType("application/xop+xml", map[string]string{"charset": "UTF-8", "type": "application/soap+xml"}))
	header.Set("Content-ID", "<root@xdsi>")
	header.Set("Content-Transfer-Encoding", "binary")
	part, err := writer.CreatePart(header)
	if err != nil {
		return nil, "", err
	}
	if _, err := part.Write(soap); err != nil {
		return nil, "", err
	}
	for _, a := range attachments {
		header = textproto.MIMEHeader{}
		header.Set("Content-Type", a.mediaType)
		header.Set("Content-ID", "<"+a.id+">")
		header.Set("Content-Transfer-Encoding", "binary")
		part, err := writer.CreatePart(header)
		if err != nil {
			return nil, "", err
		}
		if _, err := part.Write(a.data); err != nil {
			return nil, "", err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	contentType := mime.FormatMediaType("multipart/related", map[string]string{
		"boundary": writer.Boundary(), "type": "application/xop+xml", "start": "<root@xdsi>",
		"start-info": mime.FormatMediaType("application/soap+xml", map[string]string{"action": action}),
	})
	return body.Bytes(), contentType, nil
}

func readMTOM(contentType string, body io.Reader, limits Limits, maxParts int) (*wireElement, map[string]attachment, error) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "multipart/related" || params["type"] != "application/xop+xml" ||
		params["boundary"] == "" || params["start"] == "" {
		return nil, nil, ErrProtocol
	}
	startInfo, _, err := mime.ParseMediaType(params["start-info"])
	if err != nil || startInfo != "application/soap+xml" {
		return nil, nil, ErrProtocol
	}
	reader := multipart.NewReader(&io.LimitedReader{R: body, N: limits.TotalBytes + 1}, params["boundary"])
	parts := make(map[string]attachment, maxParts)
	var total int64
	rootID := strings.TrimSuffix(strings.TrimPrefix(params["start"], "<"), ">")
	for i := 0; ; i++ {
		part, err := reader.NextRawPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, ErrProtocol
		}
		if i >= maxParts {
			return nil, nil, ErrLimit
		}
		id := part.Header.Get("Content-ID")
		if len(id) < 3 || id[0] != '<' || id[len(id)-1] != '>' || len(id) > 200 {
			return nil, nil, ErrProtocol
		}
		id = id[1 : len(id)-1]
		if _, duplicate := parts[id]; duplicate {
			return nil, nil, ErrProtocol
		}
		transferEncoding := part.Header.Get("Content-Transfer-Encoding")
		if transferEncoding != "binary" && transferEncoding != "8bit" {
			return nil, nil, ErrProtocol
		}
		mt, options, err := mime.ParseMediaType(part.Header.Get("Content-Type"))
		if err != nil {
			return nil, nil, ErrProtocol
		}
		ceiling := limits.DocumentBytes
		if id == rootID {
			ceiling = maxSOAPBytes
			if mt != "application/xop+xml" || options["type"] != "application/soap+xml" {
				return nil, nil, ErrProtocol
			}
		}
		data, err := io.ReadAll(io.LimitReader(part, ceiling+1))
		if err != nil {
			return nil, nil, err
		}
		if int64(len(data)) > ceiling {
			return nil, nil, ErrLimit
		}
		total += int64(len(data))
		if total > limits.TotalBytes {
			return nil, nil, ErrLimit
		}
		parts[id] = attachment{id: id, mediaType: mt, data: data}
	}
	soap, found := parts[rootID]
	if !found {
		return nil, nil, ErrProtocol
	}
	delete(parts, rootID)
	root, err := parseXML(soap.data)
	return root, parts, err
}
