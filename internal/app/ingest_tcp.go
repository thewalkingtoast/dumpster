package app

import (
	"bufio"
	"encoding/base64"
	"errors"
	"net"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/adam/dumpster/internal/id"
	"github.com/adam/dumpster/internal/model"
)

// acceptTCP runs the accept loop for raw TCP until the listener closes.
func (a *App) acceptTCP(lis net.Listener) {
	for {
		conn, err := lis.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		go a.handleTCPConn(conn)
	}
}

// handleTCPConn reads the dumpster id from the first line, then records every
// chunk that follows. TCP is diagnostics only: nothing is echoed back except
// the error lines below.
func (a *App) handleTCPConn(conn net.Conn) {
	defer conn.Close()

	remote := remoteFromAddr(conn.RemoteAddr())
	release, admitted := a.admitConn(remote.IP)
	if !admitted {
		return // close immediately; the caller sees EOF, not a recorded session
	}
	defer release()
	if !a.admitIngest(remote.IP) {
		return
	}
	start := time.Now()
	_ = conn.SetReadDeadline(time.Now().Add(a.cfg.ConnIdle))

	reader := bufio.NewReaderSize(conn, 4096)
	line, err := readIDLine(reader, 128)
	if err != nil {
		_, _ = conn.Write([]byte("expected the dumpster id on the first line\n"))
		return
	}
	canonical, err := id.Canonical(strings.TrimSpace(line))
	if err != nil {
		_, _ = conn.Write([]byte("invalid dumpster id\n"))
		return
	}
	d, ok := a.store.Get(canonical)
	if !ok {
		_, _ = conn.Write([]byte("unknown or expired dumpster\n"))
		return
	}

	rec := &model.Record{
		Kind:       model.KindTCP,
		ReceivedAt: start,
		Remote:     remote,
		Meta: map[string]string{
			"id_line": strings.TrimSpace(line),
			"local":   conn.LocalAddr().String(),
		},
	}
	stored, err := a.store.Add(d.ID, rec)
	if err != nil {
		return
	}
	a.publishNew(d.ID, &stored)

	buf := make([]byte, 32*1024)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(a.cfg.ConnIdle))
		n, err := reader.Read(buf)
		if n > 0 {
			data := buf[:n]
			ev := model.Event{At: time.Now(), Dir: "in", Type: "chunk", Bytes: n}
			if utf8.Valid(data) {
				ev.Text = truncateText(string(data), maxInlineText)
			} else {
				ev.Base64 = base64.StdEncoding.EncodeToString(data)
			}
			if updated, appendErr := a.store.AppendEvent(d.ID, stored.Seq, ev); appendErr == nil {
				a.publishUpdate(d.ID, &updated)
			}
		}
		if err != nil {
			note := "closed by peer"
			if errors.Is(err, os.ErrDeadlineExceeded) {
				note = "idle timeout"
			}
			a.closeRecord(d.ID, stored.Seq, note)
			return
		}
	}
}

// readIDLine reads until a newline, refusing to buffer an unbounded line from a
// client that never sends one.
func readIDLine(reader *bufio.Reader, max int) (string, error) {
	var sb strings.Builder
	for sb.Len() < max {
		b, err := reader.ReadByte()
		if err != nil {
			return "", err
		}
		if b == '\n' {
			return sb.String(), nil
		}
		sb.WriteByte(b)
	}
	return "", errors.New("id line too long")
}
