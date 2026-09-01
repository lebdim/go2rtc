package miss

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"sync"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/tutk"
	"github.com/AlexxIT/go2rtc/pkg/xiaomi/crypto"
	"github.com/AlexxIT/go2rtc/pkg/xiaomi/miss/cs2"
)

const (
	codecH264 = 4
	codecH265 = 5
	codecPCM  = 1024
	codecPCMU = 1026
	codecPCMA = 1027
	codecOPUS = 1032
)

type Conn interface {
	Protocol() string
	Version() string
	ReadCommand() (cmd uint32, data []byte, err error)
	WriteCommand(cmd uint32, data []byte) error
	ReadPacket() (hdr, payload []byte, err error)
	WritePacket(hdr, payload []byte) error
	RemoteAddr() net.Addr
	SetDeadline(t time.Time) error
	Close() error
}

func NewClient(rawURL string) (*Client, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}

	// 1. Check if we can create shared key.
	query := u.Query()
	key, err := crypto.CalcSharedKey(query.Get("device_public"), query.Get("client_private"))
	if err != nil {
		return nil, err
	}

	model := query.Get("model")

	// 2. Check if this vendor supported.
	var conn Conn
	switch s := query.Get("vendor"); s {
	case "cs2":
		conn, err = cs2.Dial(u.Host, query.Get("transport"))
	case "tutk":
		conn, err = tutk.Dial(u.Host, query.Get("uid"), "Miss", "client")
	default:
		err = fmt.Errorf("miss: unsupported vendor %s", s)
	}

	if err != nil {
		return nil, err
	}

	err = login(conn, query.Get("client_public"), query.Get("sign"))
	if err != nil {
		_ = conn.Close()
		return nil, err
	}

	client := &Client{
		Conn:  conn,
		key:   key,
		model: model,
		done:  make(chan struct{}),
	}
	// commandLoop both decodes PTZ motor responses and continuously drains
	// channel 0, which is what the buffer-overflow drain fix actually needed.
	go client.commandLoop()
	return client, nil
}

type Client struct {
	Conn
	key   []byte
	model string

	queryMu sync.Mutex
	stateMu sync.RWMutex

	position *PTZPosition
	waiter   chan motorResult
	readErr  error

	done      chan struct{}
	closeOnce sync.Once
}

const (
	cmdAuthReq           = 0x100
	cmdAuthRes           = 0x101
	cmdVideoStart        = 0x102
	cmdVideoStop         = 0x103
	cmdAudioStart        = 0x104
	cmdAudioStop         = 0x105
	cmdSpeakerStartReq   = 0x106
	cmdSpeakerStartRes   = 0x107
	cmdSpeakerStop       = 0x108
	cmdStreamCtrlReq     = 0x109
	cmdStreamCtrlRes     = 0x10A
	cmdGetAudioFormatReq = 0x10B
	cmdGetAudioFormatRes = 0x10C
	cmdPlaybackReq       = 0x10D
	cmdPlaybackRes       = 0x10E
	cmdDevInfoReq        = 0x110
	cmdDevInfoRes        = 0x111
	cmdMotorReq          = 0x112
	cmdMotorRes          = 0x113
	cmdEncoded           = 0x1001
)

func login(conn Conn, clientPublic, sign string) error {
	s := fmt.Sprintf(`{"public_key":"%s","sign":"%s","uuid":"","support_encrypt":0}`, clientPublic, sign)
	if err := conn.WriteCommand(cmdAuthReq, []byte(s)); err != nil {
		return err
	}

	_, data, err := conn.ReadCommand()
	if err != nil {
		return err
	}

	if !bytes.Contains(data, []byte(`"result":"success"`)) {
		return fmt.Errorf("miss: auth: %s", data)
	}

	return nil
}

func (c *Client) Version() string {
	return fmt.Sprintf("%s (%s)", c.Conn.Version(), c.model)
}

func (c *Client) WriteCommand(data []byte) error {
	data, err := crypto.Encode(data, c.key)
	if err != nil {
		return err
	}
	return c.Conn.WriteCommand(cmdEncoded, data)
}

func (c *Client) Close() error {
	var err error
	c.closeOnce.Do(func() {
		err = c.Conn.Close()
	})
	<-c.done
	return err
}

