package hub

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func TestRedisBrokerRelaysBetweenHubsButNotToSelf(t *testing.T) {
	mr := miniredis.RunT(t)
	url := "redis://" + mr.Addr()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hubA := New(8)
	hubB := New(8)

	brokerA, err := NewRedisBroker(ctx, url, "test:", "a")
	if err != nil {
		t.Fatalf("broker A: %v", err)
	}
	brokerB, err := NewRedisBroker(ctx, url, "test:", "b")
	if err != nil {
		t.Fatalf("broker B: %v", err)
	}
	hubA.UseBroker(brokerA)
	hubB.UseBroker(brokerB)

	go func() { _ = hubA.RunBroker(ctx) }()
	go func() { _ = hubB.RunBroker(ctx) }()

	chA, cancelA := hubA.Subscribe("d1")
	defer cancelA()
	chB, cancelB := hubB.Subscribe("d1")
	defer cancelB()

	// Give both subscriptions a moment to attach.
	time.Sleep(100 * time.Millisecond)

	hubA.Publish("d1", map[string]string{"type": "new"})

	// B must receive the relayed message.
	select {
	case got := <-chB:
		if len(got) == 0 {
			t.Fatal("B received an empty payload")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("B never received the relayed message")
	}

	// A must receive exactly one copy: its local fan-out, not the echo.
	select {
	case <-chA:
	case <-time.After(2 * time.Second):
		t.Fatal("A never received its own local message")
	}
	select {
	case <-chA:
		t.Fatal("A received its own message twice (echo not filtered)")
	case <-time.After(300 * time.Millisecond):
	}
}
