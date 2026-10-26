import argparse
from datetime import datetime, timezone
import json
from pathlib import Path
import subprocess
import sys
import time
import urllib.error
import urllib.request
import uuid


ROOT = Path(__file__).resolve().parents[1]


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--output", default=".work/robots-" + uuid.uuid4().hex[:8])
    output = Path(parser.parse_args().output).resolve()
    output.mkdir(parents=True, exist_ok=False)
    name = "crawlindex-robots-" + uuid.uuid4().hex[:8]
    config_path = output / "compose.json"
    compose = ["docker", "compose", "--project-name", name, "--file", str(ROOT / "compose.yaml")]
    report = {"project": name, "status": "running", "started_at": datetime.now(timezone.utc).isoformat()}
    count = 0
    created = False
    log = (output / "commands.log").open("w", encoding="utf-8")

    def run(*args, timeout=180):
        nonlocal count
        count += 1
        path = output / f"step-{count:02d}.log"
        log.write(json.dumps(list(args)) + "\n"); log.flush()
        timed_out = False
        with path.open("wb") as handle:
            proc = subprocess.Popen(args, cwd=ROOT, stdout=handle, stderr=subprocess.STDOUT)
            try:
                code = proc.wait(timeout=timeout)
            except subprocess.TimeoutExpired:
                timed_out = True
                proc.kill(); code = proc.wait(timeout=5)
        text = path.read_text(encoding="utf-8", errors="replace")
        log.write(text + "\n"); log.flush()
        if timed_out:
            raise RuntimeError(f"command timed out: {args[:3]}")
        if code:
            raise RuntimeError(f"command exited {code}: {args[:3]}")
        return text

    def request(path, payload=None, fixture=False):
        data = None if payload is None else json.dumps(payload).encode()
        req = urllib.request.Request((fixture_base if fixture else base) + path, data=data, headers={"Content-Type": "application/json"})
        try:
            response = urllib.request.urlopen(req, timeout=5)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            raw = response.read()
            return response.status, json.loads(raw) if raw else None

    def wait(check):
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            try:
                result = check()
                if result:
                    return result
            except OSError:
                pass
            time.sleep(0.1)
        raise RuntimeError("API condition timed out")

    try:
        report["revision"] = run("git", "rev-parse", "HEAD").strip()
        report["source_status"] = run("git", "status", "--short").strip()
        report["docker_version"] = run("docker", "version", "--format", "{{.Server.Version}}", timeout=20).strip()
        config = json.loads(run(*compose, "config", "--format", "json"))
        assert config["name"] == name
        assert all(v["name"].startswith(name + "_") for v in config["volumes"].values())
        assert all(v["name"].startswith(name + "_") for v in config["networks"].values())
        config["services"]["api"]["ports"] = ["127.0.0.1::8080"]
        config["services"]["fixture"]["ports"] = ["127.0.0.1::8081"]
        config_path.write_text(json.dumps(config), encoding="utf-8")
        compose = ["docker", "compose", "--project-name", name, "--file", str(config_path)]
        print("Starting isolated robots fixture", flush=True)
        created = True
        run(*compose, "up", "--build", "-d", "api", "fixture", timeout=360)
        base = "http://" + run(*compose, "port", "api", "8080").strip()
        fixture_base = "http://" + run(*compose, "port", "fixture", "8081").strip()
        wait(lambda: request("/healthz")[0] == 200)
        def submit(path):
            status, result = request("/urls", {"url": "http://fixture:8081" + path})
            assert status == 202, (status, result)
            return result["id"]

        def job(id):
            return request(f"/jobs/{id}")[1]

        def expire():
            run(*compose, "exec", "-T", "db", "psql", "-U", "crawler", "-d", "crawler", "-v", "ON_ERROR_STOP=1",
                "-c", "UPDATE domains SET robots_expires=clock_timestamp()-interval '1 second'")

        denied, opened, public = submit("/private"), submit("/private/open"), submit("/policy-page")
        run(*compose, "up", "--build", "-d", "--scale", "worker=2", "worker", timeout=360)
        assert len(run(*compose, "ps", "--status", "running", "--quiet", "worker").splitlines()) == 2
        wait(lambda: job(denied)["state"] == "failed" and job(opened)["state"] == job(public)["state"] == "done")
        assert job(denied)["attempts"] == 1 and job(denied)["last_error"] == "robots disallowed"
        assert request(f"/documents/{denied}")[0] == 404
        assert request("/search?q=Secretword")[1] == []
        initial = request("/robots-status", fixture=True)[1]
        assert initial["robots_requests"] == 1 and initial["page_hits"].get("/private", 0) == 0, initial
        assert initial["wrong_agent"] == 0
        report["denied_job"] = job(denied)
        report["initial_fixture"] = initial

        request("/robots-control", {"mode": "deny-all"}, fixture=True)
        run(*compose, "restart", "db", "api", "worker")
        base = "http://" + run(*compose, "port", "api", "8080").strip()
        wait(lambda: request("/healthz")[0] == 200)
        cached = submit("/cached")
        wait(lambda: job(cached)["state"] == "done")
        assert request("/robots-status", fixture=True)[1]["robots_requests"] == 1
        report["cache_survived_restart"] = job(cached)

        request("/robots-control", {"mode": "error"}, fixture=True)
        expire()
        recovering = submit("/after-error")
        wait(lambda: job(recovering)["state"] == "queued" and job(recovering)["attempts"] == 1)
        blocked = job(recovering)
        assert blocked["last_error"] == "robots HTTP 503", blocked
        assert request("/robots-status", fixture=True)[1]["page_hits"].get("/after-error", 0) == 0
        started = time.monotonic()
        request("/robots-control", {"mode": "allow"}, fixture=True)
        run(*compose, "restart", "db", "api", "worker")
        base = "http://" + run(*compose, "port", "api", "8080").strip()
        wait(lambda: request("/healthz")[0] == 200)
        wait(lambda: job(recovering)["state"] == "done")
        elapsed = time.monotonic() - started
        assert elapsed >= 8 and job(recovering)["attempts"] == 2
        assert request("/robots-status", fixture=True)[1]["robots_requests"] == 3
        report["temporary_error"] = blocked
        report["recovered_job"] = job(recovering)
        report["retry_survived_restart"] = True
        report["retry_wait_seconds"] = round(elapsed, 3)

        request("/robots-control", {"mode": "missing"}, fixture=True)
        expire()
        missing = submit("/missing-policy")
        wait(lambda: job(missing)["state"] == "done")
        missing_cached = submit("/missing-cache")
        wait(lambda: job(missing_cached)["state"] == "done")
        final = request("/robots-status", fixture=True)[1]
        assert final["robots_requests"] == 4 and final["wrong_agent"] == 0
        assert final["page_hits"].get("/private", 0) == 0
        assert all(final["page_hits"].get(p) == 1 for p in ["/private/open", "/policy-page", "/cached", "/after-error", "/missing-policy", "/missing-cache"])
        budget = request("/admission")[1]
        assert budget == {"limit": 1000, "admitted": 7, "remaining": 993}, budget
        report["running_workers"] = 2
        report["final_fixture"] = final
        report["budget"] = budget
        report["status"] = "passed"
    except Exception as error:
        report["status"] = "failed"
        report["error"] = str(error)
        print(str(error), file=sys.stderr)
    finally:
        if created:
            try:
                run(*compose, "logs", "--no-color", "--tail=100", timeout=20)
            except Exception as error:
                report["log_error"] = str(error)
            try:
                run(*compose, "--profile", "test", "down", "--volumes", timeout=60)
                report["owned_stack_removed"] = not run(*compose, "ps", "--all", "--quiet").strip()
                assert report["owned_stack_removed"]
            except Exception as error:
                report["status"] = "failed"
                report["cleanup_error"] = str(error)
        report["finished_at"] = datetime.now(timezone.utc).isoformat()
        (output / "report.json").write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
        log.close()
    print(json.dumps(report, indent=2))
    return 0 if report["status"] == "passed" else 1


if __name__ == "__main__":
    raise SystemExit(main())
