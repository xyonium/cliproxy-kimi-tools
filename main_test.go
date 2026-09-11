package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeAuthFile writes a cliproxy-shaped kimi auth JSON under t.TempDir().
func writeAuthFile(t *testing.T, dir, name string, disabled bool, accessToken, deviceID string) string {
	t.Helper()
	body := `{"access_token":"` + accessToken + `","device_id":"` + deviceID + `","disabled":`
	if disabled {
		body += "true"
	} else {
		body += "false"
	}
	body += `,"expired":"2027-01-01T00:00:00Z","refresh_token":"r","type":"oauth"}`
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	return p
}

func resetAll(t *testing.T) {
	t.Helper()
	resetCredsCacheForTest()
	sendUpstream = func(req hostHTTPRequest) (*hostHTTPResponse, error) {
		raw, err := CallHost("host.http.do", req)
		if err != nil {
			return nil, err
		}
		var resp hostHTTPResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			return nil, err
		}
		return &resp, nil
	}
	cfgMu.Lock()
	cfg = PluginConfig{}
	cfgMu.Unlock()
}

func TestLoadKimiCredentials_GlobPicksFirstEnabled(t *testing.T) {
	resetAll(t)
	dir := t.TempDir()
	writeAuthFile(t, dir, "kimi-b.json", true, "disabled_tok", "dev-b")
	writeAuthFile(t, dir, "kimi-a.json", false, "tok_a", "dev_a")

	creds, err := LoadKimiCredentials(filepath.Join(dir, "kimi-*.json"))
	if err != nil {
		t.Fatalf("LoadKimiCredentials: %v", err)
	}
	if creds.AccessToken != "tok_a" || creds.DeviceID != "dev_a" {
		t.Fatalf("got %+v, want tok_a/dev_a", creds)
	}
}

func TestLoadKimiCredentials_MtimeCacheInvalidation(t *testing.T) {
	resetAll(t)
	dir := t.TempDir()
	p := writeAuthFile(t, dir, "kimi-a.json", false, "tok_v1", "dev_1")
	pattern := filepath.Join(dir, "kimi-*.json")

	c1, _ := LoadKimiCredentials(pattern)
	if c1.AccessToken != "tok_v1" {
		t.Fatalf("first read: %+v", c1)
	}
	// Invalidate by changing mtime to force re-read of updated content.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(p, future, future); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	writeAuthFile(t, dir, "kimi-a.json", false, "tok_v2", "dev_2")
	if err := os.Chtimes(p, future.Add(time.Second), future.Add(time.Second)); err != nil {
		t.Fatalf("Chtimes 2: %v", err)
	}
	c2, _ := LoadKimiCredentials(pattern)
	if c2.AccessToken != "tok_v2" {
		t.Fatalf("after refresh: %+v, want tok_v2", c2)
	}
}

