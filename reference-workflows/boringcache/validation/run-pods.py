"""Run the Goobers stage executors in three sequential disposable pods."""

import json
import os
import subprocess
import time
from pathlib import Path

NAMESPACE = "goobers-cache-validation"
SECRET = "github-oidc-request"
PHASES = ("warm", "remediate", "retry")
OIDC_ENV = ("ACTIONS_ID_TOKEN_REQUEST_URL", "ACTIONS_ID_TOKEN_REQUEST_TOKEN")
PUBLIC_ENV = (
    "CI",
    "GITHUB_ACTIONS",
    "GITHUB_REPOSITORY",
    "GITHUB_REPOSITORY_ID",
    "GITHUB_REPOSITORY_OWNER",
    "GITHUB_REPOSITORY_OWNER_ID",
    "GITHUB_REF",
    "GITHUB_REF_NAME",
    "GITHUB_REF_TYPE",
    "GITHUB_SHA",
    "GITHUB_RUN_ID",
    "GITHUB_RUN_ATTEMPT",
    "GITHUB_RUN_NUMBER",
    "GITHUB_JOB",
    "GITHUB_WORKFLOW",
    "GITHUB_WORKFLOW_REF",
    "GITHUB_WORKFLOW_SHA",
    "GITHUB_EVENT_NAME",
    "GITHUB_SERVER_URL",
    "GITHUB_API_URL",
    "GITHUB_ACTOR",
    "GITHUB_ACTOR_ID",
)


def kubectl(*args, document=None, check=True):
    command = [
        "kubectl",
        "--context",
        "kind-" + os.environ["CLUSTER_NAME"],
        "--namespace",
        NAMESPACE,
        *args,
    ]
    result = subprocess.run(
        command,
        input=json.dumps(document) if document else None,
        text=True,
        capture_output=True,
        timeout=180,
        check=False,
    )
    if check and result.returncode:
        # Do not echo a failed Secret request body or provider credentials.
        if document and document.get("kind") == "Secret":
            raise RuntimeError("kubectl could not create the OIDC request Secret")
        raise RuntimeError("kubectl " + args[0] + " failed: " + result.stderr[:2000])
    return result


def write_json(path, value):
    path.write_text(json.dumps(value, indent=2) + "\n")


def pod_document(phase):
    env = [
        {"name": name, "value": os.environ[name]}
        for name in PUBLIC_ENV
        if os.environ.get(name)
    ]
    env.extend(
        {"name": name, "valueFrom": {"secretKeyRef": {"name": SECRET, "key": name}}}
        for name in OIDC_ENV
    )
    env.extend(
        {"name": name, "value": value}
        for name, value in {
            "GITHUB_WORKSPACE": "/workspace",
            "GOOBERS_BORINGCACHE_POD_VALIDATION": "1",
            "GOOBERS_BORINGCACHE_VALIDATION_PHASE": phase,
            "GOOBERS_BORINGCACHE_EVIDENCE_DIR": "/evidence",
            "BORINGCACHE_WORKSPACE": "boringcache/goobers-onboarding",
            "GOTOOLCHAIN": "local",
        }.items()
    )
    mounts = [
        {"name": name, "mountPath": "/" + name}
        for name in ("workspace", "tmp", "cache", "evidence")
    ]
    command = """set -u
boringcache ci run --oidc-provider github-actions -- /opt/goobers-cache/pod-validation.test -test.run '^TestBoringCachePodValidation$' -test.v -test.timeout=15m > /evidence/supervisor.log 2>&1
result=$?
cat /evidence/supervisor.log
printf '%s\\n' "$result" > /evidence/exit-code
sleep 600
"""
    return {
        "apiVersion": "v1",
        "kind": "Pod",
        "metadata": {"name": "cache-" + phase, "namespace": NAMESPACE},
        "spec": {
            "restartPolicy": "Never",
            "activeDeadlineSeconds": 1800,
            "terminationGracePeriodSeconds": 5,
            "automountServiceAccountToken": False,
            "securityContext": {
                "runAsUser": 10001,
                "runAsGroup": 10001,
                "runAsNonRoot": True,
                "seccompProfile": {"type": "RuntimeDefault"},
            },
            "initContainers": [
                {
                    "name": "source",
                    "image": os.environ["POD_IMAGE"],
                    "imagePullPolicy": "Never",
                    "command": ["cp", "-R", "/opt/goobers-source/.", "/workspace/"],
                    "volumeMounts": [mounts[0]],
                    "securityContext": {
                        "allowPrivilegeEscalation": False,
                        "capabilities": {"drop": ["ALL"]},
                    },
                }
            ],
            "containers": [
                {
                    "name": "stage",
                    "image": os.environ["POD_IMAGE"],
                    "imagePullPolicy": "Never",
                    "workingDir": "/workspace",
                    "command": ["bash", "-c", command],
                    "env": env,
                    "volumeMounts": mounts,
                    "securityContext": {
                        "allowPrivilegeEscalation": False,
                        "capabilities": {"drop": ["ALL"]},
                    },
                    "resources": {
                        "requests": {"cpu": "500m", "memory": "512Mi"},
                        "limits": {
                            "cpu": "2",
                            "memory": "4Gi",
                            "ephemeral-storage": "6Gi",
                        },
                    },
                }
            ],
            "volumes": [
                {"name": "workspace", "emptyDir": {"sizeLimit": "1Gi"}},
                {"name": "tmp", "emptyDir": {"medium": "Memory", "sizeLimit": "512Mi"}},
                {"name": "cache", "emptyDir": {"sizeLimit": "4Gi"}},
                {"name": "evidence", "emptyDir": {"sizeLimit": "256Mi"}},
            ],
        },
    }


