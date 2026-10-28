// Package model defines the wire and storage shape shared by every protocol.
package model

import "time"

// Kind is the protocol a record arrived on.
type Kind string

const (
	KindHTTP Kind = "http"
	KindGRPC Kind = "grpc"
	KindWS   Kind = "ws"
	KindSSE  Kind = "sse"
	KindTCP  Kind = "tcp"
	KindUDP  Kind = "udp"
)

// Valid reports whether k is one of the six protocols this app dumps.
func (k Kind) Valid() bool {
	switch k {
	case KindHTTP, KindGRPC, KindWS, KindSSE, KindTCP, KindUDP:
		return true
	}
	return false
}

// Dumpster is one rented bucket. Its id is the only access control there is.
type Dumpster struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// NormalizeUTC rewrites every timestamp on the record into UTC.
//
// The wire format is RFC3339 and the page converts to whatever zone the browser
// is in, so the instant is already portable. What this buys is uniformity: a
// server started with TZ set would otherwise write -06:00 offsets, and one
// snapshot would then mix zones that cannot be compared or sorted by eye.
func (r *Record) NormalizeUTC() {
	r.ReceivedAt = r.ReceivedAt.UTC()
	for i := range r.Events {
		r.Events[i].At = r.Events[i].At.UTC()
	}
}

// Remote is who connected and how.
type Remote struct {
	Addr      string `json:"addr"` // "ip:port" exactly as the listener saw it
	IP        string `json:"ip"`
	Port      int    `json:"port"`
	TLS       bool   `json:"tls"`
	TLSVia    string `json:"tls_via,omitempty"` // "direct" (app terminated) or "proxy" (X-Forwarded-Proto)
	TLSSNI    string `json:"tls_sni,omitempty"`
	Forwarded string `json:"forwarded_for,omitempty"` // raw X-Forwarded-For, always
	RealIP    string `json:"x_real_ip,omitempty"`     // raw X-Real-IP, always
}

// Timing is microseconds at the points named.
type Timing struct {
	ConnWaitUS int64 `json:"conn_wait_us"` // accept -> handler entry; 0 where unknown
	BodyUS     int64 `json:"body_us"`
	TotalUS    int64 `json:"total_us"`
}

// Decoded is a body re-rendered from its Content-Type. Value is JSON-safe.
type Decoded struct {
	Format string `json:"format"` // json | form | multipart | text
	Value  any    `json:"value,omitempty"`
	// Bytes is the size of the decoded value's JSON encoding, before any cap.
	Bytes int `json:"bytes,omitempty"`
	// Truncated means Value holds a text preview rather than the whole thing.
	Truncated bool   `json:"truncated,omitempty"`
	Error     string `json:"error,omitempty"`
}

// Request is the envelope each protocol fills in as far as it can.
type Request struct {
	Protocol    string              `json:"protocol"`
	Method      string              `json:"method,omitempty"`
	Path        string              `json:"path,omitempty"`
	Subpath     string              `json:"subpath,omitempty"` // path minus the "/d/{id}" prefix
	Query       string              `json:"query,omitempty"`
	Host        string              `json:"host,omitempty"`
	HTTPVersion string              `json:"http_version,omitempty"`
	Headers     map[string][]string `json:"headers,omitempty"`
	ContentLen  int64               `json:"content_length,omitempty"`
	BodyBytes   int                 `json:"body_bytes"`
	Truncated   bool                `json:"truncated,omitempty"`
	// BodySkipped means the board was over its byte budget when this arrived, so
	// only the metadata was kept. BodyBytes still reports the real size.
	BodySkipped bool     `json:"body_skipped,omitempty"`
	BodyText    string   `json:"body_text,omitempty"`
	BodyBase64  string   `json:"body_base64,omitempty"`
	Decoded     *Decoded `json:"decoded,omitempty"`
}

// Event is one frame, chunk, datagram or emitted SSE event inside a record.
type Event struct {
	At     time.Time `json:"at"`
	Dir    string    `json:"dir"`  // in | out
	Type   string    `json:"type"` // frame | chunk | datagram | message | event
	Bytes  int       `json:"bytes"`
	Text   string    `json:"text,omitempty"`
	Base64 string    `json:"base64,omitempty"`
}

// Record is one row in the debug page.
type Record struct {
	Seq           uint64            `json:"seq"`
	Kind          Kind              `json:"kind"`
	ReceivedAt    time.Time         `json:"received_at"`
	Remote        Remote            `json:"remote"`
	Req           *Request          `json:"req,omitempty"`
	Meta          map[string]string `json:"meta,omitempty"`
	Timing        Timing            `json:"timing"`
	Events        []Event           `json:"events"`
	DroppedEvents int               `json:"dropped_events,omitempty"`
	Closed        bool              `json:"closed,omitempty"`
	CloseNote     string            `json:"close_note,omitempty"`
}
