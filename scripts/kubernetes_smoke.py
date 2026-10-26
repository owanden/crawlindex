import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
import uuid


ROOT = Path(__file__).resolve().parents[1]
NODE = "kindest/node:v1.34.0@sha256:7416a61b42b1662ca6ca89f02028ac133a309a2a30ba309614e8ec94d976dc5a"


def eventually(check, timeout=60):
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        try:
            value = check()
            if value:
                return value
        except (OSError, ValueError) as error:
            last = str(error)
        time.sleep(0.1)
    raise RuntimeError(f"condition timed out: {last}")


def request(base, path, payload=None):
    data = None if payload is None else json.dumps(payload).encode()
    req = urllib.request.Request(base + path, data=data, headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=3) as response:
        raw = response.read()
        return json.loads(raw) if raw else None


class Smoke:
    def __init__(self, args):
        self.args = args
        self.name = "crawlindex-smoke-" + uuid.uuid4().hex[:8]
        self.context = "kind-" + self.name
        self.output = Path(args.output).resolve()
        self.output.mkdir(parents=True, exist_ok=False)
        self.kubeconfig = self.output / "kubeconfig"
        self.env = dict(os.environ, KUBECONFIG=str(self.kubeconfig), KIND_EXPERIMENTAL_PROVIDER="docker")
        self.log = (self.output / "commands.log").open("w", encoding="utf-8")
        self.forwards = []
        self.created = False
        self.command_count = 0
        self.default_config = Path.home() / ".kube" / "config"
        self.before = self.config_hash()
        self.report = {"cluster": self.name, "context": self.context, "node_image": NODE, "status": "running",
                       "started_at": datetime.now(timezone.utc).isoformat()}

    def config_hash(self):
        return hashlib.sha256(self.default_config.read_bytes()).hexdigest() if self.default_config.exists() else None

    def run(self, *argv, timeout=60, input=None, env=None):
        self.log.write(json.dumps(list(argv)) + "\n")
        self.log.flush()
        self.command_count += 1
        steps = self.output / "steps"
        steps.mkdir(exist_ok=True)
        output = steps / f"{self.command_count:03d}.log"
        timed_out = False
        with output.open("wb") as handle, tempfile.TemporaryFile(mode="w+", encoding="utf-8") as source:
            if input is not None:
                source.write(input)
                source.seek(0)
            proc = subprocess.Popen(list(argv), cwd=ROOT, env=env or self.env, stdin=source,
                                    stdout=handle, stderr=subprocess.STDOUT)
            try:
                code = proc.wait(timeout=timeout)
            except subprocess.TimeoutExpired:
                timed_out = True
                proc.kill()
                code = proc.wait(timeout=5)
        content = output.read_text(encoding="utf-8", errors="replace")
        self.log.write(content + "\n")
        self.log.flush()
        if timed_out:
            raise RuntimeError(f"command timed out after {timeout}s: {argv[0:3]}")
        if code:
            raise RuntimeError(f"command exited {code}: {argv[0:3]}")
        return content

    def kube(self, *argv, **kwargs):
        return self.run(self.args.kubectl, "--kubeconfig", str(self.kubeconfig), "--context", self.context,
                        "--namespace", "crawlindex", *argv, **kwargs)

    def pods(self):
        return json.loads(self.kube("get", "pods", "-l", "app=worker", "-o", "json"))["items"]

    def ready_workers(self, expected):
        items = self.pods()
        ready = [p for p in items if not p["metadata"].get("deletionTimestamp") and
                 any(c["type"] == "Ready" and c["status"] == "True" for c in p.get("status", {}).get("conditions", []))]
        return ready if len(items) == expected and len(ready) == expected else None

    def scale(self, count):
        self.kube("scale", "deployment/worker", f"--replicas={count}")
        self.kube("rollout", "status", "deployment/worker", "--timeout=120s", timeout=140)
        return eventually(lambda: self.ready_workers(count), timeout=60)

    def forward(self, service, port):
        path = self.output / f"forward-{service}.log"
        handle = path.open("wb")
        proc = subprocess.Popen([self.args.kubectl, "--kubeconfig", str(self.kubeconfig), "--context", self.context,
                                 "--namespace", "crawlindex", "port-forward", "--address", "127.0.0.1",
                                 "service/" + service, ":" + str(port)], env=self.env, cwd=ROOT,
                                stdout=handle, stderr=subprocess.STDOUT)
        self.forwards.append((proc, handle))

        def address():
            if proc.poll() is not None:
                raise RuntimeError(f"port-forward {service} exited")
            match = re.search(r"Forwarding from 127\.0\.0\.1:(\d+)", path.read_text(encoding="utf-8"))
            return "http://127.0.0.1:" + match[1] if match else None

        return eventually(address, timeout=30)

    def execute(self):
        print("Checking tools and creating isolated cluster", flush=True)
        self.report["revision"] = self.run("git", "rev-parse", "HEAD").strip()
        self.report["source_status"] = self.run("git", "status", "--short").strip()
        self.report["kind_version"] = self.run(self.args.kind, "version").strip()
        if not self.report["kind_version"].startswith("kind v0.30.0 "):
            raise RuntimeError("kind v0.30.0 required")
        self.report["kubectl_client"] = json.loads(self.run(self.args.kubectl, "version", "--client", "-o", "json"))["clientVersion"]["gitVersion"]
        if self.report["kubectl_client"] != "v1.34.0":
            raise RuntimeError("kubectl v1.34.0 required")
        self.report["docker_version"] = self.run("docker", "version", "--format", "{{.Server.Version}}", timeout=20).strip()
        existing = self.run(self.args.kind, "get", "clusters").splitlines()
        if self.name in existing or self.kubeconfig.exists():
            raise RuntimeError("refusing existing cluster/config")
        self.created = True
        self.run(self.args.kind, "create", "cluster", "--name", self.name, "--kubeconfig", str(self.kubeconfig),
                 "--image", NODE, "--wait", "120s", timeout=360)
        self.report["server_version"] = json.loads(self.kube("version", "-o", "json"))["serverVersion"]["gitVersion"]
        image = "crawlindex:" + self.name
        print("Building and loading application/database images", flush=True)
        self.run("docker", "build", "--platform=linux/amd64", "--provenance=false", "-t", image, ".", timeout=360)
        self.report["application_image_id"] = self.run("docker", "image", "inspect", image, "--format", "{{.Id}}").strip()
        self.run("docker", "pull", "--platform=linux/amd64", "postgres:17.6-bookworm", timeout=180)
        archive = self.output / "images.tar"
        self.run("docker", "image", "save", "--platform=linux/amd64", "-o", str(archive), image, "postgres:17.6-bookworm", timeout=180)
        self.run(self.args.kind, "load", "image-archive", "--name", self.name, str(archive), timeout=240)
        rendered = self.run(self.args.kubectl, "kustomize", "deploy/local")
        if rendered.count("image: crawlindex:local") != 3:
            raise RuntimeError("unexpected application image references")
        rendered = rendered.replace("image: crawlindex:local", "image: " + image)
        (self.output / "rendered.yaml").write_text(rendered, encoding="utf-8")
        self.kube("apply", "-f", "-", input=rendered)
        for resource in ["statefulset/db", "deployment/fixture", "deployment/api", "deployment/worker"]:
            self.kube("rollout", "status", resource, "--timeout=180s", timeout=200)
        initial = eventually(lambda: self.ready_workers(2))
        self.report["initial_worker_uids"] = [p["metadata"]["uid"] for p in initial]
        api, fixture = self.forward("api", 8080), self.forward("fixture", 8081)
        eventually(lambda: request(api, "/healthz")["status"] == "ok")
        print("Verifying real submission, retrieval and full-text search", flush=True)
        first = request(api, "/urls", {"url": "http://fixture:8081/#one"})["id"]
        assert request(api, "/urls", {"url": "http://fixture:8081/#two"})["id"] == first
        eventually(lambda: len(request(api, "/search?q=PostgreSQL")) == 2)
        assert request(api, f"/documents/{first}")["title"] == "CrawlIndex fixture"
        assert request(api, "/search?q=invisibleword") == []
        self.report["search_titles"] = sorted(d["title"] for d in request(api, "/search?q=PostgreSQL"))
        victim = self.scale(1)[0]
        self.report["scale_down_ready"] = 1
        victim_name, victim_uid = victim["metadata"]["name"], victim["metadata"]["uid"]
        print("Stopping the leased container and deleting its worker pod", flush=True)
        job_id = request(api, "/urls", {"url": "http://fixture:8081/gate"})["id"]
        eventually(lambda: request(fixture, "/gate-status") == {"requests": 1, "active": 1})
        before = request(api, f"/jobs/{job_id}")
        assert before["state"] == "leased" and before["attempts"] == 1, before
        started = time.monotonic()
        container_id = victim["status"]["containerStatuses"][0]["containerID"].removeprefix("containerd://")
        node = victim["spec"]["nodeName"]
        if not node.startswith(self.name + "-"):
            raise RuntimeError("refusing runtime operation outside owned kind node")
        self.run("docker", "exec", node, "crictl", "stop", "--timeout=0", container_id)
        stopped = json.loads(self.run("docker", "exec", node, "crictl", "inspect", container_id))["status"]
        assert stopped["state"] == "CONTAINER_EXITED" and stopped["exitCode"] == 137, stopped
        self.kube("delete", "pod", victim_name, "--grace-period=0", "--force", "--wait=true")
        eventually(lambda: request(fixture, "/gate-status")["active"] == 0, timeout=3)
        after = request(api, f"/jobs/{job_id}")
        assert after["state"] == "leased" and after["attempts"] == 1 and not after["last_error"], after
        request(fixture, "/gate-release", {})
        eventually(lambda: request(api, f"/jobs/{job_id}")["state"] == "done", timeout=60)
        recovered = request(api, f"/jobs/{job_id}")
        elapsed = time.monotonic() - started
        assert elapsed >= 15, f"recovery preceded normal lease expiry: {elapsed}s"
        gate = request(fixture, "/gate-status")
        assert recovered["attempts"] == 2 and gate == {"requests": 2, "active": 0}, (recovered, gate)
        assert request(api, f"/documents/{job_id}")["title"] == "Recovered lease"
        replacement = eventually(lambda: self.ready_workers(1))[0]
        assert replacement["metadata"]["uid"] != victim_uid
        self.report["pod_recovery"] = {"job_id": job_id, "deleted_uid": victim_uid,
                                       "replacement_uid": replacement["metadata"]["uid"],
                                       "container_id": victim["status"]["containerStatuses"][0]["containerID"],
                                       "stopped_container": {"state": stopped["state"], "exit_code": stopped["exitCode"]},
                                       "before_delete": before, "after_delete": after, "completed": recovered, "fixture": gate,
                                       "observed_seconds": round(elapsed, 3)}
        print("Scaling to three workers and draining six new jobs", flush=True)
        scaled = self.scale(3)
        ids = [request(api, "/urls", {"url": f"http://fixture:8081/?scale={i}"})["id"] for i in range(6)]
        eventually(lambda: all(request(api, f"/jobs/{i}")["state"] == "done" for i in ids))
        jobs = [request(api, f"/jobs/{i}") for i in ids]
        assert all(j["attempts"] == 1 for j in jobs), jobs
        self.report["scale_up"] = {"ready_replicas": 3, "pod_uids": [p["metadata"]["uid"] for p in scaled], "jobs": jobs}
        self.report["status"] = "passed"

    def finish(self):
        if self.created and self.kubeconfig.exists():
            for name, args in [("pods.json", ("get", "pods", "-o", "json")),
                               ("events.json", ("get", "events", "-o", "json")),
                               ("worker.log", ("logs", "-l", "app=worker", "--prefix", "--tail=200"))]:
                try:
                    (self.output / name).write_text(self.kube(*args, timeout=15), encoding="utf-8")
                except Exception as error:
                    self.report.setdefault("diagnostic_errors", []).append(str(error))
        for proc, handle in self.forwards:
            if proc.poll() is None:
                proc.terminate()
                try:
                    proc.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    proc.kill()
                    proc.wait(timeout=10)
            handle.close()
        if self.created:
            try:
                self.run(self.args.kind, "delete", "cluster", "--name", self.name, "--kubeconfig", str(self.kubeconfig), timeout=120)
                self.report["cluster_deleted"] = self.name not in self.run(self.args.kind, "get", "clusters").splitlines()
                if not self.report["cluster_deleted"]:
                    raise RuntimeError("owned cluster remains")
            except Exception as error:
                self.report["cleanup_error"] = str(error)
                self.report["status"] = "failed"
        self.report["default_kubeconfig_unchanged"] = self.config_hash() == self.before
        if not self.report["default_kubeconfig_unchanged"]:
            self.report["status"] = "failed"
        self.report["finished_at"] = datetime.now(timezone.utc).isoformat()
        (self.output / "report.json").write_text(json.dumps(self.report, indent=2) + "\n", encoding="utf-8")
        self.log.close()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--kind", default="kind")
    parser.add_argument("--kubectl", default="kubectl")
    parser.add_argument("--output", default=".work/kubernetes-" + uuid.uuid4().hex[:8])
    smoke = Smoke(parser.parse_args())
    try:
        smoke.execute()
    except Exception as error:
        smoke.report["status"] = "failed"
        smoke.report["error"] = str(error)
        print(str(error), file=sys.stderr)
    finally:
        smoke.finish()
    print(json.dumps(smoke.report, indent=2))
    return 0 if smoke.report["status"] == "passed" else 1


if __name__ == "__main__":
    raise SystemExit(main())