const (
	ModelDafang  = "isa.camera.df3"
	ModelLoockV2 = "loock.cateye.v02"
	ModelC200    = "chuangmi.camera.046c04"
	ModelC300    = "chuangmi.camera.72ac1"
	// ModelXiaofang looks like it has the same firmware as the ModelDafang.
	// There is also an older model "isa.camera.isc5" that only works with the legacy protocol.
	ModelXiaofang = "isa.camera.isc5c1"
)

// Xiaomi Home PTZ operations for MISS_CMD_MOTOR_REQ / MISS_CMD_MOTOR_RESP.
// angle/elevation are Xiaomi coordinates, not verified physical degrees.
// operation=13 and ret=-5 are plugin-observed, and cmd 0x112 + {"operation":2}
// was physically verified on xiaomi.camera.c01a01.
const (
	motorLeft     = 1
	motorRight    = 2
	motorUp       = 3
	motorDown     = 4
	motorCheck    = 5
	motorGet      = 6
	motorAbsolute = 13
	motorStop     = -1001
	motorCheckEnd = -5

	positionMin = 1
	positionMax = 101
)

type PTZPosition struct {
	Angle     int       `json:"angle"`
	Elevation int       `json:"elevation"`
	Ret       int       `json:"ret"`
	UpdatedAt time.Time `json:"updated_at"`
}

type PTZState struct {
	Connected bool
	Position  *PTZPosition
}

type motorPayload struct {
	Operation int  `json:"operation"`
	Angle     *int `json:"angle,omitempty"`
	Elevation *int `json:"elevation,omitempty"`
}

type motorResult struct {
	position *PTZPosition
	err      error
}

type motorResponse struct {
	Angle     *int `json:"angle"`
	Elevation *int `json:"elevation"`
	Ret       *int `json:"ret"`
}

func (c *Client) StartMedia(channel, quality, audio string) error {
	switch c.model {
	case ModelDafang, ModelXiaofang:
		var q, a byte
		if quality == "sd" {
			q = 1 // 0 - hd, 1 - sd, default - hd
		}
		if audio != "0" {
			a = 1 // 0 - off, 1 - on, default - on
		}

		return errors.Join(
			c.WriteCommand(dafangVideoQuality(q)),
			c.WriteCommand(dafangVideoStart(1, a)),
		)
	}

	// 0 - auto, 1 - sd, 2 - hd, default - hd
	switch quality {
	case "", "hd":
		// Some models have broken codec settings in quality 3.
		// Some models have low quality in quality 2.
		// Different models require different default quality settings.
		switch c.model {
		case ModelC200, ModelC300:
			quality = "3"
		default:
			quality = "2"
		}
	case "sd":
		quality = "1"
	case "auto":
		quality = "0"
	}

	if audio == "" {
		audio = "1"
	}

	data := binary.BigEndian.AppendUint32(nil, cmdVideoStart)
	switch channel {
	case "", "0":
		data = fmt.Appendf(data, `{"videoquality":%s,"enableaudio":%s}`, quality, audio)
	default:
		data = fmt.Appendf(data, `{"videoquality":-1,"videoquality2":%s,"enableaudio":%s}`, quality, audio)
	}
	return c.WriteCommand(data)
}

func (c *Client) StopMedia() error {
	data := binary.BigEndian.AppendUint32(nil, cmdVideoStop)
	return c.WriteCommand(data)
}

func (c *Client) StartAudio() error {
	data := binary.BigEndian.AppendUint32(nil, cmdAudioStart)
	return c.WriteCommand(data)
}

func (c *Client) StartSpeaker() error {
	data := binary.BigEndian.AppendUint32(nil, cmdSpeakerStartReq)
	return c.WriteCommand(data)
}

// SpeakerCodec if the camera model has a non-standard two-way codec.
func (c *Client) SpeakerCodec() uint32 {
	switch c.model {
	case ModelDafang, ModelXiaofang, "isa.camera.hlc6":
		return codecPCM
	case "chuangmi.camera.72ac1":
		return codecOPUS
	}
	return 0
}

func (c *Client) PTZState() PTZState {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()

	var position *PTZPosition
	if c.position != nil {
		copy := *c.position
		position = &copy
	}

	return PTZState{
		Connected: c.Connected(),
		Position:  position,
	}
}

