// Package main implements a CLIProxyAPI native plugin that forwards
// datasource-tool requests to the Kimi Datasource upstream
// (https://api.kimi.com/coding/v1/tools).
//
// The plugin registers a Management API route under /v0/management/kimi/tools.
// Clients (e.g. unified-finance-mcp with KIMI_PROXY_URL pointing here) send
// POSTs to that route; the plugin injects credentials read from the cliproxy
// auth file and relays the response verbatim.
//
// Auth model: the plugin never accepts the inbound Authorization header (that
// carries the CLIProxyAPI management key, already validated by the host
// middleware). Instead it reads `access_token` and `device_id` from
// `kimi_auth_file` on every request (mtime-cached). cliproxy's keeper keeps
// the auth file fresh; the plugin is a pure reader and never writes it.
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	crand "crypto/rand"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"
	"unsafe"

	"gopkg.in/yaml.v3"
)

const abiVersion uint32 = 1

const (
	defaultUpstreamBaseURL = "https://api.kimi.com/coding/v1/tools"
	datasourceVersion      = "3.4.0"
	userAgent              = "kimi-datasource/" + datasourceVersion
	maxRequestBodySize     = 10 << 20 // 10 MB; mirrors embeddings-forward
)

// ABIVersion mirrors the C ABI negotiated at init; exported for tests.
var ABIVersion = abiVersion

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// PluginConfig is parsed from the host-supplied config_yaml payload.
type PluginConfig struct {
	// KimiAuthFile is a glob for cliproxy kimi auth JSONs
	// (e.g. /CLIProxyAPI/auths/kimi-*.json). First matching, non-disabled
	// file with valid JSON wins.
	KimiAuthFile string `yaml:"kimi_auth_file"`

	// UpstreamBaseURL overrides the Kimi endpoint (testing / mirrors).
	UpstreamBaseURL string `yaml:"upstream_base_url"`
}

