//go:build cgo

package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/5345asda/codex-turn-state-cache/internal/turnstate"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestRegisterDeclaresPluginContract(t *testing.T) {
	var registered registration
	callPluginMethod(t, pluginabi.MethodPluginRegister, lifecycleRequest{SchemaVersion: pluginabi.SchemaVersion}, &registered)
	if registered.SchemaVersion != pluginabi.SchemaVersion || registered.Metadata.Name != pluginID || registered.Metadata.Version != pluginVersion {
		t.Fatalf("registration = %#v", registered)
	}
	if registered.Metadata.Author != "5345asda" || registered.Metadata.GitHubRepository != "https://github.com/5345asda/codex-turn-state-cache" {
		t.Fatalf("metadata = %#v", registered.Metadata)
	}
	if !registered.Capabilities.RequestInterceptor || !registered.Capabilities.RequestLifecyclePlugin || !registered.Capabilities.ResponseInterceptor || !registered.Capabilities.StreamChunkInterceptor || !registered.Capabilities.WebSocketResponseObserver {
		t.Fatalf("capabilities = %#v", registered.Capabilities)
	}
}

func TestQuiesceClearsCachedState(t *testing.T) {
	configureTestPlugin(t)
	state := strings.Repeat("q", turnstate.StateLength)
	callPluginMethod(t, pluginabi.MethodRequestInterceptAfter, afterAuthRequest("capture", "auth-a", "gpt-5"), &pluginapi.RequestInterceptResponse{})
	callPluginMethod(t, pluginabi.MethodResponseInterceptAfter, pluginapi.ResponseInterceptRequest{
		RequestID:       "capture",
		ResponseHeaders: http.Header{turnstate.TurnStateHeader: {state}},
	}, &pluginapi.ResponseInterceptResponse{})
	callPluginMethod(t, pluginabi.MethodPluginQuiesce, struct{}{}, &struct{}{})

	var miss pluginapi.RequestInterceptResponse
	callPluginMethod(t, pluginabi.MethodRequestInterceptAfter, afterAuthRequest("after-quiesce", "auth-a", "gpt-5"), &miss)
	if len(miss.Headers) != 0 {
		t.Fatalf("quiesce did not clear cached state: %#v", miss.Headers)
	}
}

func configureTestPlugin(t *testing.T) {
	t.Helper()
	callPluginMethod(t, pluginabi.MethodPluginRegister, lifecycleRequest{
		SchemaVersion: pluginabi.SchemaVersion,
		ConfigYAML:    []byte("max_entries: 8\nmax_pending_entries: 8\n"),
	}, &registration{})
}

func afterAuthRequest(requestID, authID, model string) pluginapi.RequestInterceptRequest {
	return pluginapi.RequestInterceptRequest{
		RequestID: requestID,
		ToFormat:  "codex",
		Model:     model,
		Metadata:  map[string]any{"selected_auth_id": authID},
	}
}

func TestAcceptsTeamState332(t *testing.T) {
	configureTestPlugin(t)
	teamState := strings.Repeat("t", turnstate.StateLengthTeam)
	callPluginMethod(t, pluginabi.MethodRequestInterceptAfter, afterAuthRequest("capture-team", "auth-team", "gpt-6-astra"), &pluginapi.RequestInterceptResponse{})
	callPluginMethod(t, pluginabi.MethodResponseInterceptAfter, pluginapi.ResponseInterceptRequest{
		RequestID:       "capture-team",
		ResponseHeaders: http.Header{turnstate.TurnStateHeader: {teamState}},
	}, &pluginapi.ResponseInterceptResponse{})

	var hit pluginapi.RequestInterceptResponse
	callPluginMethod(t, pluginabi.MethodRequestInterceptAfter, afterAuthRequest("reuse-team", "auth-team", "gpt-6-astra"), &hit)
	if got := hit.Headers.Get(turnstate.TurnStateHeader); got != teamState {
		t.Fatalf("expected team state %q, got %q", teamState, got)
	}
}

func TestWebSocketResponseObserverRPC(t *testing.T) {
	configureTestPlugin(t)
	teamState := strings.Repeat("w", turnstate.StateLengthTeam)

	// Step 1: Bind after-auth
	callPluginMethod(t, pluginabi.MethodRequestInterceptAfter, afterAuthRequest("ws-rpc-1", "auth-ws", "gpt-6-astra"), &pluginapi.RequestInterceptResponse{})

	// Step 2: Observe WebSocket event via RPC
	metadataPayload := []byte(`{"type":"codex.response.metadata","headers":{"x-codex-turn-state":"` + teamState + `","x-codex-plan-type":"team"}}`)
	callPluginMethod(t, pluginabi.MethodWebSocketResponseEvent, webSocketResponseEventRPC{
		WebSocketResponseEvent: pluginapi.WebSocketResponseEvent{
			RequestID: "ws-rpc-1",
			EventType: "codex.response.metadata",
			Payload:   metadataPayload,
		},
	}, &struct{}{})

	// Step 3: Verify injection in next turn
	var hit pluginapi.RequestInterceptResponse
	callPluginMethod(t, pluginabi.MethodRequestInterceptAfter, afterAuthRequest("ws-rpc-2", "auth-ws", "gpt-6-astra"), &hit)
	if got := hit.Headers.Get(turnstate.TurnStateHeader); got != teamState {
		t.Fatalf("expected injected state %q, got %q", teamState, got)
	}
}

func callPluginMethod(t *testing.T, method string, request, target any) {
	t.Helper()
	rawRequest, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("marshal %s request: %v", method, err)
	}
	rawResponse, err := handleMethod(method, rawRequest)
	if err != nil {
		t.Fatalf("handle %s: %v", method, err)
	}
	var envelope pluginabi.Envelope
	if err := json.Unmarshal(rawResponse, &envelope); err != nil {
		t.Fatalf("decode %s envelope: %v", method, err)
	}
	if !envelope.OK || envelope.Error != nil {
		t.Fatalf("%s envelope = %s", method, rawResponse)
	}
	if err := json.Unmarshal(envelope.Result, target); err != nil {
		t.Fatalf("decode %s result: %v", method, err)
	}
}
