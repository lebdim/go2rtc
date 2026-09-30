package miss

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpusPairerJoinsConsecutivePackets(t *testing.T) {
	p := &opusPairer{}

	require.Nil(t, p.Push(1, []byte{0x01, 0xAA}))
	frame := p.Push(2, []byte{0x01, 0xBB})
	require.NotNil(t, frame)
}

func TestOpusPairerResyncsOnGap(t *testing.T) {
	p := &opusPairer{}

	// first half of a pair arrives, then a packet is lost: seq jumps 1 -> 3 instead of 1 -> 2
	require.Nil(t, p.Push(1, []byte{0x01, 0xAA}))
	// the stale first half must be dropped, not joined with the post-gap packet
	require.Nil(t, p.Push(3, []byte{0x01, 0xBB}))

	// the pairing is back in sync starting from seq 3: seq 4 completes a fresh pair
	frame := p.Push(4, []byte{0x01, 0xCC})
	require.NotNil(t, frame)
}

func TestOpusPairerResyncsOnReorder(t *testing.T) {
	p := &opusPairer{}

	require.Nil(t, p.Push(5, []byte{0x01, 0xAA}))
	// seq 4 arrives after seq 5: out of order, must not be joined with the seq 5 half-frame
	require.Nil(t, p.Push(4, []byte{0x01, 0xBB}))

	// sync resumes from seq 4: seq 5 duplicate-looking value is itself out of sequence relative to 4,
	// so only a genuinely consecutive seq completes a pair
	require.Nil(t, p.Push(6, []byte{0x01, 0xCC}))
	frame := p.Push(7, []byte{0x01, 0xDD})
	require.NotNil(t, frame)
}

func TestOpusPairerResyncsOnDuplicate(t *testing.T) {
	p := &opusPairer{}

	require.Nil(t, p.Push(10, []byte{0x01, 0xAA}))
	// a duplicate of seq 10 must not be treated as the matching second half
	require.Nil(t, p.Push(10, []byte{0x01, 0xAA}))

	frame := p.Push(11, []byte{0x01, 0xBB})
	require.NotNil(t, frame)
}

func TestOpusPairerHandlesSequenceWraparound(t *testing.T) {
	p := &opusPairer{}

	require.Nil(t, p.Push(0xFFFF, []byte{0x01, 0xAA}))
	frame := p.Push(0x0000, []byte{0x01, 0xBB})
	require.NotNil(t, frame)
}
