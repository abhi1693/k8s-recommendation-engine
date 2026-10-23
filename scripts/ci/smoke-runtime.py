"""Run the actual release image against isolated HTTPS and Git fixtures."""

import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import uuid

ROOT = Path(__file__).resolve().parents[2]
IMAGE, PLATFORM, VERSION = sys.argv[1:]
BINARY = "/usr/local/bin/k8s-recommendation-engine"
PYTHON_IMAGE = "python:3.14.9-alpine3.24@sha256:dbeb11d16bb95c3a7e3c25c842cb9f20cbc8c18f8be0461a4a0dcc861ad386a0"


def command(*args, check=True, timeout=90, **kwargs):
    result = subprocess.run(args, capture_output=True, text=True, timeout=timeout, **kwargs)
    if check and result.returncode:
        raise RuntimeError(f"Fixture command failed ({args[0]}): {result.stderr[-1800:]}")
    return result


assert json.loads((ROOT / "release.json").read_text())["version"] == VERSION
metadata = json.loads(command("docker", "image", "inspect", IMAGE).stdout)[0]
assert metadata["Config"]["User"] == "65532:65532"
assert metadata["Config"]["Labels"]["org.opencontainers.image.version"] == VERSION
assert command("docker", "run", "--rm", "--platform", PLATFORM, "--network", "none", IMAGE, "version").stdout.strip() == VERSION
command("docker", "run", "--rm", "--network", "none", IMAGE, "--help")

