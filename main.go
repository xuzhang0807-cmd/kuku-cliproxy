// Package main implements the kuku CLIProxyAPI (CPA) dynamic plugin.
//
// kuku wraps 百度库库AI (kuku.baidu.com / GenFlowPro) as a CPA provider: it
// performs the Baidu passport login, mints and refreshes the STOKEN the upstream
// requires, and forwards OpenAI-compatible chat completion requests to the
// upstream /wenchain/genflowpro endpoints.
//
// Protocol knowledge is ported from the 0BSD-licensed kuku2api project
// (https://github.com/IKUNHEIHIE/kuku2api). The standalone Node service there
// owns its own HTTP server, API keys, SQLite console and ledger; this plugin
// keeps only the parts CPA does not already provide: login, credential
// handling, model discovery and the upstream executor.
//
// Built with -buildmode=c-shared and exports the cliproxy C ABI entry points.
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

// Wrappers so Go can invoke the host function-pointer table via cgo. The host
// API captured at init is used to push streaming chunks back asynchronously.
static int kuku_call_host(cliproxy_host_api* api, const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	return api->call(api->host_ctx, method, request, request_len, response);
}
static void kuku_free_host_buffer(cliproxy_host_api* api, void* ptr, size_t len) {
	api->free_buffer(ptr, len);
}

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const (
	// providerName is the provider id this plugin registers. The host derives
	// the plugin's own id from its file name (kuku.so), independent of this.
	providerName = "kuku"

	// upstreamBase is the GenFlowPro API host. Overridable for tests.
	defaultUpstreamBase = "https://kuku.baidu.com"
	// passportBase is Baidu's login host, used only to mint a STOKEN.
	defaultPassportBase = "https://passport.baidu.com"

	loginTTL = 15 * time.Minute
)

// -----------------------------------------------------------------------------
// Config
// -----------------------------------------------------------------------------

// pluginConfig mirrors the optional `plugins.configs.kuku` YAML block.
type pluginConfig struct {
	// ModelPrefix namespaces this provider's models ("<prefix>/<id>") so a
	// client can address them explicitly and they cannot be confused with a
	// model another provider already publishes.
	ModelPrefix string `yaml:"model_prefix"`
	// PublishMode decides how much of the discovered catalog is advertised.
	// "catalog" (default) publishes everything upstream lists; "allow"
	// publishes only extra_models plus publish_allow.
	PublishMode string `yaml:"publish_mode"`
	// PublishAllow lists catalog ids to keep when publish_mode is "allow".
	PublishAllow []string `yaml:"publish_allow"`
	// ExtraModels lists ids upstream serves but does not advertise.
	ExtraModels []string `yaml:"extra_models"`
	// DailyBonus toggles the daily credit claim. Off unless the key is present
	// and true; claiming is a write on the user's Baidu account and the CHAT
	// task's earn turn spends points.
	DailyBonus *bool `yaml:"daily_bonus"`
}

var (
	cfgMu           sync.Mutex
	cfgModelPrefix  string
	cfgPublishMode  string
	cfgPublishAllow []string
	cfgExtraModels  []string
)

// applyConfigYAML reads the host-supplied config block from a plugin.register /
// plugin.reconfigure request. Unknown or missing config is ignored so the
// plugin keeps working with an empty config.
func applyConfigYAML(raw []byte) {
	var envelope struct {
		ConfigYAML []byte `json:"config_yaml"`
	}
	if json.Unmarshal(raw, &envelope) != nil || len(envelope.ConfigYAML) == 0 {
		return
	}
	var cfg pluginConfig
	if yaml.Unmarshal(envelope.ConfigYAML, &cfg) != nil {
		return
	}
	cfgMu.Lock()
	defer cfgMu.Unlock()
	cfgModelPrefix = strings.TrimSpace(cfg.ModelPrefix)
	cfgPublishMode = strings.ToLower(strings.TrimSpace(cfg.PublishMode))
	cfgPublishAllow = cfg.PublishAllow
	cfgExtraModels = cfg.ExtraModels
	if cfg.DailyBonus != nil {
		applyDailyBonusConfig(*cfg.DailyBonus)
	}
}

