package goei

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStartDeviceAuthSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/team/device/start" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["join_code"] != "goei_jc_x" {
			t.Errorf("join_code not forwarded: %v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code":               "goei_dc_abc",
			"user_code":                 "WXYZ-2345",
			"verification_uri":          joinURI(r),
			"verification_uri_complete": joinURI(r) + "?code=WXYZ-2345",
			"interval":                  5,
			"expires_in":                900,
			"team":                      "acme",
		})
	}))
	defer srv.Close()

	res, err := StartDeviceAuth(context.Background(), srv.URL, "goei_jc_x", "mbp", "laptop")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if res.DeviceCode != "goei_dc_abc" || res.UserCode != "WXYZ-2345" || res.Team != "acme" {
		t.Fatalf("bad start response: %+v", res)
	}
	if res.Interval != 5 || res.ExpiresIn != 900 {
		t.Fatalf("bad timing: %+v", res)
	}
}

func joinURI(r *http.Request) string { return "http://" + r.Host + "/join" }

func TestStartDeviceAuthSurfacesServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "This join code is not valid or has been revoked."})
	}))
	defer srv.Close()

	_, err := StartDeviceAuth(context.Background(), srv.URL, "bad", "mbp", "laptop")
	if err == nil {
		t.Fatal("expected an error for an invalid code")
	}
	if err.Error() != "This join code is not valid or has been revoked." {
		t.Fatalf("server message not surfaced: %v", err)
	}
}

func TestPollDeviceAuthStatuses(t *testing.T) {
	var reply map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/team/device/poll" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(reply)
	}))
	defer srv.Close()

	reply = map[string]any{"status": "pending"}
	p, err := PollDeviceAuth(context.Background(), srv.URL, "goei_dc_x")
	if err != nil || p.Status != "pending" {
		t.Fatalf("pending: %v %+v", err, p)
	}

	reply = map[string]any{"status": "approved", "token": "goei_dt_tok", "team": "acme"}
	p, err = PollDeviceAuth(context.Background(), srv.URL, "goei_dc_x")
	if err != nil || p.Status != "approved" || p.Token != "goei_dt_tok" {
		t.Fatalf("approved: %v %+v", err, p)
	}
}

func TestPollDeviceAuthTooManyIsTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "Slow down"})
	}))
	defer srv.Close()

	// A 429 must not fail the poll loop; it maps to a keep-waiting "pending".
	p, err := PollDeviceAuth(context.Background(), srv.URL, "goei_dc_x")
	if err != nil {
		t.Fatalf("429 should be transient, got error: %v", err)
	}
	if p.Status != "pending" {
		t.Fatalf("429 should map to pending, got %q", p.Status)
	}
}
