package decode

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/url"
	"strings"
	"testing"
)

func TestJSON(t *testing.T) {
	got := Body("application/json; charset=utf-8", []byte(`{"a":[1,2]}`))
	if got == nil || got.Format != "json" || got.Error != "" {
		t.Fatalf("got %+v", got)
	}
	m, ok := got.Value.(map[string]any)
	if !ok {
		t.Fatalf("value type = %T", got.Value)
	}
	if _, ok := m["a"]; !ok {
		t.Fatalf("missing key a in %+v", m)
	}
}

func TestJSONSubtypeAndError(t *testing.T) {
	if got := Body("application/vnd.api+json", []byte(`{"x":1}`)); got == nil || got.Format != "json" {
		t.Fatalf("+json subtype: %+v", got)
	}
	got := Body("application/json", []byte(`{"a":`))
	if got == nil || got.Error == "" {
		t.Fatalf("broken json should report an error, got %+v", got)
	}
}

func TestForm(t *testing.T) {
	got := Body("application/x-www-form-urlencoded", []byte("a=1&b=2&b=3"))
	if got == nil || got.Format != "form" {
		t.Fatalf("got %+v", got)
	}
	vals, ok := got.Value.(url.Values)
	if !ok {
		t.Fatalf("value type = %T", got.Value)
	}
	if len(vals["b"]) != 2 {
		t.Fatalf("b = %v, want two values", vals["b"])
	}
}

func TestTextAndBinary(t *testing.T) {
	if got := Body("text/plain", []byte("hello")); got == nil || got.Format != "text" {
		t.Fatalf("text: %+v", got)
	}
	if got := Body("application/octet-stream", []byte{0xff, 0xfe, 0x00}); got != nil {
		t.Fatalf("binary should not decode, got %+v", got)
	}
	if got := Body("application/json", nil); got == nil || got.Error == "" {
		t.Fatalf("empty json body should be an error, got %+v", got)
	}
}

func TestMultipartFieldsAndFiles(t *testing.T) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("colour", "purple"); err != nil {
		t.Fatalf("WriteField: %v", err)
	}
	fw, err := w.CreateFormFile("shot", "x.png")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := fw.Write([]byte("fake-png-bytes")); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	got := Body(w.FormDataContentType(), buf.Bytes())
	if got == nil || got.Format != "multipart" {
		t.Fatalf("got %+v", got)
	}
	val, ok := got.Value.(map[string]any)
	if !ok {
		t.Fatalf("value type = %T", got.Value)
	}
	fields := val["fields"].(map[string][]string)
	if got := fields["colour"]; len(got) != 1 || got[0] != "purple" {
		t.Fatalf("fields = %v", fields)
	}
	files := val["files"].([]map[string]any)
	if len(files) != 1 {
		t.Fatalf("files = %v, want 1", files)
	}
	if files[0]["filename"] != "x.png" {
		t.Fatalf("filename = %v", files[0]["filename"])
	}
	if files[0]["size"] != 14 {
		t.Fatalf("size = %v, want 14", files[0]["size"])
	}
}

func TestMultipartMissingBoundary(t *testing.T) {
	got := Body("multipart/form-data", []byte("garbage"))
	if got == nil || got.Error == "" {
		t.Fatalf("want a boundary error, got %+v", got)
	}
}

// A 1 MiB JSON body must not put 1 MiB into the record. The value is replaced by
// a text preview of its own encoding, and the original size is kept so the page
// can still say how much arrived.
func TestCapReplacesAnOversizedDecodedValue(t *testing.T) {
	pad := strings.Repeat("a", 1<<20)
	body := []byte(`{"pad":"` + pad + `"}`)

	d := Body("application/json", body)
	d = Cap(d, len(body), 64<<10)

	if !d.Truncated {
		t.Fatal("oversized value was not marked truncated")
	}
	if d.Bytes < len(body) {
		t.Fatalf("Bytes = %d, want at least the body size %d", d.Bytes, len(body))
	}
	enc, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(enc) > 80<<10 {
		t.Fatalf("capped record still serialises to %d bytes", len(enc))
	}
	if _, ok := d.Value.(string); !ok {
		t.Fatalf("capped value should be a text preview, got %T", d.Value)
	}
}

func TestCapLeavesSmallValuesAlone(t *testing.T) {
	body := []byte(`{"sku":"A1","qty":3}`)
	d := Cap(Body("application/json", body), len(body), 64<<10)
	if d.Truncated {
		t.Fatal("a small value was truncated")
	}
	if _, ok := d.Value.(map[string]any); !ok {
		t.Fatalf("small value should stay structured, got %T", d.Value)
	}
}
