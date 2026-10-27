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
    parser.add_argument("--output", default=".work/recrawl-" + uuid.uuid4().hex[:8])
    output = Path(parser.parse_args().output).resolve()
    output.mkdir(parents=True, exist_ok=False)
    name = "crawlindex-recrawl-" + uuid.uuid4().hex[:8]
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
        print("Starting isolated recrawl fixture", flush=True)
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

        def sql(text):
            return run(*compose, "exec", "-T", "db", "psql", "-U", "crawler", "-d", "crawler", "-v", "ON_ERROR_STOP=1", "-c", text)

        def document(id):
            status, doc = request(f"/documents/{id}")
            assert status == 200, (status, doc)
            return doc

        def recrawl(generation):
            return request(f"/jobs/{id}/recrawl", {"generation": generation})

        def restart():
            nonlocal base
            run(*compose, "restart", "db", "api")
            base = "http://" + run(*compose, "port", "api", "8080").strip()
            wait(lambda: request("/healthz")[0] == 200)

        sql("UPDATE admission_budget SET max_urls=1 WHERE id=1")
        id = submit("/refresh")
        run(*compose, "up", "--build", "-d", "--scale", "worker=2", "worker", timeout=360)
        wait(lambda: job(id)["state"] == "done")
        assert job(id)["generation"] == document(id)["generation"] == 1
        assert document(id)["title"] == "Oldword" and len(request("/search?q=Oldword")[1]) == 1
        report["initial_job"] = job(id)
        report["initial_document"] = document(id)
        run(*compose, "stop", "worker")
        request("/refresh-control", {"mode": "second"}, fixture=True)
        with ThreadPoolExecutor(max_workers=8) as pool:
            burst = list(pool.map(lambda _: recrawl(1), range(16)))
        assert all(status == 202 and j["id"] == id and j["generation"] == 2 and j["state"] == "queued" and j["attempts"] == 0 for status, j in burst), burst
        assert recrawl(2)[0] == 409
        assert submit("/refresh") == id and job(id)["generation"] == 2
        assert document(id)["generation"] == 1 and document(id)["title"] == "Oldword"
        report["concurrent_duplicates"] = len(burst)
        report["queued_job"] = job(id)
        restart()
        assert job(id) == report["queued_job"] and document(id)["generation"] == 1
        report["queued_survived_restart"] = True
        run(*compose, "up", "-d", "--scale", "worker=2", "worker")
        wait(lambda: job(id)["state"] == "done")
        assert job(id)["generation"] == document(id)["generation"] == 2
        assert job(id)["attempts"] == 1 and document(id)["title"] == "Freshword"
        assert request("/search?q=Oldword")[1] == [] and len(request("/search?q=Freshword")[1]) == 1
        assert recrawl(1)[0] == 202 and job(id)["generation"] == 2
        report["replaced_job"] = job(id)
        report["replaced_document"] = document(id)
        request("/refresh-control", {"mode": "error"}, fixture=True)
        assert recrawl(2)[0] == 202
        wait(lambda: job(id)["state"] == "failed")
        assert job(id)["generation"] == 3 and job(id)["attempts"] == 3 and job(id)["last_error"] == "HTTP status 503"
        assert document(id)["generation"] == 2 and len(request("/search?q=Freshword")[1]) == 1
        report["failed_job"] = job(id)
        run(*compose, "stop", "worker")
        restart()
        assert job(id) == report["failed_job"] and document(id)["generation"] == 2
        report["failure_survived_restart"] = True
        assert recrawl(1)[0] == 409
        request("/robots-control", {"mode": "deny-all"}, fixture=True)
        sql("UPDATE domains SET robots_expires=clock_timestamp()-interval '1 second'")
        assert recrawl(3)[0] == 202
        before = request("/refresh-status", fixture=True)[1]["requests"]
        run(*compose, "up", "-d", "--scale", "worker=2", "worker")
        wait(lambda: job(id)["state"] == "failed")
        assert job(id)["generation"] == 4 and job(id)["attempts"] == 1 and job(id)["last_error"] == "robots disallowed"
        assert request("/refresh-status", fixture=True)[1]["requests"] == before
        assert document(id)["generation"] == 2
        report["robots_denied_job"] = job(id)
        report["requests_before_robots_denial"] = before
        request("/robots-control", {"mode": "allow"}, fixture=True)
        request("/refresh-control", {"mode": "first"}, fixture=True)
        sql("UPDATE domains SET robots_expires=clock_timestamp()-interval '1 second'")
        assert recrawl(4)[0] == 202
        wait(lambda: job(id)["state"] == "done")
        assert job(id)["generation"] == document(id)["generation"] == 5 and job(id)["attempts"] == 1
        assert document(id)["title"] == "Oldword" and request("/search?q=Freshword")[1] == []
        assert len(request("/search?q=Oldword")[1]) == 1
        final = request("/refresh-status", fixture=True)[1]
        assert final == {"mode": "first", "requests": 6}, final
        policy = request("/robots-status", fixture=True)[1]
        assert policy["robots_requests"] == 3 and policy["wrong_agent"] == 0
        assert len(run(*compose, "ps", "--status", "running", "--quiet", "worker").splitlines()) == 2
        budget = request("/admission")[1]
        assert budget == {"limit": 1, "admitted": 1, "remaining": 0}, budget
        report["recovered_job"] = job(id)
        report["recovered_document"] = document(id)
        report["final_fixture"] = final
        report["final_policy"] = policy
        report["running_workers"] = 2
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
        (output / "report.json").write_bytes((json.dumps(report, indent=2) + "\n").encode("utf-8"))
        log.close()
    print(json.dumps(report, indent=2))
    return 0 if report["status"] == "passed" else 1


if __name__ == "__main__":
    raise SystemExit(main())
