package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/agentickit"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/harness"
)

// TestBoringCachePodValidation is run from a precompiled test binary in a new
// Kubernetes pod for each phase. Pod deletion owns the attempt cache cleanup;
// no cache volume is shared between the warm, remediation, and retry pods.
func TestBoringCachePodValidation(t *testing.T) {
	if os.Getenv("GOOBERS_BORINGCACHE_POD_VALIDATION") != "1" {
		t.Skip("set GOOBERS_BORINGCACHE_POD_VALIDATION=1 inside a validation pod")
	}
	if os.Getenv("KUBERNETES_SERVICE_HOST") == "" {
		t.Fatal("this validation requires a Kubernetes pod with disposable storage")
	}
	phase := os.Getenv("GOOBERS_BORINGCACHE_VALIDATION_PHASE")
	if phase != "warm" && phase != "remediate" && phase != "retry" {
		t.Fatal("GOOBERS_BORINGCACHE_VALIDATION_PHASE must be warm, remediate, or retry")
	}
	evidenceDir := os.Getenv("GOOBERS_BORINGCACHE_EVIDENCE_DIR")
	if !filepath.IsAbs(evidenceDir) {
		t.Fatal("GOOBERS_BORINGCACHE_EVIDENCE_DIR must be an absolute path")
	}
	if err := os.MkdirAll(evidenceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	broker := os.Getenv("BORINGCACHE_CI_BROKER_FILE")
	if info, err := os.Stat(broker); err != nil || !info.Mode().IsRegular() {
		t.Fatal("run the test inside the pod's native boringcache ci run supervisor")
	}
	workspace, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	goMod, err := os.ReadFile(filepath.Join(workspace, "go.mod"))
	if err != nil || !strings.HasPrefix(string(goMod), "module github.com/goobers/goobers\n") {
		t.Fatal("run the test from the original Goobers module root")
	}
	goSum, err := os.ReadFile(filepath.Join(workspace, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}

	attemptRoot, err := os.MkdirTemp("/cache", "goobers-boringcache-"+phase+"-")
	if err != nil {
		t.Fatal(err)
	}
	moduleCache := filepath.Join(attemptRoot, "go-mod")
	buildCache := filepath.Join(attemptRoot, "go-build")
	for _, dir := range []string{moduleCache, buildCache} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 0 {
			t.Fatalf("attempt cache %s must start empty", dir)
		}
	}
	t.Setenv("GOMODCACHE", moduleCache)
	t.Setenv("GOCACHE", buildCache)
	t.Setenv("GOTOOLCHAIN", "local")
	t.Setenv("GOOBERS_COPILOT_BINARY", "/opt/goobers-cache/copilot-validation-fixture")
	t.Setenv("GOOBERS_BORINGCACHE_UNDECLARED", "must-not-reach-the-harness")
	// No model or forge credential is needed by the deterministic CLI fixture.
	for _, name := range []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
		if os.Getenv(name) != "" {
			t.Fatalf("validation must not receive %s", name)
		}
	}
	for _, name := range []string{dispatcher.EnvDaemonAPI, dispatcher.EnvBlobEndpoint,
		dispatcher.EnvStageCapabilities, dispatcher.EnvStageWorkspace, dispatcher.EnvStageScript} {
		t.Setenv(name, "")
	}

	started := time.Now()
	var result apiv1.ResultEnvelope
	evidence := map[string]any{
		"phase": phase, "pod": os.Getenv("HOSTNAME"), "workspace": workspace,
		"attempt_root": attemptRoot, "module_cache": moduleCache, "build_cache": buildCache,
		"initial_module_files": 0, "initial_build_files": 0,
		"go_mod_sha256": boringCacheValidationDigest(goMod),
		"go_sum_sha256": boringCacheValidationDigest(goSum),
		"cleanup_owner": "Kubernetes pod deletion removes the private emptyDir volumes",
	}
	t.Cleanup(func() {
		evidence["elapsed_seconds"] = time.Since(started).Seconds()
		evidence["test_passed"] = !t.Failed()
		boringCacheValidationWriteJSON(t, filepath.Join(evidenceDir, "phase.json"), evidence)
	})
	logFile, err := os.Create(filepath.Join(evidenceDir, "stage.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	log := io.MultiWriter(os.Stdout, logFile)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	stage := "implement"
	if phase == "warm" {
		stage = "warm-module-cache"
	}
	// Retain the real pod recorder's transcript and native cache diagnostics.
	// The journal service is local to this test pod and has no forge authority.
	runDir := planeJournal(t, "boringcache-pod-validation", stage)
	t.Cleanup(func() {
		if err := os.CopyFS(filepath.Join(evidenceDir, "journal"), os.DirFS(runDir)); err != nil {
			t.Errorf("retain pod journal: %v", err)
		}
	})

	passthrough := []string{
		"BORINGCACHE_CI_BROKER_FILE", "BORINGCACHE_CACHE_POLICY", "BORINGCACHE_WORKSPACE",
		"GOOBERS_COPILOT_BINARY", "GOOBERS_BORINGCACHE_EVIDENCE_DIR",
	}
	if phase == "warm" {
		t.Setenv("BORINGCACHE_CACHE_POLICY", "publish")
		command, _ := json.Marshal([]string{"/opt/goobers-cache/boringcache-stage", "--", "go", "mod", "download"})
		allow, _ := json.Marshal(passthrough)
		t.Setenv(dispatcher.EnvStageCommand, string(command))
		t.Setenv(dispatcher.EnvStageTimeout, "300s")
		t.Setenv(dispatcher.EnvStageEnvDefaultDeny, "true")
		t.Setenv(dispatcher.EnvStageEnvAllow, string(allow))
		evidence["cache_policy"] = "publish"
		evidence["command"] = []string{"go", "mod", "download"}
		evidence["stage_timeout_seconds"] = 300
		result = runDeclaredStage(ctx, log, log)
	} else {
		t.Setenv("BORINGCACHE_CACHE_POLICY", "restore")
		t.Setenv("GOPROXY", "off")
		t.Setenv("GOSUMDB", "off")
		evidence["cache_policy"] = "restore"
		evidence["command"] = []string{"go", "test", "-mod=readonly", "-count=1", "./internal/agentickit"}
		evidence["go_proxy"] = "off"
		attempt := int32(1)
		if phase == "retry" {
			attempt = 2
			t.Setenv(dispatcher.EnvAttempt, "2")
		}
		kit := &agentickit.Kit{
			Envelope: apiv1.InvocationEnvelope{
				RunID: "boringcache-pod-validation", WorkflowID: "pr-remediation",
				TaskID: "implement", Attempt: attempt, Gaggle: "goobers", Goober: "cache-validation",
				Workspace: workspace, Goal: "Verify restored Go modules with the repository's focused tests.",
			},
			Goobers: map[string]apiv1.GooberSpec{
				"cache-validation": {Harness: apiv1.HarnessCopilot, TimeoutSeconds: 300},
			},
			Instructions:   map[string]string{"cache-validation": "Run the deterministic cache validation fixture and report its result. No model or forge access is required."},
			EnvPassthrough: passthrough,
			HarnessCommand: map[string][]string{string(apiv1.HarnessCopilot): {"/opt/goobers-cache/boringcache-copilot"}},
		}
		data, digest, err := agentickit.Marshal(kit)
		if err != nil {
			t.Fatal(err)
		}
		kit, err = agentickit.Unmarshal(data, digest)
		if err != nil {
			t.Fatal(err)
		}
		evidence["kit_digest"] = digest
		boringCacheValidationWriteJSON(t, filepath.Join(evidenceDir, "kit.json"), kit)
		executor, err := buildPodAgenticExecutor(kit, log, nil, t.TempDir())
		if err != nil {
			t.Fatalf("build pod executor: %v", err)
		}
		result, err = executor.Invoke(ctx, kit.Envelope)
		boringCacheValidationWriteJSON(t, filepath.Join(evidenceDir, "result.json"), result)
		if err != nil {
			t.Fatalf("invoke pod executor: %v", err)
		}
		if result.Transcript == nil {
			t.Fatal("pod harness did not record its transcript")
		}
		for _, name := range []string{"modulesRestored", "offlineGoTest", "compilerCacheConfigured", "credentialBoundaryPassed"} {
			if result.Outputs[name] != true {
				t.Fatalf("result output %s = %v, want true", name, result.Outputs[name])
			}
		}
		for name, want := range map[string]string{"module-cache.txt": moduleCache, "build-cache.txt": buildCache} {
			data, err := os.ReadFile(filepath.Join(evidenceDir, name))
			if err != nil || strings.TrimSpace(string(data)) != want {
				t.Fatalf("harness cache path %s did not preserve the attempt's private cache", name)
			}
		}
		if _, err := os.Stat(filepath.Join(workspace, ".goobers", "copilot-usage.json")); !os.IsNotExist(err) {
			t.Fatalf("harness did not remove its temporary usage output: %v", err)
		}
		evidence["harness_usage_output_removed"] = true
		if _, err := os.Stat(filepath.Join(workspace, harness.DefaultResultPath)); err != nil {
			t.Fatalf("harness completion file missing: %v", err)
		}
	}
	boringCacheValidationWriteJSON(t, filepath.Join(evidenceDir, "result.json"), result)
	if result.Status != apiv1.ResultSuccess || result.Error != nil {
		t.Fatalf("stage result = %s, error = %+v", result.Status, result.Error)
	}
	for name, original := range map[string][]byte{"go.mod": goMod, "go.sum": goSum} {
		current, err := os.ReadFile(filepath.Join(workspace, name))
		if err != nil || boringCacheValidationDigest(current) != boringCacheValidationDigest(original) {
			t.Fatalf("validation changed repository dependency input %s", name)
		}
	}
	var moduleFiles, moduleBytes, moduleZips int64
	err = filepath.WalkDir(moduleCache, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			moduleFiles++
			moduleBytes += info.Size()
			if strings.HasSuffix(path, ".zip") {
				moduleZips++
			}
		}
		return nil
	})
	if err != nil || moduleFiles == 0 || moduleZips == 0 {
		t.Fatalf("module cache was not populated: files=%d zips=%d error=%v", moduleFiles, moduleZips, err)
	}
	evidence["final_module_files"] = moduleFiles
	evidence["final_module_logical_bytes"] = moduleBytes
	evidence["final_module_zip_files"] = moduleZips
	t.Logf("phase %s succeeded with %d module files in a new private cache", phase, moduleFiles)
}

func boringCacheValidationDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func boringCacheValidationWriteJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Errorf("marshal %s: %v", filepath.Base(path), err)
		return
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Errorf("write %s: %v", filepath.Base(path), err)
	}
}
