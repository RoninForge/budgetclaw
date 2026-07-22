package goei

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultBaseURL is the production Goei origin. A repo pointer's endpoint normalizes to
// this when unset.
const DefaultBaseURL = "https://goei.roninforge.org"

// DefaultHost is the host of DefaultBaseURL, used to decide whether a repo pointer aims
// at the default hosted Goei or a self-hosted instance the user should be warned about.
const DefaultHost = "goei.roninforge.org"

// HostOf returns the host a pointer endpoint resolves to (empty -> DefaultHost), for
// display and trust decisions before any request is made.
func HostOf(endpoint string) string {
	if endpoint == "" {
		return DefaultHost
	}
	if u, err := url.Parse(baseOrigin(endpoint)); err == nil && u.Host != "" {
		return u.Host
	}
	return endpoint
}

// DeviceAuthStart is the response to POST /api/team/device/start: the device_code the
// CLI polls with, the short user_code the teammate confirms in a browser, and the URLs
// and timing to drive the flow.
type DeviceAuthStart struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	Interval                int    `json:"interval"`
	ExpiresIn               int    `json:"expires_in"`
	Team                    string `json:"team"`
}

// DeviceAuthPoll is the response to POST /api/team/device/poll. Status is one of
// pending, requested, approved, completed, denied, expired. Token is present exactly
// once, on the first poll after the lead approves.
type DeviceAuthPoll struct {
	Status string `json:"status"`
	Token  string `json:"token,omitempty"`
	Team   string `json:"team,omitempty"`
}

// baseOrigin normalizes a pointer endpoint to a scheme://host origin so the device
// endpoints attach cleanly whether the pointer gives a bare origin or a full URL.
// Falls back to DefaultBaseURL when empty. A scheme-less value like "team.example.com"
// is treated as https so it yields a usable https origin rather than a scheme-less
// string that would later fail with an opaque "unsupported protocol scheme" error.
func baseOrigin(endpoint string) string {
	if endpoint == "" {
		return DefaultBaseURL
	}
	u, err := url.Parse(endpoint)
	if err == nil && u.Scheme != "" && u.Host != "" {
		return u.Scheme + "://" + u.Host
	}
	// No scheme (or unparseable): retry as https, since a bare host is the common
	// mistake and https is the only scheme this service is served over.
	if u2, err2 := url.Parse("https://" + strings.TrimRight(endpoint, "/")); err2 == nil && u2.Host != "" {
		return u2.Scheme + "://" + u2.Host
	}
	return strings.TrimRight(endpoint, "/")
}

// IngestEndpointFor derives the ingest URL from a pointer endpoint so a token saved by
// `team join` points at the same server the join used.
func IngestEndpointFor(endpoint string) string {
	return baseOrigin(endpoint) + "/api/ingest"
}

func httpClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}

// StartDeviceAuth exchanges a join code for a device authorization. It mints no
// credential: the returned codes drive a browser confirmation and a lead approval
// before any token exists.
func StartDeviceAuth(ctx context.Context, endpoint, joinCode, machine, deviceName string) (*DeviceAuthStart, error) {
	u := baseOrigin(endpoint) + "/api/team/device/start"
	reqBody, err := json.Marshal(map[string]string{
		"join_code":   joinCode,
		"machine":     machine,
		"device_name": deviceName,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("contact %s: %w", u, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s", serverError(body, resp.StatusCode))
	}
	var out DeviceAuthStart
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode start response: %w", err)
	}
	if out.DeviceCode == "" || out.UserCode == "" {
		return nil, fmt.Errorf("server returned an incomplete authorization")
	}
	return &out, nil
}

// PollDeviceAuth checks the state of a device authorization. A 429 is treated as a
// transient "keep waiting" so the caller's poll loop backs off rather than failing.
func PollDeviceAuth(ctx context.Context, endpoint, deviceCode string) (*DeviceAuthPoll, error) {
	u := baseOrigin(endpoint) + "/api/team/device/poll"
	reqBody, err := json.Marshal(map[string]string{"device_code": deviceCode})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("contact %s: %w", u, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode == http.StatusTooManyRequests {
		return &DeviceAuthPoll{Status: "pending"}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s", serverError(body, resp.StatusCode))
	}
	var out DeviceAuthPoll
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode poll response: %w", err)
	}
	return &out, nil
}

// serverError extracts the server's {"error": "..."} message, falling back to the
// status code when the body has none.
func serverError(body []byte, status int) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		return e.Error
	}
	return fmt.Sprintf("server returned HTTP %d", status)
}
