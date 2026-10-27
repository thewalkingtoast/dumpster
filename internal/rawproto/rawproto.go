// Package rawproto carries opaque gRPC message bodies.
package rawproto

import (
	"fmt"

	"google.golang.org/grpc/encoding"
)

// Message is an opaque gRPC payload.
type Message struct {
	Body []byte
}

// Codec passes bytes through untouched, which lets the server accept calls for
// services and methods it has never heard of.
type Codec struct{}

// Marshal returns the payload as-is.
func (Codec) Marshal(v any) ([]byte, error) {
	m, ok := v.(*Message)
	if !ok {
		return nil, fmt.Errorf("rawproto: cannot marshal %T", v)
	}
	return m.Body, nil
}

// Unmarshal copies the payload into v.
func (Codec) Unmarshal(data []byte, v any) error {
	m, ok := v.(*Message)
	if !ok {
		return fmt.Errorf("rawproto: cannot unmarshal into %T", v)
	}
	m.Body = append([]byte(nil), data...)
	return nil
}

// Name reports the content-subtype this codec answers for.
func (Codec) Name() string { return "proto" }

var _ encoding.Codec = Codec{}
