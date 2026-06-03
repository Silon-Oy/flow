package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/Silon-Oy/flow/internal/auth"
)

// authDeviceStartResp matches the user-visible payload flowctl reads to decide
// what URL + code to print and how fast to poll.
type authDeviceStartResp struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

func (s *Server) handleDeviceStart(w http.ResponseWriter, r *http.Request) {
	if s.Auth == nil || s.Auth.ClientID == "" {
		writeErr(w, http.StatusServiceUnavailable,
			"device flow disabled: FLOW_GITHUB_OAUTH_CLIENT_ID not set on the central")
		return
	}
	ctx, cancel := withTimeout(r, 15*time.Second)
	defer cancel()
	start, err := s.Auth.StartDeviceLogin(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "device start: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, authDeviceStartResp{
		DeviceCode:      start.DeviceCode,
		UserCode:        start.UserCode,
		VerificationURI: start.VerificationURI,
		ExpiresIn:       start.ExpiresIn,
		Interval:        start.Interval,
	})
}

type authDevicePollReq struct {
	DeviceCode string `json:"device_code"`
}

// authDevicePollResp uses pending=true to encode "user hasn't entered the code
// yet" rather than HTTP 4xx so flowctl can keep polling without treating it as
// an error.
type authDevicePollResp struct {
	Pending      bool      `json:"pending"`
	SessionToken string    `json:"session_token,omitempty"`
	GitHubLogin  string    `json:"github_login,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
}

func (s *Server) handleDevicePoll(w http.ResponseWriter, r *http.Request) {
	if s.Auth == nil || s.Auth.ClientID == "" {
		writeErr(w, http.StatusServiceUnavailable,
			"device flow disabled: FLOW_GITHUB_OAUTH_CLIENT_ID not set on the central")
		return
	}
	var req authDevicePollReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if req.DeviceCode == "" {
		writeErr(w, http.StatusBadRequest, "device_code required")
		return
	}
	ctx, cancel := withTimeout(r, 15*time.Second)
	defer cancel()
	result, err := s.Auth.PollDeviceLogin(ctx, req.DeviceCode)
	switch {
	case err == nil:
		// fall through
	case errors.Is(err, auth.ErrSlowDown):
		// 429 nudges the client to widen its poll interval (the canonical
		// device-flow response to slow_down).
		writeErr(w, http.StatusTooManyRequests, "slow down")
		return
	case errors.Is(err, auth.ErrAccessDenied):
		writeErr(w, http.StatusForbidden, "access denied: user cancelled")
		return
	case errors.Is(err, auth.ErrExpiredToken):
		writeErr(w, http.StatusGone, "device code expired — restart login")
		return
	default:
		writeErr(w, http.StatusBadGateway, "device poll: "+err.Error())
		return
	}
	if result.Pending {
		writeJSON(w, http.StatusOK, authDevicePollResp{Pending: true})
		return
	}
	writeJSON(w, http.StatusOK, authDevicePollResp{
		SessionToken: result.SessionToken,
		GitHubLogin:  result.GitHubLogin,
		ExpiresAt:    result.ExpiresAt,
	})
}