func configuredModelPrefix() string {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	return cfgModelPrefix
}

func configuredPublish() (mode string, allow []string) {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	return cfgPublishMode, cfgPublishAllow
}

func configuredExtraModels() []string {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	return cfgExtraModels
}

// stripModelPrefix removes this plugin's routing prefix from a model id the
// client sent. The host uses the prefix to pick a credential and forwards the
// name unchanged, but upstream only knows the bare id.
func stripModelPrefix(id string) string {
	prefix := configuredModelPrefix()
	if prefix == "" {
		return id
	}
	return strings.TrimPrefix(id, prefix+"/")
}

// -----------------------------------------------------------------------------
// Host API (captured at init)
// -----------------------------------------------------------------------------

var (
	hostAPI *C.cliproxy_host_api // captured at init, used for async host calls

	// streamWG tracks the async stream pumps. The host dlcloses this library on
	// unload and a goroutine still running in an unloaded .so is a SIGSEGV, so
	// shutdown waits for them (see drainStreams).
	streamWG sync.WaitGroup
)

func main() {}

// -----------------------------------------------------------------------------
// C ABI exports
// -----------------------------------------------------------------------------

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	hostAPI = host
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, len C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	stopSigninScheduler()
	drainStreams()
}

// drainStreams waits briefly for in-flight stream pumps so the host does not
// dlclose this library under them.
// ponytail: bounded wait, no cancellation — a stream longer than the timeout can
// still be cut off at unload. Thread a context into the pump if that bites.
func drainStreams() {
	done := make(chan struct{})
	go func() { streamWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		hostLog("warn", "kuku: shutdown with a stream still in flight", nil)
	}
}

// writeResponse copies raw into the host-owned response buffer. The host frees
// it via cliproxyPluginFree.
func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}

// -----------------------------------------------------------------------------
// Host calls
// -----------------------------------------------------------------------------

