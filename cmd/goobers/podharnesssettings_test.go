package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/agentickit"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	harnesstest "github.com/goobers/goobers/test/testsupport/harness"
)

func TestWorkerKitCarriesConfiguredHarnessSettingsWithoutEnvironmentValues(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the launcher fixture uses a POSIX shell")
	}
	root := initDemo(t)
	launcher := filepath.Join(t.TempDir(), "operator-copilot")
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\nprintf '%s\\n' '{\"version\":1,\"sessionMode\":\"adapter-managed\"}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "instance.yaml")
	cfg, err := instance.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Runner.EnvPassthrough = []string{"OPERATOR_CACHE_BROKER"}
	cfg.Runner.HarnessCommand = map[string][]string{
		"copilot":     {launcher},
		"claude-code": {"unrelated-launcher"},
	}
	if err := instance.WriteConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPERATOR_CACHE_BROKER", "private-runtime-value-not-for-the-kit")
	seams := workerReloadSeams(t, root)
	writer := agenticKitWriter{instanceRoot: root, seams: seams}
	kit, err := writer.buildKit(apiv1.InvocationEnvelope{
		RunID: "launcher-run", TaskID: "implement", WorkflowID: pinWorkflow,
		Gaggle: pinGaggle, Goober: pinGoober, GooberDigest: currentPin(t, seams),
	}, agentickit.ModeInvoke)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(kit.EnvPassthrough, cfg.Runner.EnvPassthrough) {
		t.Fatalf("pod lost environment passthrough: %v", kit.EnvPassthrough)
	}
	wantCommand := map[string][]string{"copilot": {launcher}}
	if !reflect.DeepEqual(kit.HarnessCommand, wantCommand) {
		t.Fatalf("pod launcher = %v, want only its own harness: %v", kit.HarnessCommand, wantCommand)
	}
	data, _, err := agentickit.Marshal(kit)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private-runtime-value-not-for-the-kit") {
		t.Fatal("kit contains an ambient environment value")
	}
}

func TestPodHarnessUsesSettingsFromVerifiedKit(t *testing.T) {
	kit := &agentickit.Kit{
		Envelope:       apiv1.InvocationEnvelope{Goober: "coder"},
		Goobers:        map[string]apiv1.GooberSpec{"coder": {Harness: apiv1.HarnessCopilot}},
		Instructions:   map[string]string{"coder": "instructions"},
		EnvPassthrough: []string{"OPERATOR_CACHE_BROKER", "GOOBERS_POD_TOKEN", "goobers_pod_token"},
		HarnessCommand: map[string][]string{"copilot": {"operator-copilot", "--configured"}},
	}
	data, digest, err := agentickit.Marshal(kit)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := agentickit.Unmarshal(data, digest)
	if err != nil {
		t.Fatal(err)
	}
	previous := podHarnessRegistry
	t.Cleanup(func() { podHarnessRegistry = previous })
	called := false
	podHarnessRegistry = func(_ map[string]string, passthrough []string, command map[string][]string, _, _ string, _ bool, _ func(context.Context) (string, error), ephemeral bool) (*harness.Registry, error) {
		called = true
		if !reflect.DeepEqual(passthrough, []string{"OPERATOR_CACHE_BROKER"}) || !reflect.DeepEqual(command, kit.HarnessCommand) {
			t.Fatalf("pod ignored verified launcher settings: env=%v command=%v", passthrough, command)
		}
		if ephemeral {
			t.Fatal("pod should retain substrate-owned temp isolation")
		}
		registry := harness.NewRegistry()
		if err := registry.RegisterAs("copilot", &harnesstest.FakeAdapter{}); err != nil {
			return nil, err
		}
		return registry, nil
	}
	if _, err := buildPodAgenticExecutor(decoded, io.Discard, nil, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("pod did not construct its harness")
	}
}
