package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func cacheLauncherFixture(t *testing.T) (string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("BoringCache operator launchers target Linux pods")
	}
	launcher, err := filepath.Abs("../../reference-workflows/boringcache/bin")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	capture := filepath.Join(dir, "argv")
	t.Setenv("CACHE_LAUNCHER_CAPTURE", capture)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	cli := `#!/usr/bin/env bash
printf '%s\0' "$@" > "$CACHE_LAUNCHER_CAPTURE"
printf '%s' "$GOMODCACHE" > "$CACHE_LAUNCHER_CAPTURE.modules"
while [[ $1 != -- ]]; do shift; done
shift
exec "$@"
`
	if err := os.WriteFile(filepath.Join(dir, "boringcache"), []byte(cli), 0o755); err != nil {
		t.Fatal(err)
	}
	broker := filepath.Join(dir, "broker.json")
	if err := os.WriteFile(broker, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BORINGCACHE_CI_BROKER_FILE", broker)
	t.Setenv("BORINGCACHE_WORKSPACE", "")
	t.Setenv("GOMODCACHE", filepath.Join(dir, "attempt private", "gomodcache"))
	return launcher, capture
}

func TestBoringCacheStagePreservesArgumentsPolicyAndChildFailure(t *testing.T) {
	for _, tc := range []struct{ policy, flag string }{{"", "--read-only"}, {"publish", "--write"}} {
		t.Run(tc.flag, func(t *testing.T) {
			launcher, capture := cacheLauncherFixture(t)
			t.Setenv("BORINGCACHE_CACHE_POLICY", tc.policy)
			cmd := exec.Command(filepath.Join(launcher, "boringcache-stage"), "--", "sh", "-c", "exit 17", "argument with spaces; not a command")
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 17 {
				t.Fatalf("child exit = %v, want 17; output=%s", err, out)
			}
			data, err := os.ReadFile(capture)
			if err != nil {
				t.Fatal(err)
			}
			got := strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
			want := []string{"go", "--profile", "remediation", tc.flag, "--no-git", "--fail-on-cache-error", "--", "sh", "-c", "exit 17", "argument with spaces; not a command"}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("argv = %#v, want %#v", got, want)
			}
			path, err := os.ReadFile(capture + ".modules")
			if err != nil || string(path) != os.Getenv("GOMODCACHE") {
				t.Fatalf("launcher replaced the attempt's module cache: %q, %v", path, err)
			}
		})
	}
}

func TestBoringCacheStageRequiresLocalSupervisor(t *testing.T) {
	launcher, capture := cacheLauncherFixture(t)
	t.Setenv("BORINGCACHE_CI_BROKER_FILE", "")
	cmd := exec.Command(filepath.Join(launcher, "boringcache-stage"), "--", "true")
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "inside boringcache ci run") {
		t.Fatalf("missing supervisor should stop before cache execution: %v %s", err, out)
	}
	if _, err := os.Stat(capture); !os.IsNotExist(err) {
		t.Fatal("launcher invoked the cache CLI without its supervisor")
	}
}

func TestBoringCacheCopilotPreflightNeedsNoCacheOrRepository(t *testing.T) {
	launcher, capture := cacheLauncherFixture(t)
	t.Setenv("BORINGCACHE_CI_BROKER_FILE", "")
	copilot := filepath.Join(t.TempDir(), "copilot-fixture")
	if err := os.WriteFile(copilot, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOBERS_COPILOT_BINARY", copilot)
	for _, args := range [][]string{{"--goobers-launcher-contract"}, {"--version"}, {"--help"}, {"-p", "Reply with exactly: ok", "--allow-all-tools", "--available-tools="}} {
		cmd := exec.Command(filepath.Join(launcher, "boringcache-copilot"), args...)
		cmd.Dir = t.TempDir()
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("probe %v failed: %v %s", args, err, out)
		}
		if args[0] == "--goobers-launcher-contract" && string(out) != "{\"version\":1,\"sessionMode\":\"adapter-managed\"}\n" {
			t.Fatalf("unexpected launcher contract: %s", out)
		}
	}
	if _, err := os.Stat(capture); !os.IsNotExist(err) {
		t.Fatal("preflight invoked the cache CLI")
	}
}

