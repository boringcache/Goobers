package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/instance"
)

func TestBoringCacheWorkflowPatchPreservesStageBoundsAndTransitions(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	paths := []string{
		"reference-workflows/gaggles/goobers/workflows/pr-remediation.yaml",
		"reference-workflows/gaggles/goobers/workflows/implementation.yaml",
	}
	for _, path := range paths {
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(work, path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, path), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("git", "apply", filepath.Join(root, "reference-workflows/boringcache/warm-module-cache.patch"))
	cmd.Dir = work
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("apply operator patch: %v %s", err, out)
	}
	for _, path := range paths {
		var original, patched apiv1.Workflow
		for file, target := range map[string]*apiv1.Workflow{filepath.Join(root, path): &original, filepath.Join(work, path): &patched} {
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if err := yaml.UnmarshalStrict(data, target); err != nil {
				t.Fatal(err)
			}
		}
		found := false
		for i := range patched.Spec.Tasks {
			task := &patched.Spec.Tasks[i]
			if task.Name != "warm-module-cache" {
				continue
			}
			found = true
			want := []string{"/opt/goobers-cache/boringcache-stage", "--", "go", "mod", "download"}
			if task.Run == nil || !reflect.DeepEqual(task.Run.Command, want) || task.Run.Env["BORINGCACHE_CACHE_POLICY"] != "publish" {
				t.Fatalf("%s did not install the publishing wrapper: %#v", path, task.Run)
			}
			task.Run.Command = original.Spec.Tasks[i].Run.Command
			task.Run.Env = original.Spec.Tasks[i].Run.Env
		}
		if !found || !reflect.DeepEqual(original, patched) {
			t.Fatalf("%s changed fields beyond the warming command and its cache policy", path)
		}
	}
}

func TestBoringCacheOperatorTemplateRendersWithPrivateCacheAndBrokerAllowlist(t *testing.T) {
	var deployment appsv1.Deployment
	var config instance.Config
	for file, target := range map[string]any{"pod-template.yaml": &deployment, "instance-fragment.yaml": &config} {
		data, err := os.ReadFile(filepath.Join("../../reference-workflows/boringcache", file))
		if err != nil {
			t.Fatal(err)
		}
		if err := yaml.UnmarshalStrict(data, target); err != nil {
			t.Fatal(err)
		}
	}
	runner := dispatcher.RunnerSpec{
		Name: "cache", OS: "linux", HostKind: instance.RunnerHostDeployment, Host: deployment.Name,
		Restrictions: []string{"tmp:ephemeral", "env:default-deny"},
	}
	pod, err := dispatcher.RenderFromTemplate(
		dispatcher.Config{Namespace: "fixture", EnvPassthrough: config.Runner.EnvPassthrough},
		dispatcher.Attempt{RunID: "fixture", Stage: "warm-module-cache", Number: 1}, runner, &deployment)
	if err != nil {
		t.Fatal(err)
	}
	stage := pod.Spec.Containers[0]
	if !reflect.DeepEqual(stage.Command, []string{"goobers"}) || !reflect.DeepEqual(stage.Args, []string{"__dispatch-exec"}) {
		t.Fatal("template no longer invokes the supervised executable")
	}
	var allowed []string
	for _, env := range stage.Env {
		if env.Name == dispatcher.EnvStageEnvAllow {
			if err := json.Unmarshal([]byte(env.Value), &allowed); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !slices.Contains(allowed, "BORINGCACHE_CI_BROKER_FILE") {
		t.Fatal("default-deny removes the local supervisor handle from the deterministic stage")
	}
	for _, name := range []string{"GITHUB_ACTIONS", "GITHUB_RUN_ID", "GITHUB_REPOSITORY", "GITHUB_RUN_ATTEMPT", "GITHUB_REF", "GITHUB_SHA"} {
		if !slices.Contains(allowed, name) {
			t.Fatalf("default-deny removes public execution metadata %s", name)
		}
	}
	for _, name := range []string{"ACTIONS_ID_TOKEN_REQUEST_URL", "ACTIONS_ID_TOKEN_REQUEST_TOKEN", "GITHUB_TOKEN", "COPILOT_GITHUB_TOKEN"} {
		if slices.Contains(allowed, name) {
			t.Fatalf("operator allowlist forwards a provider or model credential: %s", name)
		}
	}
	for _, volume := range pod.Spec.Volumes {
		if volume.Name == "cache-work" {
			if volume.EmptyDir == nil || volume.EmptyDir.Medium != "" || volume.EmptyDir.SizeLimit == nil || volume.EmptyDir.SizeLimit.String() != "4Gi" {
				t.Fatal("cache volume is not bounded, private disk storage")
			}
			return
		}
	}
	t.Fatal("renderer removed the private cache volume")
}
