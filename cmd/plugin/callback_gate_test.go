//go:build cgo

package main

import (
	"strings"
	"testing"
	"time"
)

func preserveRuntimeStopFlag(t *testing.T) {
	t.Helper()
	lifecycleMu.Lock()
	previous := runtimeStopped
	lifecycleMu.Unlock()
	t.Cleanup(func() { lifecycleMu.Lock(); runtimeStopped = previous; lifecycleMu.Unlock() })
}

func TestShutdownDrainsForegroundHostCallbacks(t *testing.T) {
	preserveRuntimeStopFlag(t)
	resetRuntime()
	hostCallbackGate.Lock()
	hostCallbacksOpen = true
	hostCallbackGate.Unlock()
	// Model an in-flight foreground/log callback, independent of worker wg.
	hostCallbackGate.RLock()
	done := make(chan struct{})
	go func() { cliproxyPluginShutdown(); close(done) }()
	select {
	case <-done:
		hostCallbackGate.RUnlock()
		t.Fatal("shutdown did not drain callbacks")
	case <-time.After(20 * time.Millisecond):
	}
	hostCallbackGate.RUnlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not finish")
	}
	if _, err := callHost("host.log", nil); err == nil {
		t.Fatal("new callback accepted after shutdown")
	}
}

func TestShutdownBlocksAndRejectsReconfigure(t *testing.T) {
	preserveRuntimeStopFlag(t)
	resetRuntime()
	before := currentPlugin()
	hostCallbackGate.RLock()
	locked := true
	defer func() {
		if locked {
			hostCallbackGate.RUnlock()
		}
	}()
	stopped := make(chan struct{})
	go func() { cliproxyPluginShutdown(); close(stopped) }()
	// Once reset has published its empty runtime, shutdown is held at callback
	// draining. This is precisely the old race window, not a probabilistic race.
	deadline := time.Now().Add(time.Second)
	for currentPlugin() == before {
		if time.Now().After(deadline) {
			t.Fatal("shutdown did not reach draining")
		}
		time.Sleep(time.Millisecond)
	}
	empty := currentPlugin()
	configured := make(chan error, 1)
	go func() { configured <- configure([]byte(`{"schema_version":6}`)) }()
	select {
	case err := <-configured:
		t.Fatalf("reconfigure entered shutdown window: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	hostCallbackGate.RUnlock()
	locked = false
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("shutdown stalled")
	}
	select {
	case err := <-configured:
		if err == nil || !strings.Contains(err.Error(), "shut down") {
			t.Fatalf("reconfigure after shutdown: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("queued reconfigure stalled")
	}
	if currentPlugin() != empty {
		t.Fatal("new runtime published after shutdown")
	}
	resetRuntime() // Quiesce/reset must not reopen a terminal runtime.
	if err := configure([]byte(`{"schema_version":6}`)); err == nil {
		t.Fatal("reset reopened shutdown runtime")
	}
}
