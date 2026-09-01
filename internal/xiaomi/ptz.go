package xiaomi

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/AlexxIT/go2rtc/internal/api"
	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/AlexxIT/go2rtc/pkg/xiaomi/miss"
)

type ptzController interface {
	PTZState() miss.PTZState
	Move(direction string) error
	StopMove() error
	Calibrate() error
	RefreshPosition(ctx context.Context) (*miss.PTZPosition, error)
	SetPosition(angle, elevation int) error
}

type ptzRequest struct {
	Action    string `json:"action"`
	Direction string `json:"direction"`
	Angle     *int   `json:"angle"`
	Elevation *int   `json:"elevation"`
}

type ptzResponse struct {
	Connected    bool              `json:"connected"`
	Position     *miss.PTZPosition `json:"position"`
	Limits       ptzLimits         `json:"limits"`
	CanLeft      bool              `json:"can_left"`
	CanRight     bool              `json:"can_right"`
	CanUp        bool              `json:"can_up"`
	CanDown      bool              `json:"can_down"`
	Capabilities ptzCapabilities   `json:"capabilities"`
}

type ptzLimits struct {
	AngleMin     int `json:"angle_min"`
	AngleMax     int `json:"angle_max"`
	ElevationMin int `json:"elevation_min"`
	ElevationMax int `json:"elevation_max"`
}

type ptzCapabilities struct {
	RelativeMove bool `json:"relative_move"`
	Stop         bool `json:"stop"`
	Position     bool `json:"position"`
	AbsoluteMove bool `json:"absolute_move"`
	Calibrate    bool `json:"calibrate"`
}

func apiPTZ(w http.ResponseWriter, r *http.Request) {
	ctrl, src, err := getPTZController(r.URL.Query().Get("src"))
	if err != nil {
		writePTZError(w, err)
		return
	}

	switch r.Method {
	case "GET":
		state := ctrl.PTZState()

		if refreshRaw := r.URL.Query().Get("refresh"); refreshRaw != "" {
			refresh, err := strconv.ParseBool(refreshRaw)
			if err != nil {
				http.Error(w, "invalid refresh value", http.StatusBadRequest)
				return
			}
			if refresh {
				ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
				defer cancel()

				log.Debug().Str("stream", src).Int("operation", 6).Msg("[xiaomi] ptz refresh")

				position, err := ctrl.RefreshPosition(ctx)
				if err != nil {
					log.Debug().Str("stream", src).Err(err).Msg("[xiaomi] ptz refresh failed")
					writePTZError(w, err)
					return
				}
				state.Position = position
				state.Connected = true
			}
		}

		api.ResponseJSON(w, buildPTZResponse(state))

	case "POST":
		var req ptzRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "malformed request body", http.StatusBadRequest)
			return
		}

		req.Action = strings.ToLower(req.Action)
		req.Direction = strings.ToLower(req.Direction)

		switch req.Action {
		case "move":
			if req.Direction == "" {
				http.Error(w, "direction is required", http.StatusBadRequest)
				return
			}
			if !isPTZDirection(req.Direction) {
				http.Error(w, "invalid direction", http.StatusBadRequest)
				return
			}
			log.Debug().Str("stream", src).Str("direction", req.Direction).Msg("[xiaomi] ptz move")
			err = ctrl.Move(req.Direction)
		case "stop":
			log.Debug().Str("stream", src).Int("operation", -1001).Msg("[xiaomi] ptz stop")
			err = ctrl.StopMove()
		case "calibrate":
			log.Debug().Str("stream", src).Int("operation", 5).Msg("[xiaomi] ptz calibrate")
			err = ctrl.Calibrate()
		case "refresh":
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()

			log.Debug().Str("stream", src).Int("operation", 6).Msg("[xiaomi] ptz refresh")

			position, err := ctrl.RefreshPosition(ctx)
			if err != nil {
				log.Debug().Str("stream", src).Err(err).Msg("[xiaomi] ptz refresh failed")
				writePTZError(w, err)
				return
			}
			state := ctrl.PTZState()
			state.Position = position
			state.Connected = true
			api.ResponseJSON(w, buildPTZResponse(state))
			return
		case "position":
			if req.Angle == nil {
				http.Error(w, "angle is required", http.StatusBadRequest)
				return
			}
			if req.Elevation == nil {
				http.Error(w, "elevation is required", http.StatusBadRequest)
				return
			}
			log.Debug().
				Str("stream", src).
				Int("operation", 13).
				Int("angle", *req.Angle).
				Int("elevation", *req.Elevation).
				Msg("[xiaomi] ptz absolute move")
			err = ctrl.SetPosition(*req.Angle, *req.Elevation)
		default:
			http.Error(w, "invalid action", http.StatusBadRequest)
			return
		}

		if err != nil {
			writePTZError(w, err)
			return
		}

		api.ResponseJSON(w, buildPTZResponse(ctrl.PTZState()))

	default:
		http.Error(w, "", http.StatusMethodNotAllowed)
	}
}

