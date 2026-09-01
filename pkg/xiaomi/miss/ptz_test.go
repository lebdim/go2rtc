package miss

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	xcrypto "github.com/AlexxIT/go2rtc/pkg/xiaomi/crypto"
	"github.com/stretchr/testify/require"
)

type fakeConn struct {
	reads  chan fakeRead
	writes chan fakeWrite
	once   sync.Once
}

type fakeRead struct {
	cmd  uint32
	data []byte
	err  error
}

type fakeWrite struct {
	cmd  uint32
	data []byte
}

func newFakeConn() *fakeConn {
	return &fakeConn{
		reads:  make(chan fakeRead, 16),
		writes: make(chan fakeWrite, 16),
	}
}

func (c *fakeConn) Protocol() string { return "fake" }
func (c *fakeConn) Version() string  { return "fake" }

func (c *fakeConn) ReadCommand() (uint32, []byte, error) {
	read, ok := <-c.reads
	if !ok {
		return 0, nil, io.EOF
	}
	return read.cmd, read.data, read.err
}

func (c *fakeConn) WriteCommand(cmd uint32, data []byte) error {
	c.writes <- fakeWrite{cmd: cmd, data: append([]byte(nil), data...)}
	return nil
}

func (c *fakeConn) ReadPacket() ([]byte, []byte, error) { return nil, nil, io.EOF }
func (c *fakeConn) WritePacket([]byte, []byte) error    { return nil }
func (c *fakeConn) RemoteAddr() net.Addr                { return &net.TCPAddr{} }
func (c *fakeConn) SetDeadline(time.Time) error         { return nil }

func (c *fakeConn) Close() error {
	c.once.Do(func() {
		close(c.reads)
	})
	return nil
}

func newTestClient(conn Conn, key []byte) *Client {
	return &Client{
		Conn: conn,
		key:  key,
		done: make(chan struct{}),
	}
}

func encodeRead(t *testing.T, key []byte, cmd uint32, payload string) fakeRead {
	t.Helper()

	data := binary.BigEndian.AppendUint32(nil, cmd)
	data = append(data, payload...)

	encoded, err := xcrypto.Encode(data, key)
	require.NoError(t, err)

	return fakeRead{cmd: cmdEncoded, data: encoded}
}

func decodeWrite(t *testing.T, key []byte, write fakeWrite) (uint32, string) {
	t.Helper()

	require.Equal(t, uint32(cmdEncoded), write.cmd)

	decoded, err := xcrypto.Decode(write.data, key)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(decoded), 4)

	return binary.BigEndian.Uint32(decoded[:4]), string(decoded[4:])
}