var (
	cfgMu sync.RWMutex
	cfg   PluginConfig
)

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if host == nil || plugin == nil {
		return 1
	}
	if uint32(host.abi_version) != abiVersion {
		return 1
	}
	C.store_host_api(host)
	plugin.abi_version = C.uint32_t(abiVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if method == nil {
		return 1
	}
	m := C.GoString(method)
	var reqBody []byte
	if request != nil && requestLen > 0 {
		if requestLen > C.size_t(math.MaxInt32) {
			writeResponse(response, errorEnvelope("handler_error", "request payload too large"))
			return 0
		}
		reqBody = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	result, err := HandleMethod(m, reqBody)
	if err != nil {
		writeResponse(response, errorEnvelope("handler_error", err.Error()))
		return 0
	}
	writeResponse(response, result)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, len C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
	_ = len
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

// registerJSON is the plugin's registration envelope returned to
// plugin.register / plugin.reconfigure. It lists config fields so the
// management UI can render a form, plus the management_api capability flag.
const registerJSON = `{"schema_version":1,"metadata":{"Name":"kimi-tools","Version":"0.1.0","Author":"xyonium","GitHubRepository":"https://github.com/xyonium/cliproxy-kimi-tools","ConfigFields":[{"Name":"kimi_auth_file","Type":"string","Description":"Glob for cliproxy kimi auth JSON. Example: /CLIProxyAPI/auths/kimi-*.json — first enabled file wins (access_token/device_id read per request, mtime-cached)."},{"Name":"upstream_base_url","Type":"string","Description":"Upstream Kimi datasource endpoint. Default https://api.kimi.com/coding/v1/tools."}]},"capabilities":{"management_api":true}}`

// HandleMethod dispatches an RPC method. Exported for unit testing.
func HandleMethod(method string, reqBody []byte) ([]byte, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		// Registration must succeed even on bad config so the host loads the
		// plugin; requests then produce a clear "not configured" error.
		_ = ParseConfig(reqBody)
		return okEnvelopeJSON(registerJSON)
	case "management.register":
		return okEnvelopeJSON(`{"routes":[{"Method":"POST","Path":"/kimi/tools"}]}`)
	case "management.handle":
		return HandleManagement(reqBody)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// ParseConfig extracts plugin settings from the config_yaml payload.
func ParseConfig(reqBody []byte) error {
	var req struct {
		ConfigYAML []byte `json:"config_yaml"`
	}
	if err := json.Unmarshal(reqBody, &req); err != nil {
		return fmt.Errorf("decode config request: %w", err)
	}
	var c PluginConfig
	if err := yaml.Unmarshal(req.ConfigYAML, &c); err != nil {
		return fmt.Errorf("parse config_yaml: %w", err)
	}
	if strings.TrimSpace(c.UpstreamBaseURL) == "" {
		c.UpstreamBaseURL = defaultUpstreamBaseURL
	}
	cfgMu.Lock()
	cfg = c
	cfgMu.Unlock()
	return nil
}

// GetConfig returns a copy of the current plugin configuration. Thread-safe.
func GetConfig() PluginConfig {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	return cfg
}

// managementRequest is the inbound request from ServeManagementHTTP.
// Field names match pluginapi.ManagementRequest (no JSON tags → Go field names).
type managementRequest struct {
	Method  string              `json:"Method"`
	Path    string              `json:"Path"`
	Headers map[string][]string `json:"Headers"`
	Query   map[string][]string `json:"Query"`
	Body    []byte              `json:"Body"`
}

// managementResponse is the outbound response for ServeManagementHTTP.
// Field names match pluginapi.ManagementResponse (no JSON tags → Go field names).
type managementResponse struct {
	StatusCode int                 `json:"StatusCode"`
	Headers    map[string][]string `json:"Headers"`
	Body       []byte              `json:"Body"`
}

// hostHTTPRequest is the payload for the host.http.do callback.
// JSON tags are lowercase to match host_callbacks.go.
type hostHTTPRequest struct {
	Method  string              `json:"method,omitempty"`
	URL     string              `json:"url,omitempty"`
	Headers map[string][]string `json:"headers,omitempty"`
	Body    []byte              `json:"body,omitempty"`
}

// hostHTTPResponse is the result from host.http.do.
type hostHTTPResponse struct {
	StatusCode int                 `json:"StatusCode"`
	Headers    map[string][]string `json:"Headers"`
	Body       []byte              `json:"Body"`
}

// sendUpstream performs one upstream HTTP call through the host ABI.
// Overridable in tests.
var sendUpstream = func(req hostHTTPRequest) (*hostHTTPResponse, error) {
	raw, err := CallHost("host.http.do", req)
	if err != nil {
		return nil, err
	}
	var resp hostHTTPResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode upstream response: %w", err)
	}
	return &resp, nil
}

// HandleManagement dispatches a management.handle request to the kimi route.
// Exported for unit testing.
func HandleManagement(reqBody []byte) ([]byte, error) {
	var mgmtReq managementRequest
	if err := json.Unmarshal(reqBody, &mgmtReq); err != nil {
		return okManagementResponse(http.StatusBadRequest, nil, errorJSONBody("failed to decode management request"))
	}
	if !strings.HasSuffix(mgmtReq.Path, "/kimi/tools") {
		return okManagementResponse(http.StatusNotFound, nil, errorJSONBody("unknown route: "+mgmtReq.Path))
	}
	return HandleKimiTools(mgmtReq)
}

// HandleKimiTools forwards one request to the configured Kimi datasource
// upstream. Read-only; inheriting per-request credentials from the auth file.
// Exported for unit testing.
func HandleKimiTools(mgmtReq managementRequest) ([]byte, error) {
	c := GetConfig()
	if strings.TrimSpace(c.KimiAuthFile) == "" {
		return okManagementResponse(http.StatusBadGateway, nil, errorJSONBody("kimi_auth_file not configured"))
	}
	if mgmtReq.Method != http.MethodPost {
		return okManagementResponse(http.StatusMethodNotAllowed, nil, errorJSONBody("only POST is supported"))
	}
	if len(mgmtReq.Body) > maxRequestBodySize {
		return okManagementResponse(http.StatusRequestEntityTooLarge, nil, errorJSONBody("request body too large"))
	}

	creds, err := LoadKimiCredentials(c.KimiAuthFile)
	if err != nil {
		// Do NOT leak auth-file internals in the response body — the message
		// deliberately strips path/parse detail to a generic instruction.
		return okManagementResponse(http.StatusBadGateway, nil, errorJSONBody("kimi credentials unavailable (check kimi_auth_file in plugin config and cliproxy login state)"))
	}

	// Strip any inbound Authorization — host middleware already validated the
	// management key; never relay it upstream. Same for Cookie.
	upstreamHeaders := map[string][]string{
		"Content-Type":          {"application/json"},
		"Authorization":         {"Bearer " + creds.AccessToken},
		"X-Msh-Device-Id":       {creds.DeviceID},
		"X-Msh-Platform":        {"kimi-code-cli"},
		"X-Msh-Version":         {datasourceVersion},
		"X-Msh-Tool-Call-Id":    {newUUID()},
		"User-Agent":            {userAgent},
		"X-Kimi-Proxy-Received": {time.Now().UTC().Format(time.RFC3339)},
	}

	upstreamURL := strings.TrimRight(c.UpstreamBaseURL, "/")
	httpResp, err := sendUpstream(hostHTTPRequest{
		Method:  http.MethodPost,
		URL:     upstreamURL,
		Headers: upstreamHeaders,
		Body:    mgmtReq.Body,
	})
	if err != nil {
		return okManagementResponse(http.StatusBadGateway, nil, errorJSONBody("upstream request failed"))
	}

	// On 401, cliproxy's keeper may have just rotated the auth file. Re-read
	// once and retry before giving up.
	if httpResp.StatusCode == http.StatusUnauthorized {
		if creds2, err2 := ReloadKimiCredentials(c.KimiAuthFile); err2 == nil && creds2.AccessToken != "" && creds2.AccessToken != creds.AccessToken {
			upstreamHeaders["Authorization"] = []string{"Bearer " + creds2.AccessToken}
			upstreamHeaders["X-Msh-Device-Id"] = []string{creds2.DeviceID}
			retry, err3 := sendUpstream(hostHTTPRequest{
				Method:  http.MethodPost,
				URL:     upstreamURL,
				Headers: upstreamHeaders,
				Body:    mgmtReq.Body,
			})
			if err3 == nil {
				httpResp = retry
			}
		}
	}
	return okManagementResponse(httpResp.StatusCode, filterResponseHeaders(httpResp.Headers), httpResp.Body)
}

// filterResponseHeaders lets a safe subset through and guarantees Content-Type.
func filterResponseHeaders(upstream map[string][]string) map[string][]string {
	allowed := map[string]bool{
		"Content-Type":     true,
		"X-Msh-Request-Id": true,
	}
	out := map[string][]string{}
	for k, v := range upstream {
		canonical := http.CanonicalHeaderKey(k)
		if allowed[canonical] {
			out[canonical] = v
		}
	}
	if _, ok := out["Content-Type"]; !ok {
		out["Content-Type"] = []string{"application/json"}
	}
	return out
}

// CallHost invokes a host callback method and returns the decoded result.
// Exported for unit testing; uses the C host API stored at init time.
func CallHost(method string, payload any) (json.RawMessage, error) {
	rawPayload, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return nil, fmt.Errorf("marshal host callback payload %s: %w", method, errMarshal)
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))

	var response C.cliproxy_buffer
	var requestPtr *C.uint8_t
	if len(rawPayload) > 0 {
		cPayload := C.CBytes(rawPayload)
		if cPayload == nil {
			return nil, fmt.Errorf("allocate host callback payload %s", method)
		}
		defer C.free(cPayload)
		requestPtr = (*C.uint8_t)(cPayload)
	}
	callCode := C.call_host_api(cMethod, requestPtr, C.size_t(len(rawPayload)), &response)
	var rawResponse []byte
	if response.ptr != nil && response.len > 0 {
		if response.len > C.size_t(math.MaxInt32) {
			C.free_host_buffer(response.ptr, response.len)
			return nil, fmt.Errorf("host callback %s response too large: %d bytes", method, uint64(response.len))
		}
		rawResponse = C.GoBytes(response.ptr, C.int(response.len))
	}
	if response.ptr != nil {
		C.free_host_buffer(response.ptr, response.len)
	}
	if len(rawResponse) == 0 {
		return nil, fmt.Errorf("host callback %s returned no response, code=%d", method, int(callCode))
	}

	var env envelope
	if errUnmarshal := json.Unmarshal(rawResponse, &env); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host callback envelope %s (code=%d): %w", method, int(callCode), errUnmarshal)
	}
	if callCode != 0 {
		if env.Error != nil {
			return nil, fmt.Errorf("%s: %s", env.Error.Code, env.Error.Message)
		}
		return nil, fmt.Errorf("host callback %s returned code=%d", method, int(callCode))
	}
	if !env.OK {
		if env.Error != nil {
			return nil, fmt.Errorf("%s: %s", env.Error.Code, env.Error.Message)
		}
		return nil, fmt.Errorf("host callback %s failed", method)
	}
	return append(json.RawMessage(nil), env.Result...), nil
}

func okManagementResponse(statusCode int, headers map[string][]string, body []byte) ([]byte, error) {
	if headers == nil {
		headers = map[string][]string{"Content-Type": {"application/json"}}
	}
	resp := managementResponse{
		StatusCode: statusCode,
		Headers:    headers,
		Body:       body,
	}
	return json.Marshal(envelope{OK: true, Result: json.RawMessage(mustMarshal(resp))})
}

func okEnvelopeJSON(result string) ([]byte, error) {
	return json.Marshal(envelope{OK: true, Result: json.RawMessage(result)})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

func errorJSONBody(msg string) []byte {
	type errBody struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	var e errBody
	e.Error.Message = msg
	e.Error.Type = "server_error"
	raw, _ := json.Marshal(e)
	return raw
}

func mustMarshal(v any) []byte {
	raw, _ := json.Marshal(v)
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}

// newUUID returns a random UUID string (v4) for X-Msh-Tool-Call-Id.
// Avoids pulling in a uuid dependency; the format is all upstream consumes.
func newUUID() string {
	var b [16]byte
	// rand.Read is fine: this is a tracing id, not a secret.
	if _, err := randRead(b[:]); err != nil {
		return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// randRead is crypto/rand.Read, overridable in tests.
var randRead = crand.Read
