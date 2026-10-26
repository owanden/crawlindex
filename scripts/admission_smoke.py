import argparse
from concurrent.futures import ThreadPoolExecutor
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
    parser.add_argument("--output", default=".work/admission-" + uuid.uuid4().hex[:8])
    output = Path(parser.parse_args().output).resolve()
    output.mkdir(parents=True, exist_ok=False)
    name = "crawlindex-admission-" + uuid.uuid4().hex[:8]
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

    def request(path, payload=None):
        data = None if payload is None else json.dumps(payload).encode()
        req = urllib.request.Request(base + path, data=data, headers={"Content-Type": "application/json"})
        try:
            response = urllib.request.urlopen(req, timeout=5)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            return response.status, json.load(response)

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
        config_path.write_text(json.dumps(config), encoding="utf-8")
        compose = ["docker", "compose", "--project-name", name, "--file", str(config_path)]
        print("Starting isolated admission fixture", flush=True)
        created = True
        run(*compose, "up", "--build", "-d", "api", "fixture", timeout=360)
        base = "http://" + run(*compose, "port", "api", "8080").strip()
        wait(lambda: request("/healthz")[0] == 200)
        run(*compose, "exec", "-T", "db", "psql", "-U", "crawler", "-d", "crawler", "-v", "ON_ERROR_STOP=1",
            "-c", "UPDATE admission_budget SET max_urls=2 WHERE id=1")
        assert request("/admission") == (200, {"limit": 2, "admitted": 0, "remaining": 2})
        status, seed = request("/urls", {"url": "http://fixture:8081/#one"})
        assert status == 202 and request("/urls", {"url": "http://fixture:8081/#two"}) == (202, seed)
        run(*compose, "up", "--build", "-d", "--scale", "worker=2", "worker", timeout=360)
        assert len(run(*compose, "ps", "--status", "running", "--quiet", "worker").splitlines()) == 2
        wait(lambda: request(f"/jobs/{seed['id']}")[1]["state"] == "done" and len(request("/search?q=PostgreSQL")[1]) == 1)
        job = request(f"/jobs/{seed['id']}")[1]
        budget = request("/admission")[1]
        assert job["discovery_rejected"] == 1 and job["attempts"] == 1, job
        assert budget == {"limit": 2, "admitted": 2, "remaining": 0}, budget
        assert request(f"/documents/{seed['id']}")[1]["title"] == "CrawlIndex fixture"
        assert request("/urls", {"url": "http://fixture:8081/new"}) == (409, {"error": "URL admission budget exhausted"})

        def burst(i):
            if i % 2:
                return request("/urls", {"url": "http://fixture:8081/#duplicate"}) == (202, seed)
            return request("/urls", {"url": f"http://fixture:8081/new-{i}"})[0] == 409

        with ThreadPoolExecutor(max_workers=8) as pool:
            assert all(pool.map(burst, range(16)))
        report.update({"running_workers": 2, "completed_seed": job, "before_restart": budget,
                       "burst_duplicates_accepted": 8, "burst_new_seeds_rejected": 8})
        print("Restarting database, API and workers with an exhausted budget", flush=True)
        run(*compose, "restart", "db", "api", "worker")
        previous_base = base
        base = "http://" + run(*compose, "port", "api", "8080").strip()
        report["published_port_changed_on_restart"] = previous_base != base
        wait(lambda: request("/healthz")[0] == 200)
        after = request("/admission")[1]
        assert after == budget
        assert request("/urls", {"url": "http://fixture:8081/after-restart"})[0] == 409
        assert request("/urls", {"url": "http://fixture:8081/"}) == (202, seed)
        assert request(f"/jobs/{seed['id']}")[1] == job
        assert len(request("/search?q=PostgreSQL")[1]) == 1
        report["after_restart"] = after
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