// hostCall invokes a host RPC method via the function-pointer table captured at
// init. Used to push stream chunks back asynchronously (host.stream.emit /
// host.stream.close) and to persist credentials (host.auth.save).
func hostCall(method string, request []byte) ([]byte, error) {
	if hostAPI == nil || hostAPI.call == nil {
		return nil, fmt.Errorf("host API unavailable")
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var cReq unsafe.Pointer
	var reqLen C.size_t
	if len(request) > 0 {
		cReq = C.CBytes(request)
		defer C.free(cReq)
		reqLen = C.size_t(len(request))
	}
	var resp C.cliproxy_buffer
	rc := C.kuku_call_host(hostAPI, cMethod, (*C.uint8_t)(cReq), reqLen, &resp)
	var out []byte
	if resp.ptr != nil && resp.len > 0 {
		out = C.GoBytes(resp.ptr, C.int(resp.len))
	}
	if resp.ptr != nil && hostAPI.free_buffer != nil {
		C.kuku_free_host_buffer(hostAPI, resp.ptr, resp.len)
	}
	if rc != 0 {
		return out, fmt.Errorf("host call %s returned %d", method, int(rc))
	}
	return out, nil
}

// hostLog forwards a diagnostic line to the host logger so discovery problems
// show up in the CPA log instead of failing silently inside the plugin.
func hostLog(level, message string, fields map[string]any) {
	if hostAPI == nil || hostAPI.call == nil {
		return
	}
	if fields == nil {
		fields = map[string]any{}
	}
	body, _ := json.Marshal(map[string]any{"level": level, "message": message, "fields": fields})
	_, _ = hostCall(pluginabi.MethodHostLog, body)
}

// streamEmit pushes one chunk payload to the host stream. Returns an error if
// the host rejected it (e.g. the client already disconnected), which the pump
// uses to stop reading a dead upstream.
func streamEmit(streamID string, payload []byte) error {
	if streamID == "" {
		return fmt.Errorf("no stream id")
	}
	body, _ := json.Marshal(map[string]any{"stream_id": streamID, "payload": payload})
	_, err := hostCall(pluginabi.MethodHostStreamEmit, body)
	return err
}

func streamEmitError(streamID, message string) {
	if streamID == "" {
		return
	}
	errJSON, _ := json.Marshal(map[string]any{"error": map[string]any{"message": message}})
	_ = streamEmit(streamID, errJSON)
}

func streamClose(streamID string) {
	if streamID == "" {
		return
	}
	body, _ := json.Marshal(map[string]any{"stream_id": streamID})
	_, _ = hostCall(pluginabi.MethodHostStreamClose, body)
}

// saveAuthFile asks the host to persist a credential JSON as a physical auth
// file. This is the only way a login performed inside the plugin reaches the
// host's auth store.
func saveAuthFile(name string, payload []byte) error {
	body, _ := json.Marshal(map[string]any{"name": name, "json": json.RawMessage(payload)})
	_, err := hostCall(pluginabi.MethodHostAuthSave, body)
	return err
}

// -----------------------------------------------------------------------------
// Envelopes
// -----------------------------------------------------------------------------

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func okEnvelope(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

type identifierResponse struct {
	Identifier string `json:"identifier"`
}

// -----------------------------------------------------------------------------
// RPC dispatch
// -----------------------------------------------------------------------------

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		applyConfigYAML(request)
		// A hot reload quiesces us first, so this must be able to bring the
		// scheduler back after stopSigninScheduler.
		startSigninScheduler()
		return okEnvelope(kukuRegistration())
	case pluginabi.MethodPluginQuiesce:
		stopSigninScheduler()
		return okEnvelope(struct{}{})
	case pluginabi.MethodModelStatic:
		// The catalog lives behind a credential, so model.for_auth is the only
		// authoritative source. A bundled list would publish models no
		// credential can serve.
		return okEnvelope(pluginapi.ModelResponse{Provider: providerName})
	case pluginabi.MethodModelForAuth:
		return handleModelsForAuth(request)
	case pluginabi.MethodAuthIdentifier:
		return okEnvelope(identifierResponse{Identifier: providerName})
	case pluginabi.MethodAuthParse:
		return handleParseAuth(request)
	case pluginabi.MethodAuthLoginStart:
		return handleStartLogin(request)
	case pluginabi.MethodAuthLoginPoll:
		return handlePollLogin(request)
	case pluginabi.MethodAuthRefresh:
		return handleRefreshAuth(request)
	case pluginabi.MethodExecutorIdentifier:
		return okEnvelope(identifierResponse{Identifier: providerName})
	case pluginabi.MethodExecutorExecute:
		return handleExecExecute(request)
	case pluginabi.MethodExecutorExecuteStream:
		return handleExecStream(request)
	case pluginabi.MethodManagementRegister:
		return handleManagementRegister(request)
	case pluginabi.MethodManagementHandle:
		return handleManagement(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// -----------------------------------------------------------------------------
// Registration
// -----------------------------------------------------------------------------

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	ModelProvider         bool                         `json:"model_provider"`
	AuthProvider          bool                         `json:"auth_provider"`
	Executor              bool                         `json:"executor"`
	ExecutorModelScope    pluginapi.ExecutorModelScope `json:"executor_model_scope"`
	ExecutorInputFormats  []string                     `json:"executor_input_formats,omitempty"`
	ExecutorOutputFormats []string                     `json:"executor_output_formats,omitempty"`
	// ManagementAPI exposes the login/credits pages in the host UI. The host
	// only sends management.register when this is set.
	ManagementAPI bool `json:"management_api"`
}

func kukuRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             providerName,
			Version:          "0.1.0",
			Author:           "xuzhang0807-cmd (protocol ported from IKUNHEIHIE/kuku2api, 0BSD)",
			GitHubRepository: "https://github.com/xuzhang0807-cmd/kuku-cliproxy",
			Logo:             kukuLogoDataURL,
		},
		Capabilities: registrationCapability{
			ModelProvider:         true,
			AuthProvider:          true,
			Executor:              true,
			ExecutorModelScope:    pluginapi.ExecutorModelScopeBoth,
			ExecutorInputFormats:  []string{"chat-completions"},
			ExecutorOutputFormats: []string{"chat-completions"},
			ManagementAPI:         true,
		},
	}
}
