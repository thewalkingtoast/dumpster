package app

import (
	"encoding/base64"
	"errors"
	"net"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/adam/dumpster/internal/id"
	"github.com/adam/dumpster/internal/model"
)

// readUDP reads datagrams until the socket closes. Datagrams whose first 36
// bytes are not a dumpster id are silently dropped: UDP has no way to complain.
func (a *App) readUDP(conn net.PacketConn) {
	buf := make([]byte, 65535)
	for {
		n, addr, err := conn.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		a.handleUDPDatagram(buf[:n], addr)
	}
}

// handleUDPDatagram records exactly one datagram as one record.
func (a *App) handleUDPDatagram(packet []byte, addr net.Addr) {
	// A datagram shorter than an id cannot be addressed to anyone.
	if len(packet) < 36 {
		return
	}
	if !a.admitIngest(ipFromAddr(addr)) {
		return // dropped: a datagram sender cannot be told to slow down
	}
	canonical, err := id.Canonical(string(packet[:36]))
	if err != nil {
		return
	}
	d, ok := a.store.Get(canonical)
	if !ok {
		return
	}

	payload := packet[36:]
	if len(payload) > 0 {
		switch payload[0] {
		case '|', '\n', '\r', ' ':
			payload = payload[1:]
		}
	}

	ev := model.Event{At: time.Now(), Dir: "in", Type: "datagram", Bytes: len(payload)}
	if utf8.Valid(payload) {
		ev.Text = truncateText(string(payload), maxInlineText)
	} else {
		ev.Base64 = base64.StdEncoding.EncodeToString(payload)
	}

	rec := &model.Record{
		Kind:       model.KindUDP,
		ReceivedAt: time.Now(),
		Remote:     remoteFromAddr(addr),
		Meta:       map[string]string{"datagram_bytes": strconv.Itoa(len(packet))},
		Events:     []model.Event{ev},
	}
	stored, err := a.store.Add(d.ID, rec)
	if err != nil {
		return
	}
	a.publishNew(d.ID, &stored)
}
