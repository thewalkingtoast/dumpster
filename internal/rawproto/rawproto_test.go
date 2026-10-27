package rawproto

import "testing"

func TestRoundTrip(t *testing.T) {
	in := &Message{Body: []byte{0x00, 0x01, 0xff}}
	raw, err := Codec{}.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	out := &Message{}
	if err := (Codec{}).Unmarshal(raw, out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if string(out.Body) != string(in.Body) {
		t.Fatalf("body = %v, want %v", out.Body, in.Body)
	}
	// Unmarshal must copy: the caller reuses its buffer.
	buf := []byte{1, 2, 3}
	_ = (Codec{}).Unmarshal(buf, out)
	buf[0] = 9
	if out.Body[0] != 1 {
		t.Fatal("Unmarshal aliased the source buffer")
	}
}

func TestMarshalRejectsWrongType(t *testing.T) {
	if _, err := (Codec{}).Marshal("nope"); err == nil {
		t.Fatal("expected an error for a non-Message value")
	}
	if err := (Codec{}).Unmarshal([]byte("x"), &struct{}{}); err == nil {
		t.Fatal("expected an error for a non-Message destination")
	}
}

func TestCodecName(t *testing.T) {
	if got := (Codec{}).Name(); got != "proto" {
		t.Fatalf("Name = %q, want proto", got)
	}
}