func isPTZDirection(direction string) bool {
	switch direction {
	case "left", "right", "up", "down":
		return true
	default:
		return false
	}
}

func getPTZController(src string) (ptzController, string, error) {
	if src == "" {
		return nil, "", errors.New("src is required")
	}

	stream := streams.Get(src)
	if stream == nil {
		return nil, src, errors.New(api.StreamNotFound)
	}

	active := stream.ActiveProducers()
	for _, producer := range active {
		if ctrl, ok := producer.(ptzController); ok {
			return ctrl, src, nil
		}
	}

	for _, source := range stream.Sources() {
		if strings.HasPrefix(source, "xiaomi://") {
			return nil, src, errors.New("xiaomi producer is not active")
		}
	}

	return nil, src, errors.New("stream is not Xiaomi")
}

func buildPTZResponse(state miss.PTZState) ptzResponse {
	resp := ptzResponse{
		Connected: state.Connected,
		Position:  state.Position,
		Limits: ptzLimits{
			AngleMin:     1,
			AngleMax:     101,
			ElevationMin: 1,
			ElevationMax: 101,
		},
		Capabilities: ptzCapabilities{
			RelativeMove: true,
			Stop:         true,
			Position:     true,
			AbsoluteMove: true,
			Calibrate:    true,
		},
	}

	if state.Position == nil {
		resp.CanLeft = true
		resp.CanRight = true
		resp.CanUp = true
		resp.CanDown = true
		return resp
	}

	resp.CanLeft = state.Position.Angle < 101
	resp.CanRight = state.Position.Angle > 1
	resp.CanUp = state.Position.Elevation < 101
	resp.CanDown = state.Position.Elevation > 1
	return resp
}

func writePTZError(w http.ResponseWriter, err error) {
	switch {
	case err == nil:
		return
	case errors.Is(err, context.DeadlineExceeded):
		http.Error(w, "response timeout", http.StatusGatewayTimeout)
	case errors.Is(err, context.Canceled):
		http.Error(w, err.Error(), http.StatusRequestTimeout)
	case errors.Is(err, net.ErrClosed):
		http.Error(w, "xiaomi producer disconnected", http.StatusServiceUnavailable)
	case err.Error() == api.StreamNotFound:
		http.Error(w, err.Error(), http.StatusNotFound)
	case err.Error() == "stream is not Xiaomi":
		http.Error(w, err.Error(), http.StatusBadRequest)
	case err.Error() == "xiaomi producer is not active":
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	case strings.Contains(err.Error(), "malformed motor response"):
		http.Error(w, err.Error(), http.StatusBadGateway)
	case strings.Contains(err.Error(), "invalid direction"),
		strings.Contains(err.Error(), "angle must be between"),
		strings.Contains(err.Error(), "elevation must be between"),
		strings.Contains(err.Error(), "src is required"):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
