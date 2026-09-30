package miss

import (
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/opus"
	"github.com/AlexxIT/go2rtc/pkg/pcm"
	"github.com/pion/rtp"
)

func (p *Producer) AddTrack(media *core.Media, _ *core.Codec, track *core.Receiver) error {
	if err := p.client.StartSpeaker(); err != nil {
		return err
	}
	// TODO: check this!!!
	time.Sleep(time.Second)

	sender := core.NewSender(media, track.Codec)

	switch track.Codec.Name {
	case core.CodecPCMA:
		var buf []byte

		if p.client.SpeakerCodec() == codecPCM {
			dst := &core.Codec{Name: core.CodecPCML, ClockRate: 8000}
			transcode := pcm.Transcode(dst, track.Codec)

			sender.Handler = func(pkt *rtp.Packet) {
				buf = append(buf, transcode(pkt.Payload)...)
				const size = 2 * 8000 * 0.040 // 16bit 40ms
				for len(buf) >= size {
					p.Send += size
					_ = p.client.WriteAudio(codecPCM, buf[:size])
					buf = buf[size:]
				}
			}
		} else {
			sender.Handler = func(pkt *rtp.Packet) {
				buf = append(buf, pkt.Payload...)
				const size = 8000 * 0.040 // 8bit 40 ms
				for len(buf) >= size {
					p.Send += size
					_ = p.client.WriteAudio(codecPCMA, buf[:size])
					buf = buf[size:]
				}
			}
		}
	case core.CodecOpus:
		if p.client.SpeakerCodec() == codecOPUS {
			pairer := &opusPairer{}
			sender.Handler = func(pkt *rtp.Packet) {
				if frame := pairer.Push(pkt.SequenceNumber, pkt.Payload); frame != nil {
					p.Send += len(frame)
					_ = p.client.WriteAudio(codecOPUS, frame)
				}
			}
		} else {
			sender.Handler = func(pkt *rtp.Packet) {
				p.Send += len(pkt.Payload)
				_ = p.client.WriteAudio(codecOPUS, pkt.Payload)
			}
		}
	}

	sender.HandleRTP(track)
	p.Senders = append(p.Senders, sender)
	return nil
}

// opusPairer joins two consecutive 20ms Opus RTP frames into one 40ms frame for the camera speaker.
// It resyncs on any loss, reorder or duplicate sequence number, so a single bad packet only drops
// one pairing instead of permanently misaligning every later frame for the rest of the stream.
type opusPairer struct {
	buf     []byte
	lastSeq uint16
	haveSeq bool
}

// Push returns the joined 40ms frame once two consecutive packets have been seen, or nil while
// waiting for the second half of the current pair.
func (o *opusPairer) Push(seq uint16, payload []byte) []byte {
	if o.haveSeq && seq != o.lastSeq+1 {
		o.buf = nil
	}
	o.lastSeq = seq
	o.haveSeq = true

	if o.buf == nil {
		o.buf = payload
		return nil
	}

	frame := opus.JoinFrames(o.buf, payload)
	o.buf = nil
	return frame
}
