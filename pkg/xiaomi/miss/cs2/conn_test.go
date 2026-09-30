package cs2

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

func frame(size uint32, payload int) []byte {
	b := make([]byte, 4+payload)
	binary.BigEndian.PutUint32(b, size)
	return b
}

func TestPushFrameSizeBounds(t *testing.T) {
	tests := []struct {
		name    string
		size    uint32
		payload int
		wantErr string
	}{
		{"exactly at the bound", maxFrameSize, maxFrameSize, ""},
		{"one byte over the bound", maxFrameSize + 1, 0, "cs2: invalid frame size 16777217"},
		{"uint32 max stays positive", 0xFFFFFFFF, 0, "cs2: invalid frame size 4294967295"},
		{"negative when cast to int32", 0x80000000, 0, "cs2: invalid frame size 2147483648"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newDataChannel(0, 1)
			err := c.Push(frame(tt.size, tt.payload))

			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tt.wantErr != "" && err == nil:
				t.Fatalf("expected error %q, got nil", tt.wantErr)
			case tt.wantErr != "" && err.Error() != tt.wantErr:
				t.Fatalf("expected error %q, got %q", tt.wantErr, err)
			}

			if tt.wantErr != "" && c.waitData != nil {
				t.Fatal("waitData must be dropped after a rejected frame")
			}
		})
	}
}

func TestPushZeroSizeIsPadding(t *testing.T) {
	c := newDataChannel(0, 1)
	payload := []byte("frame")

	if err := c.Push(append(frame(0, 0), frame(uint32(len(payload)), 0)...)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := c.Push(payload); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, ok := c.Pop()
	if !ok || string(data) != "frame" {
		t.Fatalf("expected %q, got %q (ok=%v)", payload, data, ok)
	}
}

func TestPushReassemblesSplitWrites(t *testing.T) {
	c := newDataChannel(0, 2)
	payload := make([]byte, 100)
	for i := range payload {
		payload[i] = byte(i)
	}

	wire := append(frame(uint32(len(payload)), 0), payload...)
	for i := 0; i < len(wire); i += 7 {
		end := min(i+7, len(wire))
		if err := c.Push(wire[i:end]); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	data, ok := c.Pop()
	if !ok || string(data) != string(payload) {
		t.Fatalf("reassembled frame mismatch (ok=%v, len=%d)", ok, len(data))
	}
	if c.waitData != nil {
		t.Fatal("waitData must be released once fully consumed")
	}
}

type captureConn struct {
	writes [][]byte
}

func (c *captureConn) Read([]byte) (int, error) { return 0, io.EOF }
func (c *captureConn) Write(p []byte) (int, error) {
	c.writes = append(c.writes, append([]byte(nil), p...))
	return len(p), nil
}
func (c *captureConn) Close() error                     { return nil }
func (c *captureConn) LocalAddr() net.Addr              { return dummyAddr("local") }
func (c *captureConn) RemoteAddr() net.Addr             { return dummyAddr("remote") }
func (c *captureConn) SetDeadline(time.Time) error      { return nil }
func (c *captureConn) SetReadDeadline(time.Time) error  { return nil }
func (c *captureConn) SetWriteDeadline(time.Time) error { return nil }

type dummyAddr string

func (a dummyAddr) Network() string { return "test" }
func (a dummyAddr) String() string  { return string(a) }

func TestWritePacketAppendsPayload(t *testing.T) {
	wire := &captureConn{}
	c := &Conn{Conn: wire}

	hdr := bytes.Repeat([]byte{0x11}, hdrSize)
	payload := []byte{0xAA, 0xBB, 0xCC, 0xDD}

	if err := c.WritePacket(hdr, payload); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(wire.writes) != 1 {
		t.Fatalf("expected 1 write, got %d", len(wire.writes))
	}

	const offset = 12
	got := wire.writes[0]
	if len(got) != offset+hdrSize+len(payload) {
		t.Fatalf("unexpected wire size: got %d want %d", len(got), offset+hdrSize+len(payload))
	}
	if want := uint16(hdrSize + len(payload) + 8); binary.BigEndian.Uint16(got[2:4]) != want {
		t.Fatalf("unexpected DRW size: got %d want %d", binary.BigEndian.Uint16(got[2:4]), want)
	}
	if want := uint32(hdrSize + len(payload)); binary.BigEndian.Uint32(got[8:12]) != want {
		t.Fatalf("unexpected payload size: got %d want %d", binary.BigEndian.Uint32(got[8:12]), want)
	}
	if !bytes.Equal(got[offset:offset+hdrSize], hdr) {
		t.Fatal("header bytes mismatch")
	}
	if !bytes.Equal(got[offset+hdrSize:], payload) {
		t.Fatal("payload bytes mismatch")
	}
}