network = ""
containers = []
with tempfile.TemporaryDirectory(prefix="recommender-smoke-") as directory:
    fixture = Path(directory)
    fixture.chmod(0o755)
    try:
        command("openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1", "-subj", "/CN=Isolated CI Fixture", "-addext", "subjectAltName=DNS:fixture", "-addext", "basicConstraints=critical,CA:TRUE", "-keyout", str(fixture / "key.pem"), "-out", str(fixture / "cert.pem"))
        (fixture / "key.pem").chmod(0o400)
        (fixture / "cert.pem").chmod(0o644)
        bundle = command("docker", "run", "--rm", "--network", "none", "--entrypoint", "cat", IMAGE, "/etc/ssl/certs/ca-certificates.crt").stdout
        assert "BEGIN CERTIFICATE" in bundle
        (fixture / "trust.pem").write_text(bundle + (fixture / "cert.pem").read_text())
        (fixture / "runtime_fixture.py").write_text((ROOT / "scripts/ci/runtime_fixture.py").read_text())
        manifest = {
            "apiVersion": "apps/v1", "kind": "Deployment",
            "metadata": {"name": "example", "namespace": "fixture"},
            "spec": {"replicas": 2, "template": {"spec": {"containers": [{"name": "web", "resources": {"requests": {"cpu": "100m", "memory": "128Mi"}}}]}}},
        }
        profile = {
            "apiVersion": "k8s-recommendation-engine.io/v1alpha1", "kind": "ApplicationProfile",
            "metadata": {"name": "runtime-fixture"},
            "spec": {"namespace": "fixture", "git": {"provider": "github", "mode": "fleet", "repository": "https://fixture:8443/repo.git", "branch": "master", "basePath": "."}, "workloads": [{"name": "web", "targetRef": {"apiVersion": "apps/v1", "kind": "Deployment", "name": "example"}, "sourceFile": "app.yaml", "metricProfileRef": "resource", "scaling": {"replicas": False, "cpu": False, "memory": False}}]},
            "metricProfiles": {"resource": {"signals": {"cpu_usage": {"query": "vector(0.1)", "required": True}}}},
        }
        (fixture / "profile.json").write_text(json.dumps(profile))
        (fixture / "kubeconfig.json").write_text(json.dumps({
            "apiVersion": "v1", "kind": "Config", "current-context": "fixture",
            "clusters": [{"name": "fixture", "cluster": {"server": "https://fixture:8443", "certificate-authority": "/fixture/cert.pem"}}],
            "contexts": [{"name": "fixture", "context": {"cluster": "fixture", "user": "fixture"}}],
            "users": [{"name": "fixture", "user": {}}],
        }))
        seed = fixture / "seed"
        command("git", "init", "-q", "-b", "master", str(seed))
        command("git", "-C", str(seed), "config", "user.name", "Synthetic fixture")
        command("git", "-C", str(seed), "config", "user.email", "fixture@example.invalid")
        (seed / "app.yaml").write_text(json.dumps(manifest))
        command("git", "-C", str(seed), "add", "app.yaml")
        command("git", "-C", str(seed), "commit", "-qm", "Synthetic baseline")
        command("git", "clone", "-q", "--bare", str(seed), str(fixture / "repo.git"))
        command("git", "--git-dir", str(fixture / "repo.git"), "update-server-info")
        for path in fixture.rglob("*"):
            if path.name == "key.pem":
                continue
            path.chmod(0o755 if path.is_dir() else 0o644)
        # Register names before Docker calls so a CLI timeout cannot leave an
        # untracked container/network behind during the cleanup path.
        network = "recommender-smoke-" + uuid.uuid4().hex[:12]
        command("docker", "network", "create", "--internal", network)
        mount = ["--mount", f"type=bind,src={fixture},dst=/fixture,readonly"]
        service = network + "-fixture"
        containers.append(service)
        command("docker", "run", "-d", "--rm", "--name", service, "--network", network, "--network-alias", "fixture", "--user", f"{os.getuid()}:{os.getgid()}", "--read-only", "--cap-drop", "ALL", *mount, PYTHON_IMAGE, "python", "/fixture/runtime_fixture.py", timeout=180)
        ready = False
        for _ in range(40):
            probe = command("docker", "exec", service, "python", "-c", "import ssl,urllib.request;urllib.request.urlopen('https://fixture:8443/ci-stats',context=ssl.create_default_context(cafile='/fixture/cert.pem'),timeout=2).read()", check=False, timeout=8)
            if probe.returncode == 0:
                ready = True
                break
            time.sleep(0.5)
        assert ready, "HTTPS fixture never became ready"

        analyze = ["analyze", "--config", "/fixture/profile.json", "--kubeconfig", "/fixture/kubeconfig.json", "--prometheus-url", "https://fixture:8443", "--history-window", "1h", "--history-step", "5m", "--timeout", "30s", "--output", "json"]
        # Negative controls use the untouched image trust store. Neither Git nor
        # Go may silently accept an untrusted certificate or disable verification.
        rejected_git = command("docker", "run", "--rm", "--network", network, "--entrypoint", "git", IMAGE, "ls-remote", "https://fixture:8443/repo.git", check=False)
        assert rejected_git.returncode and "certificate" in rejected_git.stderr.lower()
        rejected_go = json.loads(command("docker", "run", "--rm", "--network", network, *mount, IMAGE, *analyze).stdout)
        signal = rejected_go["workloads"][0]["metricSignals"][0]
        assert not signal["healthy"] and "certificate" in signal["error"].lower()

        app = network + "-app"
        containers.append(app)
        command("docker", "run", "-d", "--rm", "--name", app, "--network", network, "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--tmpfs", "/work:rw,uid=65532,gid=65532,mode=0700", "--tmpfs", "/tmp:rw,nosuid,noexec,size=16m", *mount, "--mount", f"type=bind,src={fixture / 'trust.pem'},dst=/etc/ssl/certs/ca-certificates.crt,readonly", "--entrypoint", "/bin/sh", IMAGE, "-ec", "exec sleep 3600")

        def execute(*args):
            return command("docker", "exec", app, *args).stdout

        assert execute("id", "-u").strip() == "65532"
        git_version = execute("git", "--version").strip()
        execute("git", "clone", "-q", "https://fixture:8443/repo.git", "/work/repo")
        head = execute("git", "-C", "/work/repo", "rev-parse", "HEAD").strip()
        report = json.loads(execute(BINARY, *analyze, "--git-worktree", "/work/repo", "--state-db", "/work/state.db"))
        assert report["summary"]["workloadsTotal"] == report["summary"]["metricsHealthy"] == 1
        assert report["workloads"][0]["recommendation"]["mode"] == "dry-run"
        assert report["workloads"][0]["rollout"]["settled"] is True
        assert report["gitHealth"]["status"] == "clean" and not report["gitHealth"].get("errors")
        assert execute("git", "-C", "/work/repo", "rev-parse", "HEAD").strip() == head
        assert not execute("git", "-C", "/work/repo", "status", "--porcelain").strip()
        execute("/bin/sh", "-ec", "test -s /work/state.db; test -x \"$(git --exec-path)/git-credential-store\"")
        assert "Proposal Status" in execute(BINARY, "proposal", "status", "--git-worktree", "/work/repo")
        # Exercise required Git commit/push functionality only against a new
        # disposable file remote. The HTTPS mock rejects every write method.
        execute("/bin/sh", "-ec", "git init -q --bare /work/write-remote.git; cd /work/repo; git config user.name 'CI fixture'; git config user.email fixture@example.invalid; printf 'synthetic only\\n' > roundtrip.txt; git add roundtrip.txt; git commit -qm 'Synthetic roundtrip'; git push -q /work/write-remote.git HEAD:refs/heads/fixture; test \"$(git rev-parse HEAD)\" = \"$(git --git-dir=/work/write-remote.git rev-parse refs/heads/fixture)\"")
        stats = json.loads(command("docker", "exec", service, "python", "-c", "import ssl,urllib.request;print(urllib.request.urlopen('https://fixture:8443/ci-stats',context=ssl.create_default_context(cafile='/fixture/cert.pem')).read().decode())").stdout)
        assert stats["writes"] == 0 and all(stats[k] > 0 for k in ("kubernetes", "prometheus", "git"))
        print(json.dumps({"version": VERSION, "platform": PLATFORM, "uid": 65532, "git": git_version, "checks": ["actual-binary-version", "git-https", "go-prometheus-tls", "untrusted-tls-rejected", "mock-kubernetes", "dry-run-no-mutation", "sqlite-persistence", "git-credential-helper", "git-local-commit-push"], "fixtureRequests": stats}))
    finally:
        for container in reversed(containers):
            command("docker", "rm", "-f", container, check=False, timeout=30)
        if network:
            command("docker", "network", "rm", network, check=False, timeout=30)