func TestBoringCacheCopilotKeepsNativeSessionArguments(t *testing.T) {
	launcher, capture := cacheLauncherFixture(t)
	t.Setenv("BORINGCACHE_CACHE_POLICY", "restore")
	child, err := exec.LookPath("true")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOBERS_COPILOT_BINARY", child)
	args := []string{"--session-id", "test-session", "-p", "test prompt"}
	if out, err := exec.Command(filepath.Join(launcher, "boringcache-copilot"), args...).CombinedOutput(); err != nil {
		t.Fatalf("launcher: %v %s", err, out)
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
	want := append([]string{child}, args...)
	if len(got) < len(want) || !reflect.DeepEqual(got[len(got)-len(want):], want) {
		t.Fatalf("launcher changed Copilot's arguments: %#v", got)
	}
}

func TestBoringCachePodSupervisorUsesExplicitOIDCProvider(t *testing.T) {
	for _, provider := range []string{"github-actions", "command"} {
		t.Run(provider, func(t *testing.T) {
			launcher, capture := cacheLauncherFixture(t)
			t.Setenv("BORINGCACHE_CI_BROKER_FILE", "")
			t.Setenv("GOOBERS_CACHE_OIDC_PROVIDER", provider)
			child, err := exec.LookPath("true")
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("GOOBERS_REAL_BINARY", child)
			// The wrapper passes this to the native CLI as one value. It must
			// never interpret shell syntax supplied in the token command.
			marker := filepath.Join(t.TempDir(), "must-not-exist")
			command := "cat /var/run/secrets/boringcache/token; touch " + marker
			t.Setenv("GOOBERS_CACHE_OIDC_TOKEN_COMMAND", command)
			args := []string{"__dispatch-exec", "argument with spaces"}
			if out, err := exec.Command(filepath.Join(launcher, "goobers"), args...).CombinedOutput(); err != nil {
				t.Fatalf("supervisor wrapper: %v %s", err, out)
			}
			data, err := os.ReadFile(capture)
			if err != nil {
				t.Fatal(err)
			}
			got := strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
			want := []string{"ci", "run", "--oidc-provider", provider}
			if provider == "command" {
				want = append(want, "--oidc-token-command", command)
			}
			want = append(want, "--", child)
			want = append(want, args...)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("supervisor argv = %#v, want %#v", got, want)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatal("wrapper evaluated the token command")
			}
		})
	}
}

func TestBoringCachePodSupervisorReusesBrokerAndDelegatesOtherCommands(t *testing.T) {
	for _, command := range []string{"__dispatch-exec", "validate"} {
		t.Run(command, func(t *testing.T) {
			launcher, capture := cacheLauncherFixture(t)
			child := filepath.Join(t.TempDir(), "goobers-real")
			if err := os.WriteFile(child, []byte("#!/bin/sh\nexit 17\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GOOBERS_REAL_BINARY", child)
			t.Setenv("GOOBERS_CACHE_OIDC_PROVIDER", "")
			if command != "__dispatch-exec" {
				t.Setenv("BORINGCACHE_CI_BROKER_FILE", "")
			}
			out, err := exec.Command(filepath.Join(launcher, "goobers"), command).CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 17 {
				t.Fatalf("child exit = %v, want 17; output=%s", err, out)
			}
			if _, err := os.Stat(capture); !os.IsNotExist(err) {
				t.Fatal("wrapper started a redundant supervisor")
			}
		})
	}
}

func TestBoringCachePodSupervisorRejectsMissingOrInvalidConfiguration(t *testing.T) {
	for _, mode := range []string{"provider", "token-command", "broker"} {
		t.Run(mode, func(t *testing.T) {
			launcher, capture := cacheLauncherFixture(t)
			t.Setenv("BORINGCACHE_CI_BROKER_FILE", "")
			t.Setenv("GOOBERS_CACHE_OIDC_PROVIDER", "")
			t.Setenv("GOOBERS_CACHE_OIDC_TOKEN_COMMAND", "")
			if mode == "token-command" {
				t.Setenv("GOOBERS_CACHE_OIDC_PROVIDER", "command")
			}
			if mode == "broker" {
				t.Setenv("BORINGCACHE_CI_BROKER_FILE", filepath.Join(t.TempDir(), "absent"))
				t.Setenv("GOOBERS_CACHE_OIDC_PROVIDER", "github-actions")
			}
			out, err := exec.Command(filepath.Join(launcher, "goobers"), "__dispatch-exec").CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 2 || len(out) == 0 {
				t.Fatalf("configuration error = %v, output=%s", err, out)
			}
			if _, err := os.Stat(capture); !os.IsNotExist(err) {
				t.Fatal("wrapper invoked the CLI after invalid configuration")
			}
		})
	}
}
