package decode

import (
	"bytes"
	"mime/multipart"
	"net/url"
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
