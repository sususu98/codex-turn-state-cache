package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"

	"github.com/5345asda/codex-turn-state-cache/internal/prewarm"
	"github.com/5345asda/codex-turn-state-cache/internal/probe"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

var invokeProbeHost = callHost
var newProbeClient = func(path string) (prewarm.Host, error) {
	cwd, _ := os.Getwd()
	resolved, err := probe.ResolveHostConfigPath(path, os.Args, cwd, liveProcessEnv)
	if err != nil {
		return nil, err
	}
	return probe.New(existingHostCall, resolved, func() error { return probe.ValidateFileHost(os.Args, liveProcessEnv) })
}

// Only pre-existing read-only Host callbacks are permitted by this bridge.
// Credential JSON is decoded in memory; error text and raw replies are not logged.
// ctx is checked before/after the synchronous native call, but cannot preempt it.
// A blocked Host callback can therefore delay discovery, probes and shutdown
// beyond their context deadlines. Never abandon it in a goroutine: native unload
// must wait for every callback to return to avoid executing in an unloaded image.
func existingHostCall(ctx context.Context, method string, input, output any) error {
	switch method {
	case pluginabi.MethodHostAuthList, pluginabi.MethodHostAuthGetRuntime, pluginabi.MethodHostAuthGet:
	default:
		return errors.New("unsupported probe Host callback")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return errors.New("cannot encode account query")
	}
	raw, err := invokeProbeHost(method, payload)
	if err != nil {
		return errors.New("existing Host account callback unavailable")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var envelope pluginabi.Envelope
	if json.Unmarshal(raw, &envelope) != nil || !envelope.OK || envelope.Error != nil {
		return errors.New("Host account query failed")
	}
	if json.Unmarshal(envelope.Result, output) != nil {
		return errors.New("invalid Host account reply")
	}
	return nil
}