func TestLoadKimiCredentials_NonDictJSON(t *testing.T) {
	resetAll(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "kimi-bad.json")
	// Array, not object — must not crash, must classify as "no enabled".
	if err := os.WriteFile(p, []byte(`[1,2,3]`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKimiCredentials(filepath.Join(dir, "kimi-*.json")); err == nil {
		t.Fatal("want error for array-JSON auth file")
	}
}

func TestLoadKimiCredentials_NoAccessToken(t *testing.T) {
	resetAll(t)
	dir := t.TempDir()
	writeAuthFile(t, dir, "kimi-a.json", false, "", "")
	if _, err := LoadKimiCredentials(filepath.Join(dir, "kimi-*.json")); err == nil {
		t.Fatal("want error for missing access_token")
	}
}

func TestHandleMethod_RegisterReturnsMetadata(t *testing.T) {
	resetAll(t)
	raw, err := HandleMethod("plugin.register", []byte(`{"config_yaml":""}`))
	if err != nil {
		t.Fatalf("HandleMethod: %v", err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if !env.OK {
		t.Fatalf("register envelope not OK: %+v", env)
	}
	if !strings.Contains(string(env.Result), `"management_api":true`) {
		t.Fatalf("capabilities missing management_api: %s", env.Result)
	}
	if !strings.Contains(string(env.Result), `"Name":"kimi-tools"`) {
		t.Fatalf("metadata name wrong: %s", env.Result)
	}
}

func TestHandleMethod_ManagementRegistersRoute(t *testing.T) {
	resetAll(t)
	raw, err := HandleMethod("management.register", nil)
	if err != nil {
		t.Fatalf("HandleMethod: %v", err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if !env.OK || !strings.Contains(string(env.Result), `"Path":"/kimi/tools"`) {
		t.Fatalf("management.register envelope: %+v / %s", env, env.Result)
	}
}

// decodeManagementResponse decodes the ok envelope around a managementResponse.
func decodeManagementResponse(t *testing.T, raw []byte) managementResponse {
	t.Helper()
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if !env.OK {
		t.Fatalf("management envelope not OK: %+v", env)
	}
	var resp managementResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("unmarshal management response: %v", err)
	}
	return resp
}

func TestHandleKimiTools_MissingConfigReturnsClearError(t *testing.T) {
	resetAll(t)
	raw, err := HandleKimiTools(managementRequest{Method: "POST", Path: "/v0/management/kimi/tools", Body: []byte(`{}`)})
	if err != nil {
		t.Fatalf("HandleKimiTools: %v", err)
	}
	resp := decodeManagementResponse(t, raw)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if !strings.Contains(string(resp.Body), "kimi_auth_file not configured") {
		t.Fatalf("body: %s", resp.Body)
	}
}

func TestHandleKimiTools_ForwardsWithBearerAndDeviceID(t *testing.T) {
	resetAll(t)
	dir := t.TempDir()
	writeAuthFile(t, dir, "kimi-a.json", false, "bearer_xyz", "dev_top")

	cfgMu.Lock()
	cfg = PluginConfig{
		KimiAuthFile:    filepath.Join(dir, "kimi-*.json"),
		UpstreamBaseURL: "https://api.kimi.com/coding/v1/tools",
	}
	cfgMu.Unlock()

	captured := hostHTTPRequest{}
	sendUpstream = func(req hostHTTPRequest) (*hostHTTPResponse, error) {
		captured = req
		return &hostHTTPResponse{
			StatusCode: 200,
			Headers:    map[string][]string{"Content-Type": {"application/json"}},
			Body:       []byte(`{"is_success":true,"result":{}}`),
		}, nil
	}

	req := managementRequest{
		Method:  "POST",
		Path:    "/v0/management/kimi/tools",
		Headers: map[string][]string{"Authorization": {"Bearer mgmt_key"}},
		Body:    []byte(`{"method":"list_data_sources"}`),
	}
	raw, err := HandleKimiTools(req)
	if err != nil {
		t.Fatalf("HandleKimiTools: %v", err)
	}
	resp := decodeManagementResponse(t, raw)
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d body: %s", resp.StatusCode, resp.Body)
	}
	if captured.URL != "https://api.kimi.com/coding/v1/tools" {
		t.Fatalf("upstream URL: %s", captured.URL)
	}
	auth := captured.Headers["Authorization"]
	if len(auth) != 1 || auth[0] != "Bearer bearer_xyz" {
		t.Fatalf("Authorization: %v", auth)
	}
	dev := captured.Headers["X-Msh-Device-Id"]
	if len(dev) != 1 || dev[0] != "dev_top" {
		t.Fatalf("X-Msh-Device-Id: %v", dev)
	}
	if ua := captured.Headers["User-Agent"]; len(ua) != 1 || ua[0] != "kimi-datasource/3.4.0" {
		t.Fatalf("User-Agent: %v", ua)
	}
	if string(captured.Body) != `{"method":"list_data_sources"}` {
		t.Fatalf("forwarded body differs: %s", captured.Body)
	}
}

func TestHandleKimiTools_UnauthorizedTriggersOneReloadAndRetry(t *testing.T) {
	resetAll(t)
	dir := t.TempDir()
	authPath := writeAuthFile(t, dir, "kimi-a.json", false, "tok_old", "dev_1")

	cfgMu.Lock()
	cfg = PluginConfig{
		KimiAuthFile:    filepath.Join(dir, "kimi-*.json"),
		UpstreamBaseURL: "https://api.kimi.com/coding/v1/tools",
	}
	cfgMu.Unlock()

	// First call: 401. Keeper then "rotates" the file (new mtime + new token),
	// plugin must reload and retry once with the fresh bearer.
	calls := 0
	sendUpstream = func(req hostHTTPRequest) (*hostHTTPResponse, error) {
		calls++
		if calls == 1 {
			future := time.Now().Add(5 * time.Second)
			os.Chtimes(authPath, future, future)
			writeAuthFile(t, dir, "kimi-a.json", false, "tok_new", "dev_2")
			os.Chtimes(authPath, future.Add(time.Second), future.Add(time.Second))
			return &hostHTTPResponse{StatusCode: 401, Body: []byte(`{"error":"expired"}`)}, nil
		}
		if got := req.Headers["Authorization"]; len(got) != 1 || got[0] != "Bearer tok_new" {
			t.Fatalf("retry Authorization: %v, want tok_new", got)
		}
		if got := req.Headers["X-Msh-Device-Id"]; len(got) != 1 || got[0] != "dev_2" {
			t.Fatalf("retry X-Msh-Device-Id: %v, want dev_2", got)
		}
		return &hostHTTPResponse{StatusCode: 200, Body: []byte(`{"is_success":true}`)}, nil
	}

	raw, err := HandleKimiTools(managementRequest{
		Method: "POST", Path: "/v0/management/kimi/tools", Body: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("HandleKimiTools: %v", err)
	}
	resp := decodeManagementResponse(t, raw)
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d body: %s", resp.StatusCode, resp.Body)
	}
	if calls != 2 {
		t.Fatalf("expected 2 upstream calls, got %d", calls)
	}
}

func TestHandleKimiTools_MethodNotAllowed(t *testing.T) {
	resetAll(t)
	cfgMu.Lock()
	cfg = PluginConfig{KimiAuthFile: "dummy"}
	cfgMu.Unlock()
	raw, _ := HandleKimiTools(managementRequest{Method: "GET", Path: "/v0/management/kimi/tools"})
	resp := decodeManagementResponse(t, raw)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status: %d", resp.StatusCode)
	}
}

func TestNewUUID_FormatV4(t *testing.T) {
	id := newUUID()
	parts := strings.Split(id, "-")
	if len(parts) != 5 {
		t.Fatalf("uuid segments: %q", id)
	}
	if len(parts[0]) != 8 || len(parts[1]) != 4 || len(parts[2]) != 4 || len(parts[3]) != 4 || len(parts[4]) != 12 {
		t.Fatalf("uuid shape: %q", id)
	}
	// version nibble must be 4
	if parts[2][0] != '4' {
		t.Fatalf("version nibble: %q", parts[2])
	}
}
