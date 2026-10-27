// Package decode re-renders a request body according to its Content-Type.
package decode

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/adam/dumpster/internal/model"
)

const (
	maxParts    = 50      // multipart parts examined per request
	maxPartByte = 1 << 20 // bytes read per part
	previewByte = 512     // bytes of a file part shown as text
)

// Body classifies raw bytes by media type. It returns nil when nothing sensible
// can be decoded, which the UI shows as "not decodable" rather than an error.
func Body(contentType string, body []byte) *model.Decoded {
	mt, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		mt = strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	}
	switch {
	case mt == "application/json" || strings.HasSuffix(mt, "+json"):
		var v any
		if err := json.Unmarshal(body, &v); err != nil {
			return &model.Decoded{Format: "json", Error: err.Error()}
		}
		return &model.Decoded{Format: "json", Value: v}
	case mt == "application/x-www-form-urlencoded":
		vals, err := url.ParseQuery(string(body))
		if err != nil {
			return &model.Decoded{Format: "form", Error: err.Error()}
		}
		return &model.Decoded{Format: "form", Value: vals}
	case mt == "multipart/form-data":
		return multipartBody(body, params["boundary"])
	case len(body) > 0 && utf8.Valid(body):
		return &model.Decoded{Format: "text", Value: string(body)}
	}
	return nil
}

// multipartBody splits fields from files. Files are described, not retained:
// name, media type, size and sha256 are enough to identify an upload.
func multipartBody(body []byte, boundary string) *model.Decoded {
	if boundary == "" {
		return &model.Decoded{Format: "multipart", Error: "missing boundary parameter"}
	}
	mr := multipart.NewReader(bytes.NewReader(body), boundary)
	fields := map[string][]string{}
	files := []map[string]any{}
	for i := 0; i < maxParts; i++ {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return &model.Decoded{Format: "multipart", Error: err.Error()}
		}
		data, readErr := io.ReadAll(io.LimitReader(p, maxPartByte))
		p.Close()
		if readErr != nil {
			return &model.Decoded{Format: "multipart", Error: readErr.Error()}
		}
		if name := p.FileName(); name != "" {
			sum := sha256.Sum256(data)
			files = append(files, map[string]any{
				"field":        p.FormName(),
				"filename":     name,
				"content_type": p.Header.Get("Content-Type"),
				"size":         len(data),
				"sha256":       hex.EncodeToString(sum[:]),
				"preview":      preview(data),
			})
			continue
		}
		fields[p.FormName()] = append(fields[p.FormName()], string(data))
	}
	return &model.Decoded{Format: "multipart", Value: map[string]any{
		"fields": fields,
		"files":  files,
	}}
}

// preview renders the first previewByte bytes as text when they are UTF-8.
func preview(b []byte) string {
	if len(b) > previewByte {
		b = b[:previewByte]
	}
	if !utf8.Valid(b) {
		return ""
	}
	return string(b)
}
