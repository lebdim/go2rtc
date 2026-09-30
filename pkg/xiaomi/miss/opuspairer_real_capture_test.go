package miss

import (
	"encoding/binary"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// decodeCode2Length mirrors how a real Opus decoder reads a code-2 frame-1
// length (RFC 6716 3.2.1), used here to assert structural validity, not just
// non-nil output, for every frame produced from real captured RTP payloads.
func decodeCode2Length(b []byte) (length, headerSize int) {
	if b[1] < 252 {
		return int(b[1]), 2
	}
	return int(b[1]) + 4*int(b[2]), 3
}

// TestOpusPairerOnRealCapturedTraffic replays a real go2rtc->camera RTP capture
// (213 packets, sizes 126-307 bytes, config=19/CELT-NB/20ms, all code=0) through
// the actual production opusPairer+JoinFrames path and checks every joined frame
// is structurally valid Opus, catching the size>=252 length-encoding bug that
// produced "running water" on the real camera (34/213 real frames hit it).
func TestOpusPairerOnRealCapturedTraffic(t *testing.T) {
	raw, err := os.ReadFile("testdata/real_capture.bin")
	require.NoError(t, err)

	type rtpPkt struct {
		seq     uint16
		payload []byte
	}
	var pkts []rtpPkt
	off := 0
	for off < len(raw) {
		ln := int(binary.BigEndian.Uint16(raw[off:]))
		off += 2
		pkt := raw[off : off+ln]
		off += ln

		b0 := pkt[0]
		cc := int(b0 & 0x0F)
		headerLen := 12 + cc*4
		seq := binary.BigEndian.Uint16(pkt[2:4])
		pkts = append(pkts, rtpPkt{seq: seq, payload: pkt[headerLen:]})
	}
	require.Len(t, pkts, 213)

	pairer := &opusPairer{}
	joined := 0
	oversizeCases := 0
	for _, pkt := range pkts {
		frame := pairer.Push(pkt.seq, pkt.payload)
		if frame == nil {
			continue
		}
		joined++

		switch frame[0] & 0b11 {
		case 0b10: // code 2: differing sizes, has explicit length field to validate
			decodedLen, headerSize := decodeCode2Length(frame)
			require.LessOrEqual(t, headerSize+decodedLen, len(frame),
				"joined frame declares a frame-1 length longer than the packet itself (seq=%d)", pkt.seq)
			if decodedLen >= 252 {
				oversizeCases++
			}
		case 0b01: // code 1: equal sizes, trivially valid by construction
		case 0b00, 0b11:
			t.Fatalf("unexpected joined TOC code %d for seq=%d", frame[0]&0b11, pkt.seq)
		}
	}

	require.Greater(t, joined, 0, "expected at least one joined frame from the real capture")
	require.Greater(t, oversizeCases, 0, "expected the real capture to exercise the >=252 byte path (it has 34/213 such frames)")
}
