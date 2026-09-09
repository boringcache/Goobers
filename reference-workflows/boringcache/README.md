# Goobers remediation cache integration

This opt-in integration restores Go modules into each remediation pod before its
command or Copilot session starts. A successful trusted warming stage publishes
modules before its pod is deleted. Later stages restore them into their own
empty directories. The Go adapter also configures compiler caching for the
child process. Neither a shared filesystem nor a persistent pod is required.

The change addresses the cross-stage persistence discussed in
[issue #4179](https://github.com/Agent-Clubhouse/Goobers/issues/4179).
The checked-in workflows are [reference configuration](../README.md), not a
copy of the deployed instance. Their existing download timeout and activity
reporting remain in place. This integration does not claim that the historical
90-minute failure still occurs unchanged.

## Runtime changes

The run-pinned agentic kit now carries the configured `runner.harnessCommand`
for its selected harness and the names in `runner.envPassthrough`. It carries
no environment values. The dispatched agentic executor passes these settings
to the normal harness registry and excludes dispatcher control variables from
the harness allowlist. This makes the existing Copilot launcher contract work
in a dispatched pod as it does on the daemon host.

The three executable wrappers in [bin](bin) have separate responsibilities:

- `goobers` starts the native BoringCache OIDC supervisor for `__dispatch-exec`
  only. Other Goobers commands call the real executable directly. An existing
  readable `BORINGCACHE_CI_BROKER_FILE` reuses the supervisor in that same pod;
  an explicitly configured unreadable handle fails. The native CLI validates
  a readable handle when the cache operation connects to its broker.
- `boringcache-stage` runs the native Go adapter with the `remediation`
  profile from [.boringcache.toml](../../.boringcache.toml). That profile reads
  the current `GOMODCACHE` through `path_env`, so the launcher does not replace
  the executor's cache directory. `BORINGCACHE_CACHE_POLICY=restore` selects
  `--read-only`; `publish` selects `--write`. Restore is the default. Cache
  errors fail the operation; a cold cache miss is permitted.
- `boringcache-copilot` implements the adapter-managed launcher contract and
  preserves Copilot's native session arguments. Contract, version, help, and
  the existing credential probe run directly, without a repository or cache
  connection. Actual sessions run through `boringcache-stage`.

The launcher uses released CLI v1.30.1. The existing modules benchmark and
compiler tag remain unchanged; pod persistence is not a new cold compiler
benchmark. `BORINGCACHE_WORKSPACE`, when set, overrides the plan's workspace
through the supported `--workspace` option.

## Operator configuration

1. Build the modified Goobers executable for the runner's Linux architecture.
   Put it at `goobers` in a temporary copy of this directory. Put the
   checksum-verified BoringCache v1.30.1 Linux binary at
   `boringcache-v1.30.1` in that same directory. Build [Dockerfile](Dockerfile)
   with `GOOBERS_IMAGE` set to the existing runner image by digest. That base
   must retain its normal Go and Copilot tools, Bash, and a numeric non-root
   user. The image preserves that user. Install this build on the daemon too,
   so it writes the added kit fields and can run launcher preflight checks.
   Install the Copilot wrapper and stage wrapper at `/opt/goobers-cache` on
   the daemon host used for those checks.
2. Merge [instance-fragment.yaml](instance-fragment.yaml) into the existing
   `runner` block. Add or update the chosen entry in the `runners` inventory
   with `host: goobers-cache-runner`, the name of the Deployment in
   [pod-template.yaml](pod-template.yaml). Keep the instance's existing engine
   connection and placement requirements. Declare the image's actual Go,
   shell, Copilot, and other required capabilities in that runner entry.
3. Adapt the Deployment template's namespace, image, and service account to
   the instance. Goobers reads this Deployment as a template and creates a
   fresh pod for each stage attempt; `replicas: 0` does not create a resident
   worker. The dispatcher invokes `goobers __dispatch-exec` on `PATH`, so an
   image `ENTRYPOINT` alone cannot install the supervisor.
4. Apply [warm-module-cache.patch](warm-module-cache.patch) to the matching
   reference configuration, or make its two equivalent edits in the live
   configuration. It wraps only the deterministic warming command and sets
   that stage to publish. The child command remains exactly
   `go mod download`, with `timeoutSeconds: 300`, `maxAttempts: 1`, and the
   existing transitions. Copilot sessions default to restore. Use publication
   only where the workload identity is authorized to write trusted content.

`runner.harnessCommand` applies to the whole instance, including Copilot stages
placed on a self runner. Configure affected stages to select the updated pod
runner, or launch their self execution inside its own native `boringcache ci run`
supervisor on that host. Installing the `goobers` wrapper does not supervise
self execution: it starts a supervisor only for `__dispatch-exec`. Without a
local broker, the Copilot cache launcher fails before starting the session.
A broker from another pod or host cannot supply this self-runner connection.

The template uses fresh disk-backed `emptyDir` storage with a 4 GiB size limit:
`GOMODCACHE=/cache/gomodcache` and `GOCACHE=/cache/gocache`. The default 512 MiB
memory-backed `/tmp` remains separate. Set the runner's `provides.memory` and
`provides.disk` limits, and stage minimums where needed, for the complete
workload; Goobers stamps resource requests and limits from those declarations
over the illustrative template values. The 4 GiB cache allocation must cover
restored modules, compiler files, and cache transfer work for that workload.

Goobers replaces the Linux pod security context during rendering, including
any template `fsGroup`. This example therefore does not depend on `fsGroup`.
Kubernetes creates a default disk `emptyDir` with writable directory permissions
([Kubernetes v1.37.0 implementation](https://github.com/kubernetes/kubernetes/blob/v1.37.0/pkg/volume/emptydir/empty_dir.go)).
Check that the deployed image's non-root user can create both cache directories
under the cluster's actual policy. Pod deletion removes the entire volume.
On a self runner, the executor removes its attempt's temporary scope. In a pod,
the executor leaves pod-owned temporary cleanup to pod deletion. Neither path
removes the pod-private `/cache` between commands in the same pod.

## OIDC and network configuration

Each pod must start its own native supervisor. A broker file is a handle to a
local supervisor, not a credential that can be copied from another pod or
GitHub runner. No static token fallback is configured.

For the operator template, set `GOOBERS_CACHE_OIDC_PROVIDER=command` and
`GOOBERS_CACHE_OIDC_TOKEN_COMMAND=cat /var/run/secrets/boringcache/token`.
The wrapper passes the token command as one argument to the native CLI; it
does not evaluate it in a shell. The projected service-account token has
audience `urn:boringcache:workload`. The Kubernetes issuer and subject must
already be registered and bound to the intended BoringCache workspace and
permissions. Creating this Deployment or service account does not enroll that
identity. The service account itself is operator-owned and is not created by
this example.

GitHub Actions uses `GOOBERS_CACHE_OIDC_PROVIDER=github-actions` instead. The
native `ci run` supervisor renews workload authorization and gives the child
its local broker handle. It removes its provider and static credential
environment from the child. The instance fragment passes the handle,
policy/workspace names, and an explicit list of public GitHub run metadata to
the harness. Those public fields keep cache operations attached to the workflow
run after Goobers applies its environment allowlist; they grant no cache
authority. Provider request credentials remain excluded. Other CI providers
need their equivalent public run metadata or the CLI's supported
`BORINGCACHE_CI_*` context inputs explicitly configured by the operator.

The pod needs egress to the configured BoringCache API and storage endpoints,
plus the issuer endpoints required by the chosen provider. Trusted warming
also needs the repository's Go module sources. Keep the existing forge and
model egress needed by a real Copilot session. A runner restricted by an
allowlist must explicitly include those destinations; the integration does
not widen network policy.

## Kubernetes validation

[BoringCache remediation pods](../../.github/workflows/boringcache-pods.yml)
builds this revision and creates a disposable Kubernetes cluster in GitHub
Actions. It runs a warming pod, deletes it, then runs a remediation pod and a
retry pod, deleting each before starting the next. Every pod has new private
workspace, temporary, and cache volumes. The test starts with empty module and
compiler directories and saves pod identities, cache sizes, native cache logs,
and completion results as workflow artifacts.

The warming phase executes the real `runDeclaredStage` path with the bounded
`go mod download` command. Remediation and retry deserialize a real kit and
execute `buildPodAgenticExecutor` with the configured Copilot wrapper. A
deterministic Copilot fixture checks for restored module files, runs
`go mod download` with `GOPROXY=off` and `GOSUMDB=off`, and runs the focused
`internal/agentickit` tests through the normal harness completion-file path.
It also uses released `boringcache ci context --json` to verify the run,
repository, attempt, ref, and commit after each execution path's environment
filter. It checks that compiler caching is configured; that alone is not a
measured compiler cache hit or a speed improvement.

This validation exercises the two Goobers execution paths inside separate Kubernetes
pods. It does not run a live daemon, call a language model, deploy an operator's
instance, or validate a separately registered Kubernetes issuer. The validation uses
the fork's already authorized GitHub Actions OIDC identity with the native
supervisor inside each pod. The prior fresh-runner benchmarks and their
reports remain separate evidence.
