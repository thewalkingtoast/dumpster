package hub

import (
	"testing"
	"time"
)

func TestPublishFansOutAndCloseEndsIt(t *testing.T) {
	h := New(4)
	ch, cancel := h.Subscribe("abc")
	defer cancel()

	h.Publish("abc", map[string]string{"type": "new"})
	select {
	case b := <-ch:
		if string(b) != `{"type":"new"}` {
			t.Fatalf("message = %s", b)
		}
	case <-time.After(time.Second):
		t.Fatal("no message received")
	}

	h.Close("abc")
	select {
	case _, open := <-ch:
		if open {
			t.Fatal("channel still open after Close")
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not close the subscriber")
	}
}

func TestTwoSubscribersBothReceive(t *testing.T) {
	h := New(4)
	a, ca := h.Subscribe("abc")
	defer ca()
	b, cb := h.Subscribe("abc")
	defer cb()

	h.Publish("abc", 1)
	for name, ch := range map[string]<-chan []byte{"a": a, "b": b} {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatalf("subscriber %s got nothing", name)
		}
	}
}

func TestPublishNeverBlocksOnFullSubscriber(t *testing.T) {
	h := New(1)
	_, cancel := h.Subscribe("abc")
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 200; i++ {
			h.Publish("abc", i)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Publish blocked on a full subscriber")
	}
}

func TestCancelIsIdempotent(t *testing.T) {
	h := New(1)
	_, cancel := h.Subscribe("abc")
	cancel()
	cancel() // must not panic on double close
}