func (c *Client) Connected() bool {
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

func (c *Client) Move(direction string) error {
	var operation int

	switch direction {
	case "left":
		operation = motorLeft
	case "right":
		operation = motorRight
	case "up":
		operation = motorUp
	case "down":
		operation = motorDown
	default:
		return fmt.Errorf("xiaomi ptz: invalid direction %q", direction)
	}

	return c.sendMotorCommand(operation, nil, nil)
}

func (c *Client) StopMove() error {
	return c.sendMotorCommand(motorStop, nil, nil)
}

func (c *Client) Calibrate() error {
	return c.sendMotorCommand(motorCheck, nil, nil)
}

func (c *Client) SetPosition(angle, elevation int) error {
	if err := validateTargetPosition(angle, elevation); err != nil {
		return err
	}
	return c.sendMotorCommand(motorAbsolute, &angle, &elevation)
}

func (c *Client) RefreshPosition(ctx context.Context) (*PTZPosition, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	c.queryMu.Lock()
	defer c.queryMu.Unlock()

	waiter := make(chan motorResult, 1)
	c.setWaiter(waiter)

	if err := c.sendMotorCommand(motorGet, nil, nil); err != nil {
		c.clearWaiter(waiter)
		return nil, err
	}

	select {
	case result := <-waiter:
		return result.position, result.err
	case <-ctx.Done():
		c.clearWaiter(waiter)
		return nil, ctx.Err()
	}
}

func validateTargetPosition(angle, elevation int) error {
	if angle < positionMin || angle > positionMax {
		return fmt.Errorf("xiaomi ptz: angle must be between %d and %d", positionMin, positionMax)
	}
	if elevation < positionMin || elevation > positionMax {
		return fmt.Errorf("xiaomi ptz: elevation must be between %d and %d", positionMin, positionMax)
	}
	return nil
}

func parseMotorResponse(data []byte) (*PTZPosition, error) {
	var response motorResponse
	if err := json.Unmarshal(data, &response); err != nil {
		return nil, fmt.Errorf("xiaomi ptz: malformed motor response: %w", err)
	}
	if response.Angle == nil || response.Elevation == nil || response.Ret == nil {
		return nil, errors.New("xiaomi ptz: malformed motor response: missing fields")
	}

	angle, elevation := *response.Angle, *response.Elevation
	if angle < 0 || angle > 101 || elevation < 0 || elevation > 101 {
		return nil, fmt.Errorf("xiaomi ptz: malformed motor response: angle=%d elevation=%d", angle, elevation)
	}

	return &PTZPosition{
		Angle:     angle,
		Elevation: elevation,
		Ret:       *response.Ret,
		UpdatedAt: time.Now().UTC(),
	}, nil
}

func motorCommandPayload(operation int, angle, elevation *int) ([]byte, error) {
	return json.Marshal(motorPayload{
		Operation: operation,
		Angle:     angle,
		Elevation: elevation,
	})
}

func (c *Client) sendMotorCommand(operation int, angle, elevation *int) error {
	payload, err := motorCommandPayload(operation, angle, elevation)
	if err != nil {
		return err
	}

	data := binary.BigEndian.AppendUint32(nil, cmdMotorReq)
	data = append(data, payload...)
	return c.WriteCommand(data)
}

func (c *Client) commandLoop() {
	defer close(c.done)

	for {
		cmd, data, err := c.Conn.ReadCommand()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				c.setCommandError(err)
			}
			c.resolveWaiter(motorResult{err: err})
			return
		}

		cmd, data, err = c.decodeCommand(cmd, data)
		if err != nil {
			c.resolveWaiter(motorResult{err: err})
			continue
		}

		if cmd != cmdMotorRes {
			continue
		}

		position, err := parseMotorResponse(data)
		if err == nil {
			c.stateMu.Lock()
			c.position = position
			c.stateMu.Unlock()
		}

		c.resolveWaiter(motorResult{position: position, err: err})
	}
}

func (c *Client) decodeCommand(cmd uint32, data []byte) (uint32, []byte, error) {
	if cmd != cmdEncoded {
		return cmd, data, nil
	}

	data, err := crypto.Decode(data, c.key)
	if err != nil {
		return 0, nil, err
	}
	if len(data) < 4 {
		return 0, nil, errors.New("xiaomi ptz: encoded command too small")
	}

	return binary.BigEndian.Uint32(data), data[4:], nil
}

func (c *Client) setWaiter(waiter chan motorResult) {
	c.stateMu.Lock()
	c.waiter = waiter
	c.stateMu.Unlock()
}

