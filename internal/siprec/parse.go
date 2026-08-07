package siprec

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"strings"
)

// utf8BOM is stripped before parsing: some SBCs prepend it to the metadata part.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// Parse decodes an RFC 7865 metadata document. Unknown elements are ignored,
// since vendors extend the schema.
func Parse(raw []byte) (*Recording, error) {
	trimmed := bytes.TrimSpace(bytes.TrimPrefix(bytes.TrimSpace(raw), utf8BOM))
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("empty metadata document")
	}

	var r Recording
	dec := xml.NewDecoder(bytes.NewReader(trimmed))
	dec.Strict = false
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("parse recording metadata: %w", err)
	}
	if r.XMLName.Local != "recording" {
		return nil, fmt.Errorf("unexpected metadata root element %q", r.XMLName.Local)
	}
	r.DataMode = strings.ToLower(strings.TrimSpace(r.DataMode))
	return &r, nil
}

// IsPartial reports whether the document is an incremental update. Absent
// datamode means complete (RFC 7865 §6.1).
func (r *Recording) IsPartial() bool {
	return r != nil && r.DataMode == DataModePartial
}

// Marshal renders the document with the registered namespace and an XML
// declaration. Field order is fixed by the struct, so output is deterministic.
func (r *Recording) Marshal() ([]byte, error) {
	if r == nil {
		return nil, fmt.Errorf("nil recording")
	}
	out := *r
	out.Xmlns = Namespace
	if out.DataMode == "" {
		out.DataMode = DataModeComplete
	}

	body, err := xml.Marshal(&out)
	if err != nil {
		return nil, fmt.Errorf("marshal recording metadata: %w", err)
	}
	return append([]byte(xml.Header), body...), nil
}
