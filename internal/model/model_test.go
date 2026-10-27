package model

import "testing"

func TestKindValid(t *testing.T) {
	for _, k := range []Kind{KindHTTP, KindGRPC, KindWS, KindSSE, KindTCP, KindUDP} {
		if !k.Valid() {
			t.Fatalf("%q should be valid", k)
		}
	}
	if Kind("smtp").Valid() {
		t.Fatal("smtp is not a supported kind")
	}
}
