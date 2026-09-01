package xiaomi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/AlexxIT/go2rtc/internal/api"
	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/xiaomi/miss"
	"github.com/stretchr/testify/require"
)

type fakePTZProducer struct {
	core.Connection

	mu        sync.Mutex
	state     miss.PTZState
	moveDir   string
	stopped   bool
	calibrate bool
	setAngle  int
	setTilt   int
	refreshFn func(context.Context) (*miss.PTZPosition, error)
}

func (p *fakePTZProducer) GetMedias() []*core.Media { return nil }
func (p *fakePTZProducer) GetTrack(*core.Media, *core.Codec) (*core.Receiver, error) {
	return nil, nil
}
func (p *fakePTZProducer) Start() error { return nil }
func (p *fakePTZProducer) Stop() error  { return nil }

func (p *fakePTZProducer) PTZState() miss.PTZState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state
}

func (p *fakePTZProducer) Move(direction string) error {
	p.mu.Lock()
	p.moveDir = direction
	p.mu.Unlock()
	return nil
}

func (p *fakePTZProducer) StopMove() error {
	p.mu.Lock()
	p.stopped = true
	p.mu.Unlock()
	return nil
}

func (p *fakePTZProducer) Calibrate() error {
	p.mu.Lock()
	p.calibrate = true
	p.mu.Unlock()
	return nil
}

func (p *fakePTZProducer) RefreshPosition(ctx context.Context) (*miss.PTZPosition, error) {
	if p.refreshFn != nil {
		return p.refreshFn(ctx)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state.Position, nil
}

func (p *fakePTZProducer) SetPosition(angle, elevation int) error {
	p.mu.Lock()
	p.setAngle = angle
	p.setTilt = elevation
	p.mu.Unlock()
	return nil
}

type fakePlainProducer struct{ core.Connection }

func (p *fakePlainProducer) GetMedias() []*core.Media { return nil }
func (p *fakePlainProducer) GetTrack(*core.Media, *core.Codec) (*core.Receiver, error) {
	return nil, nil
}
func (p *fakePlainProducer) Start() error { return nil }
func (p *fakePlainProducer) Stop() error  { return nil }

func setupStream(t *testing.T, name, source string, producer core.Producer) {
	t.Helper()

	streams.HandleFunc("test", func(string) (core.Producer, error) { return nil, nil })
	_, err := streams.Patch(name, source)
	require.NoError(t, err)

	stream := streams.Get(name)
	require.NotNil(t, stream)
	stream.AddProducer(producer)

	t.Cleanup(func() {
		streams.Delete(name)
	})
}

func TestBuildPTZResponseWithoutPosition(t *testing.T) {
	resp := buildPTZResponse(miss.PTZState{Connected: true})
	require.True(t, resp.Connected)
	require.Nil(t, resp.Position)
	require.True(t, resp.CanLeft)
	require.True(t, resp.CanRight)
	require.True(t, resp.CanUp)
	require.True(t, resp.CanDown)
}

func TestBuildPTZResponseWithPosition(t *testing.T) {
	resp := buildPTZResponse(miss.PTZState{
		Connected: true,
		Position:  &miss.PTZPosition{Angle: 101, Elevation: 1},
	})
	require.False(t, resp.CanLeft)
	require.True(t, resp.CanRight)
	require.True(t, resp.CanUp)
	require.False(t, resp.CanDown)
}

func TestAPIGetCachedState(t *testing.T) {
	producer := &fakePTZProducer{
		state: miss.PTZState{
			Connected: true,
			Position:  &miss.PTZPosition{Angle: 73, Elevation: 41, Ret: 0},
		},
	}
	setupStream(t, "ptz-get", "test://camera", producer)

	req := httptest.NewRequest(http.MethodGet, "/api/xiaomi/ptz?src=ptz-get", nil)
	w := httptest.NewRecorder()

	apiPTZ(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var resp ptzResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.True(t, resp.Connected)
	require.NotNil(t, resp.Position)
	require.Equal(t, 73, resp.Position.Angle)
}

func TestAPIRefresh(t *testing.T) {
	producer := &fakePTZProducer{
		state: miss.PTZState{Connected: true},
		refreshFn: func(context.Context) (*miss.PTZPosition, error) {
			return &miss.PTZPosition{Angle: 50, Elevation: 60, Ret: 0}, nil
		},
	}
	setupStream(t, "ptz-refresh", "test://camera", producer)

	req := httptest.NewRequest(http.MethodGet, "/api/xiaomi/ptz?src=ptz-refresh&refresh=true", nil)
	w := httptest.NewRecorder()

	apiPTZ(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), `"angle":50`)
	require.Contains(t, w.Body.String(), `"elevation":60`)
}

func TestAPIInvalidDirection(t *testing.T) {
	producer := &fakePTZProducer{state: miss.PTZState{Connected: true}}
	setupStream(t, "ptz-bad-dir", "test://camera", producer)

	req := httptest.NewRequest(http.MethodPost, "/api/xiaomi/ptz?src=ptz-bad-dir", strings.NewReader(`{"action":"move","direction":"bad"}`))
	req.Header.Set("Content-Type", api.MimeJSON)
	w := httptest.NewRecorder()

	apiPTZ(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Contains(t, w.Body.String(), "invalid direction")
}

func TestAPIMissingPositionFields(t *testing.T) {
	producer := &fakePTZProducer{state: miss.PTZState{Connected: true}}
	setupStream(t, "ptz-missing", "test://camera", producer)

	req := httptest.NewRequest(http.MethodPost, "/api/xiaomi/ptz?src=ptz-missing", strings.NewReader(`{"action":"position","angle":50}`))
	req.Header.Set("Content-Type", api.MimeJSON)
	w := httptest.NewRecorder()

	apiPTZ(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Contains(t, w.Body.String(), "elevation is required")
}

func TestAPIStreamNotXiaomi(t *testing.T) {
	setupStream(t, "ptz-other", "test://camera", &fakePlainProducer{})

	req := httptest.NewRequest(http.MethodGet, "/api/xiaomi/ptz?src=ptz-other", nil)
	w := httptest.NewRecorder()

	apiPTZ(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Contains(t, w.Body.String(), "stream is not Xiaomi")
}

func TestAPIProducerNotActive(t *testing.T) {
	streams.HandleFunc("xiaomi", func(string) (core.Producer, error) { return nil, nil })
	_, err := streams.Patch("ptz-inactive", "xiaomi://user:region@192.0.2.1?did=1&model=xiaomi.camera.c01a01")
	require.NoError(t, err)
	t.Cleanup(func() {
		streams.Delete("ptz-inactive")
	})

	req := httptest.NewRequest(http.MethodGet, "/api/xiaomi/ptz?src=ptz-inactive", nil)
	w := httptest.NewRecorder()

	apiPTZ(w, req)

	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.Contains(t, w.Body.String(), "xiaomi producer is not active")
}

func TestAPIInactiveNonXiaomiStream(t *testing.T) {
	streams.HandleFunc("testinactive", func(string) (core.Producer, error) { return nil, nil })
	_, err := streams.Patch("ptz-inactive-other", "testinactive://camera")
	require.NoError(t, err)
	t.Cleanup(func() {
		streams.Delete("ptz-inactive-other")
	})

	req := httptest.NewRequest(http.MethodGet, "/api/xiaomi/ptz?src=ptz-inactive-other", nil)
	w := httptest.NewRecorder()

	apiPTZ(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Contains(t, w.Body.String(), "stream is not Xiaomi")
}