func (c *Client) clearWaiter(waiter chan motorResult) {
	c.stateMu.Lock()
	if c.waiter == waiter {
		c.waiter = nil
	}
	c.stateMu.Unlock()
}

func (c *Client) resolveWaiter(result motorResult) {
	c.stateMu.Lock()
	waiter := c.waiter
	c.waiter = nil
	c.stateMu.Unlock()

	if waiter != nil {
		waiter <- result
	}
}

func (c *Client) setCommandError(err error) {
	c.stateMu.Lock()
	if c.readErr == nil {
		c.readErr = err
	}
	c.stateMu.Unlock()
}

func (c *Client) commandError() error {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	return c.readErr
}

const hdrSize = 32

func (c *Client) ReadPacket() (*Packet, error) {
	hdr, payload, err := c.Conn.ReadPacket()
	if err != nil {
		return nil, fmt.Errorf("miss: read media: %w", err)
	}

	if len(hdr) < hdrSize {
		return nil, fmt.Errorf("miss: packet header too small")
	}

	payload, err = crypto.Decode(payload, c.key)
	if err != nil {
		return nil, err
	}

	pkt := &Packet{
		CodecID:  binary.LittleEndian.Uint32(hdr[4:]),
		Sequence: binary.LittleEndian.Uint32(hdr[8:]),
		Flags:    binary.LittleEndian.Uint32(hdr[12:]),
		Payload:  payload,
	}

	switch c.model {
	case ModelDafang, ModelXiaofang, ModelLoockV2:
		// Dafang has ts in sec
		// LoockV2 has ts in msec for video, but zero ts for audio
		pkt.Timestamp = uint64(time.Now().UnixMilli())
	default:
		pkt.Timestamp = binary.LittleEndian.Uint64(hdr[16:])
	}

	return pkt, nil
}

func (c *Client) WriteAudio(codecID uint32, payload []byte) error {
	payload, err := crypto.Encode(payload, c.key) // new payload will have new size!
	if err != nil {
		return err
	}

	n := uint32(len(payload))

	header := make([]byte, hdrSize)
	binary.LittleEndian.PutUint32(header, n)
	binary.LittleEndian.PutUint32(header[4:], codecID)
	binary.LittleEndian.PutUint64(header[16:], uint64(time.Now().UnixMilli())) // not really necessary
	return c.Conn.WritePacket(header, payload)
}

type Packet struct {
	//Length    uint32
	CodecID   uint32
	Sequence  uint32
	Flags     uint32
	Timestamp uint64 // msec
	//TimestampS uint32
	//Reserved uint32
	Payload []byte
}

func (p *Packet) SampleRate() uint32 {
	// flag:         1 0011 000 - sample rate 16000
	// flag: 100 00 01 0000 000 - sample rate  8000
	v := (p.Flags >> 3) & 0b1111
	if v != 0 {
		return 16000
	}
	return 8000
}

//func (p *Packet) AudioUnknown1() byte {
//	return byte((p.Flags >> 7) & 0b11)
//}
//
//func (p *Packet) AudioUnknown2() byte {
//	return byte((p.Flags >> 9) & 0b11)
//}

func dafangRaw(cmd uint32, args ...byte) []byte {
	payload := tutk.ICAM(cmd, args...)

	data := make([]byte, 4+len(payload)*2)
	copy(data, "\x7f\xff\xff\xff")
	hex.Encode(data[4:], payload)
	return data
}

// DafangVideoQuality 0 - hd, 1 - sd
func dafangVideoQuality(quality uint8) []byte {
	return dafangRaw(0xff07d5, quality)
}

func dafangVideoStart(video, audio uint8) []byte {
	return dafangRaw(0xff07d8, video, audio)
}

//func dafangLeft() []byte {
//	return dafangRaw(0xff2404, 2, 0, 5)
//}
//
//func dafangRight() []byte {
//	return dafangRaw(0xff2404, 1, 0, 5)
//}
//
//func dafangUp() []byte {
//	return dafangRaw(0xff2404, 0, 2, 5)
//}
//
//func dafangDown() []byte {
//	return dafangRaw(0xff2404, 0, 1, 5)
//}
//
//func dafangStop() []byte {
//	return dafangRaw(0xff2404, 0, 0, 5)
//}
