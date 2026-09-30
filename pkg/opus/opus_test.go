package opus

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// decodeCode2Length parses a code-2 packet's frame-1 length exactly as an Opus
// decoder would (RFC 6716 3.2.1), returning the decoded length and header size.
func decodeCode2Length(b []byte) (length, headerSize int) {
	if b[1] < 252 {
		return int(b[1]), 2
	}
	return int(b[1]) + 4*int(b[2]), 3
}

func TestJoinFramesSameSize(t *testing.T) {
	b1 := append([]byte{0x00}, make([]byte, 50)...)
	b2 := append([]byte{0x00}, make([]byte, 50)...)
	joined := JoinFrames(b1, b2)
	require.Equal(t, byte(0b01), joined[0]&0b11)
}

// Real captured C300 TTS traffic showed 34/213 frames >=252 bytes (VBR speech
// routinely exceeds this); this must round-trip to the exact original sizes.
func TestJoinFramesDifferentSizeLarge(t *testing.T) {
	for _, size1 := range []int{100, 251, 252, 253, 260, 298, 299, 1275} {
		b1 := append([]byte{0x00}, make([]byte, size1)...)
		b2 := append([]byte{0x00}, make([]byte, 60)...)
		joined := JoinFrames(b1, b2)

		require.Equal(t, byte(0b10), joined[0]&0b11, "size1=%d", size1)

		decodedLen, headerSize := decodeCode2Length(joined)
		require.Equal(t, size1, decodedLen, "size1=%d: decoded frame-1 length mismatch", size1)
		require.Equal(t, joined[headerSize:headerSize+size1], b1[1:], "size1=%d: frame-1 payload mismatch", size1)
		require.Equal(t, joined[headerSize+size1:], b2[1:], "size1=%d: frame-2 payload mismatch", size1)
	}
}

func TestJoinFramesCantJoin(t *testing.T) {
	b1 := []byte{0x01, 0xAA}
	b2 := []byte{0x01, 0xBB}
	joined := JoinFrames(b1, b2)
	require.Equal(t, append(append([]byte{}, b1...), b2...), joined)
}