func TestMotorPayloads(t *testing.T) {
	key := make([]byte, 32)
	conn := newFakeConn()
	client := newTestClient(conn, key)

	tests := []struct {
		name      string
		run       func() error
		command   uint32
		jsonValue string
	}{
		{name: "left", run: func() error { return client.Move("left") }, command: cmdMotorReq, jsonValue: `{"operation":1}`},
		{name: "right", run: func() error { return client.Move("right") }, command: cmdMotorReq, jsonValue: `{"operation":2}`},
		{name: "up", run: func() error { return client.Move("up") }, command: cmdMotorReq, jsonValue: `{"operation":3}`},
		{name: "down", run: func() error { return client.Move("down") }, command: cmdMotorReq, jsonValue: `{"operation":4}`},
		{name: "calibrate", run: func() error { return client.Calibrate() }, command: cmdMotorReq, jsonValue: `{"operation":5}`},
		{name: "get", run: func() error { return client.sendMotorCommand(motorGet, nil, nil) }, command: cmdMotorReq, jsonValue: `{"operation":6}`},
		{name: "stop", run: func() error { return client.StopMove() }, command: cmdMotorReq, jsonValue: `{"operation":-1001}`},
		{name: "absolute", run: func() error { return client.SetPosition(50, 50) }, command: cmdMotorReq, jsonValue: `{"operation":13,"angle":50,"elevation":50}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.NoError(t, test.run())
			write := <-conn.writes
			cmd, payload := decodeWrite(t, key, write)
			require.Equal(t, test.command, cmd)
			require.Equal(t, test.jsonValue, payload)
		})
	}
}

func TestParseMotorResponse(t *testing.T) {
	position, err := parseMotorResponse([]byte(`{"angle":73,"elevation":41,"ret":0}`))
	require.NoError(t, err)
	require.Equal(t, 73, position.Angle)
	require.Equal(t, 41, position.Elevation)
	require.Equal(t, 0, position.Ret)
	require.False(t, position.UpdatedAt.IsZero())

	position, err = parseMotorResponse([]byte(`{"angle":73,"elevation":41,"ret":-5}`))
	require.NoError(t, err)
	require.Equal(t, motorCheckEnd, position.Ret)
}

func TestParseMotorResponseMalformed(t *testing.T) {
	_, err := parseMotorResponse([]byte(`{"angle":73,"ret":0}`))
	require.ErrorContains(t, err, "missing fields")

	_, err = parseMotorResponse([]byte(`{"angle":200,"elevation":41,"ret":0}`))
	require.ErrorContains(t, err, "malformed motor response")
}

func TestSetPositionValidation(t *testing.T) {
	require.NoError(t, validateTargetPosition(1, 1))
	require.NoError(t, validateTargetPosition(101, 101))
	require.ErrorContains(t, validateTargetPosition(0, 50), "angle must be between")
	require.ErrorContains(t, validateTargetPosition(102, 50), "angle must be between")
	require.ErrorContains(t, validateTargetPosition(50, 0), "elevation must be between")
	require.ErrorContains(t, validateTargetPosition(50, 102), "elevation must be between")
}

func TestRefreshPositionUpdatesCache(t *testing.T) {
	key := make([]byte, 32)
	conn := newFakeConn()
	client := newTestClient(conn, key)
	go client.commandLoop()
	defer func() {
		require.NoError(t, client.Close())
	}()

	result := make(chan *PTZPosition, 1)
	errc := make(chan error, 1)

	go func() {
		position, err := client.RefreshPosition(context.Background())
		if err != nil {
			errc <- err
			return
		}
		result <- position
	}()

	write := <-conn.writes
	cmd, payload := decodeWrite(t, key, write)
	require.Equal(t, uint32(cmdMotorReq), cmd)
	require.Equal(t, `{"operation":6}`, payload)

	conn.reads <- encodeRead(t, key, cmdMotorRes, `{"angle":73,"elevation":41,"ret":0}`)

	select {
	case err := <-errc:
		require.NoError(t, err)
	case position := <-result:
		require.Equal(t, 73, position.Angle)
		require.Equal(t, 41, position.Elevation)
	}

	state := client.PTZState()
	require.NotNil(t, state.Position)
	require.Equal(t, 73, state.Position.Angle)
}

func TestRefreshPositionSerializesWaiters(t *testing.T) {
	key := make([]byte, 32)
	conn := newFakeConn()
	client := newTestClient(conn, key)
	go client.commandLoop()
	defer func() {
		require.NoError(t, client.Close())
	}()

	type response struct {
		position *PTZPosition
		err      error
	}

	first := make(chan response, 1)
	second := make(chan response, 1)

	go func() {
		position, err := client.RefreshPosition(context.Background())
		first <- response{position: position, err: err}
	}()

	firstWrite := <-conn.writes
	_, payload := decodeWrite(t, key, firstWrite)
	require.Equal(t, `{"operation":6}`, payload)

	go func() {
		position, err := client.RefreshPosition(context.Background())
		second <- response{position: position, err: err}
	}()

	select {
	case <-conn.writes:
		t.Fatal("second refresh should wait for the first response")
	case <-time.After(100 * time.Millisecond):
	}

	conn.reads <- encodeRead(t, key, cmdMotorRes, `{"angle":73,"elevation":41,"ret":0}`)

	firstResp := <-first
	require.NoError(t, firstResp.err)
	require.Equal(t, 73, firstResp.position.Angle)

	secondWrite := <-conn.writes
	_, payload = decodeWrite(t, key, secondWrite)
	require.Equal(t, `{"operation":6}`, payload)

	conn.reads <- encodeRead(t, key, cmdMotorRes, `{"angle":40,"elevation":20,"ret":0}`)

	secondResp := <-second
	require.NoError(t, secondResp.err)
	require.Equal(t, 40, secondResp.position.Angle)

	state := client.PTZState()
	require.NotNil(t, state.Position)
	require.Equal(t, 40, state.Position.Angle)
}

func TestRefreshPositionTimeoutAndUnsolicitedUpdate(t *testing.T) {
	key := make([]byte, 32)
	conn := newFakeConn()
	client := newTestClient(conn, key)
	go client.commandLoop()
	defer func() {
		require.NoError(t, client.Close())
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := client.RefreshPosition(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	conn.reads <- encodeRead(t, key, cmdMotorRes, `{"angle":11,"elevation":22,"ret":0}`)

	require.Eventually(t, func() bool {
		state := client.PTZState()
		return state.Position != nil && state.Position.Angle == 11 && state.Position.Elevation == 22
	}, time.Second, 10*time.Millisecond)
}

func TestRefreshPositionDisconnected(t *testing.T) {
	key := make([]byte, 32)
	conn := newFakeConn()
	client := newTestClient(conn, key)
	go client.commandLoop()

	errc := make(chan error, 1)
	go func() {
		_, err := client.RefreshPosition(context.Background())
		errc <- err
	}()

	<-conn.writes
	require.NoError(t, conn.Close())

	err := <-errc
	require.Error(t, err)
	require.ErrorIs(t, err, io.EOF)
	require.NoError(t, client.Close())
}