def wait_for_result(name):
    deadline = time.monotonic() + 1200
    while time.monotonic() < deadline:
        result = kubectl(
            "exec", name, "-c", "stage", "--", "cat", "/evidence/exit-code", check=False
        )
        if result.returncode == 0:
            return int(result.stdout.strip())
        pod = json.loads(kubectl("get", "pod", name, "-o", "json").stdout)
        if pod.get("status", {}).get("phase") in ("Failed", "Succeeded"):
            raise RuntimeError(name + " stopped before writing a test result")
        time.sleep(10)
    raise TimeoutError(name + " did not finish within 20 minutes")


def run_phase(phase, evidence):
    name = "cache-" + phase
    directory = evidence / phase
    directory.mkdir()
    started = time.monotonic()
    kubectl("create", "-f", "-", document=pod_document(phase))
    pod = json.loads(kubectl("get", "pod", name, "-o", "json").stdout)
    receipt = {
        "phase": phase,
        "pod_name": name,
        "pod_uid": pod["metadata"]["uid"],
        "volumes": pod["spec"]["volumes"],
        "previous_pod_deleted": None if phase == "warm" else True,
    }
    try:
        code = wait_for_result(name)
        receipt["exit_code"] = code
        kubectl("cp", name + ":/evidence/.", str(directory), "-c", "stage")
        if code != 0:
            raise RuntimeError(name + " test exited with code " + str(code))
        phase_result = json.loads((directory / "phase.json").read_text())
        stage_result = json.loads((directory / "result.json").read_text())
        if (
            phase_result.get("test_passed") is not True
            or stage_result.get("status") != "success"
        ):
            raise RuntimeError(name + " did not report successful stage execution")
    finally:
        kubectl("cp", name + ":/evidence/.", str(directory), "-c", "stage", check=False)
        logs = kubectl("logs", name, "-c", "stage", check=False)
        (directory / "pod.log").write_text(logs.stdout)
        status = json.loads(kubectl("get", "pod", name, "-o", "json").stdout).get(
            "status", {}
        )
        write_json(directory / "pod-status.json", status)
        kubectl("delete", "pod", name, "--wait=true", "--timeout=120s")
        remaining = kubectl("get", "pod", name, "--ignore-not-found", "-o", "name")
        if remaining.stdout.strip():
            raise RuntimeError(name + " still exists after deletion")
        receipt["deleted_before_next_phase"] = True
        receipt["elapsed_seconds"] = time.monotonic() - started
        write_json(directory / "pod-receipt.json", receipt)
    return receipt


def main():
    if (
        os.environ.get("GITHUB_REPOSITORY") != "boringcache/Goobers"
        or os.environ.get("GITHUB_REF") != "refs/heads/boringcache-validation"
    ):
        raise RuntimeError("run this driver from the authorized fork validation branch")
    for name in OIDC_ENV:
        if not os.environ.get(name):
            raise RuntimeError("GitHub OIDC request environment is missing " + name)
    if not os.environ["CLUSTER_NAME"].startswith("goobers-pods-"):
        raise RuntimeError("the driver requires its dedicated disposable kind cluster")
    evidence = Path(os.environ["EVIDENCE_DIR"]).resolve()
    evidence.mkdir(parents=True, exist_ok=True)
    kubectl("create", "namespace", NAMESPACE)
    try:
        # Send only GitHub's job-scoped OIDC request credentials over stdin.
        # Each pod starts its own native broker; no host broker handle is copied.
        kubectl(
            "create",
            "-f",
            "-",
            document={
                "apiVersion": "v1",
                "kind": "Secret",
                "type": "Opaque",
                "metadata": {"name": SECRET, "namespace": NAMESPACE},
                "stringData": {name: os.environ[name] for name in OIDC_ENV},
            },
        )
        receipts = []
        for phase in PHASES:
            print("Starting " + phase + " in a new pod", flush=True)
            receipts.append(run_phase(phase, evidence))
            print("Completed " + phase + " and deleted its pod", flush=True)
        if len({receipt["pod_uid"] for receipt in receipts}) != len(PHASES):
            raise RuntimeError("validation phases reused a pod")
        write_json(
            evidence / "summary.json",
            {
                "passed": True,
                "phases": receipts,
                "module_downloads_disabled_in": ["remediate", "retry"],
                "shared_cache_volumes": False,
                "live_deployment": False,
            },
        )
    finally:
        kubectl("delete", "namespace", NAMESPACE, "--wait=true", "--timeout=120s")


if __name__ == "__main__":
    main()
