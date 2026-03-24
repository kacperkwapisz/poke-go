// Package ws provides a minimal WebSocket client using only the Go standard
// library. It implements enough of RFC 6455 to drive a reverse-proxy tunnel.
package ws

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// WebSocket opcodes (RFC 6455 §11.8).
const (
	opContinuation byte = 0x0
	opText         byte = 0x1
	opBinary       byte = 0x2
	opClose        byte = 0x8
	opPing         byte = 0x9
	opPong         byte = 0xA
)

// Conn is a WebSocket connection.
type Conn struct {
	conn net.Conn
	r    *bufio.Reader
}

// Dial establishes a WebSocket connection to rawURL with the given extra
// HTTP headers (e.g. Authorization).
func Dial(rawURL string, headers http.Header) (*Conn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid WebSocket URL: %w", err)
	}

	useTLS := u.Scheme == "wss"
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		if useTLS {
			port = "443"
		} else {
			port = "80"
		}
	}
	addr := net.JoinHostPort(host, port)

	var conn net.Conn
	if useTLS {
		conn, err = tls.Dial("tcp", addr, &tls.Config{ServerName: host})
	} else {
		conn, err = net.Dial("tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", addr, err)
	}

	// Build the Sec-WebSocket-Key.
	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		conn.Close()
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)

	path := u.RequestURI()
	if path == "" {
		path = "/"
	}

	// Write the HTTP upgrade request.
	var sb strings.Builder
	fmt.Fprintf(&sb, "GET %s HTTP/1.1\r\n", path)
	fmt.Fprintf(&sb, "Host: %s\r\n", u.Host)
	sb.WriteString("Upgrade: websocket\r\n")
	sb.WriteString("Connection: Upgrade\r\n")
	fmt.Fprintf(&sb, "Sec-WebSocket-Key: %s\r\n", key)
	sb.WriteString("Sec-WebSocket-Version: 13\r\n")
	for name, values := range headers {
		for _, v := range values {
			fmt.Fprintf(&sb, "%s: %s\r\n", name, v)
		}
	}
	sb.WriteString("\r\n")

	if _, err := io.WriteString(conn, sb.String()); err != nil {
		conn.Close()
		return nil, fmt.Errorf("sending upgrade request: %w", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: "GET"})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("reading upgrade response: %w", err)
	}
	resp.Body.Close()

	if resp.StatusCode != 101 {
		conn.Close()
		if resp.StatusCode == 401 {
			return nil, fmt.Errorf("unauthorized — run 'poke login' again")
		}
		return nil, fmt.Errorf("WebSocket upgrade failed: %s", resp.Status)
	}

	// Verify Sec-WebSocket-Accept.
	h := sha1.New()
	h.Write([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	want := base64.StdEncoding.EncodeToString(h.Sum(nil))
	if got := resp.Header.Get("Sec-Websocket-Accept"); got != want {
		conn.Close()
		return nil, fmt.Errorf("bad Sec-WebSocket-Accept: got %q want %q", got, want)
	}

	return &Conn{conn: conn, r: br}, nil
}

// ReadMessage reads the next complete (possibly fragmented) text or binary
// message, transparently handling ping/pong and close frames.
func (c *Conn) ReadMessage() ([]byte, error) {
	var payload []byte
	for {
		fin, opcode, data, err := c.readFrame()
		if err != nil {
			return nil, err
		}
		switch opcode {
		case opClose:
			_ = c.writeFrame(opClose, nil)
			c.conn.Close()
			return nil, io.EOF
		case opPing:
			_ = c.writeFrame(opPong, data)
			continue
		case opPong:
			continue
		}
		payload = append(payload, data...)
		if fin {
			return payload, nil
		}
	}
}

// WriteMessage sends data as a single unmasked (server-masked per RFC for
// clients) text frame.
func (c *Conn) WriteMessage(data []byte) error {
	return c.writeFrame(opText, data)
}

// Close sends a close frame and closes the underlying connection.
func (c *Conn) Close() {
	_ = c.writeFrame(opClose, nil)
	c.conn.Close()
}

// readFrame reads one WebSocket frame.
func (c *Conn) readFrame() (fin bool, opcode byte, payload []byte, err error) {
	b0, err := c.r.ReadByte()
	if err != nil {
		return false, 0, nil, err
	}
	b1, err := c.r.ReadByte()
	if err != nil {
		return false, 0, nil, err
	}

	fin = b0&0x80 != 0
	opcode = b0 & 0x0F
	masked := b1&0x80 != 0
	payloadLen := int64(b1 & 0x7F)

	switch payloadLen {
	case 126:
		var ext uint16
		if err := binary.Read(c.r, binary.BigEndian, &ext); err != nil {
			return false, 0, nil, err
		}
		payloadLen = int64(ext)
	case 127:
		var ext uint64
		if err := binary.Read(c.r, binary.BigEndian, &ext); err != nil {
			return false, 0, nil, err
		}
		payloadLen = int64(ext)
	}

	var maskKey [4]byte
	if masked {
		if _, err := io.ReadFull(c.r, maskKey[:]); err != nil {
			return false, 0, nil, err
		}
	}

	data := make([]byte, payloadLen)
	if _, err := io.ReadFull(c.r, data); err != nil {
		return false, 0, nil, err
	}
	if masked {
		for i := range data {
			data[i] ^= maskKey[i%4]
		}
	}
	return fin, opcode, data, nil
}

// writeFrame writes one masked (client-to-server) WebSocket frame.
func (c *Conn) writeFrame(opcode byte, payload []byte) error {
	maskKey := make([]byte, 4)
	if _, err := rand.Read(maskKey); err != nil {
		return err
	}

	var frame []byte
	frame = append(frame, 0x80|opcode) // FIN bit set

	plen := len(payload)
	switch {
	case plen <= 125:
		frame = append(frame, byte(plen)|0x80) // mask bit set
	case plen <= 65535:
		frame = append(frame, 126|0x80)
		frame = append(frame, byte(plen>>8), byte(plen))
	default:
		frame = append(frame, 127|0x80)
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(plen))
		frame = append(frame, ext[:]...)
	}
	frame = append(frame, maskKey...)

	masked := make([]byte, plen)
	for i, b := range payload {
		masked[i] = b ^ maskKey[i%4]
	}
	frame = append(frame, masked...)

	_, err := c.conn.Write(frame)
	return err
}
