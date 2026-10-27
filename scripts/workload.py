import argparse
from datetime import datetime, timezone
import hashlib
import json
from pathlib import Path
import platform
import re
import statistics
import subprocess
import sys
import time
import urllib.error
import urllib.request
import uuid

ROOT = Path(__file__).resolve().parents[1]
PAGES, HTML_BYTES, REPEATS = 24, 393216, 3


def memory_bytes(value):
    number, unit = re.fullmatch(r"([0-9.]+)([kMGT]?i?B)", value).groups()
    return round(float(number) * {"B": 1, "kB": 1000, "MB": 10**6, "GB": 10**9, "TB": 10**12,
                                "KiB": 1024, "MiB": 1024**2, "GiB": 1024**3, "TiB": 1024**4}[unit])


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--output", default=".work/workload-" + uuid.uuid4().hex[:8])
    output = Path(parser.parse_args().output).resolve()
    output.mkdir(parents=True, exist_ok=False)
    name = "crawlindex-workload-" + uuid.uuid4().hex[:8]
    config_path = output / "compose.json"
    compose = ["docker", "compose", "--project-name", name, "--file", str(ROOT / "compose.yaml")]
    report = {"project": name, "status": "running", "started_at": datetime.now(timezone.utc).isoformat(), "runs": []}
    observations = []
    count, created = 0, False
    log = (output / "commands.log").open("w", encoding="utf-8")

    def run(*args, timeout=180):
        nonlocal count
        count += 1
        path = output / f"step-{count:03d}.log"
        log.write(json.dumps(list(args)) + "\n"); log.flush()
        with path.open("wb") as handle:
            proc = subprocess.Popen(args, cwd=ROOT, stdout=handle, stderr=subprocess.STDOUT)
            timed_out = False
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

    def request(base, path, payload=None):
        data = None if payload is None else json.dumps(payload).encode()
        req = urllib.request.Request(base + path, data=data, headers={"Content-Type": "application/json"})
        try:
            response = urllib.request.urlopen(req, timeout=5)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            raw = response.read(1_000_001)
            assert len(raw) <= 1_000_000
            return response.status, json.loads(raw) if raw else None

    def sql(statement):
        return json.loads(run(*compose, "exec", "-T", "db", "psql", "-U", "crawler", "-d", "crawler", "-At", "-v", "ON_ERROR_STOP=1", "-c", statement))

    def cleanup():
        nonlocal created
        if created:
            run(*compose, "logs", "--no-color", "--tail=80", timeout=20)
            run(*compose, "down", "--volumes", timeout=60)
            assert not run(*compose, "ps", "--all", "--quiet").strip()
            created = False

    try:
        report["revision"] = run("git", "rev-parse", "HEAD").strip()
        report["source_status"] = run("git", "status", "--short").strip()
        report["build_files_sha256"] = {p: hashlib.sha256((ROOT / p).read_bytes()).hexdigest() for p in ("Dockerfile", "go.mod", "go.sum")}
        report["build_images"] = [line for line in (ROOT / "Dockerfile").read_text().splitlines() if line.startswith("FROM ")]
        info_format = '{"cpus":{{.NCPU}},"background_running_containers":{{.ContainersRunning}},"memory_bytes":{{.MemTotal}},"os":{{json .OSType}},"architecture":{{json .Architecture}},"operating_system":{{json .OperatingSystem}},"kernel":{{json .KernelVersion}},"server":{{json .ServerVersion}},"storage":{{json .Driver}},"cgroup_version":{{json .CgroupVersion}}}'
        report["engine"] = json.loads(run("docker", "info", "--format", info_format))
        assert report["engine"]["os"] == "linux"
        report["client"] = {"system": platform.system(), "release": platform.release(), "machine": platform.machine(), "python": platform.python_version()}
        report["settings"] = {"repeats": REPEATS, "workers": 2, "concurrency_per_worker": 4, "origins": 1,
                              "domain_interval_ms": 500, "http_timeout_seconds": 5, "lease_seconds": 20,
                              "poll_target_seconds": 1, "resource_sample_target_seconds": 4, "settle_seconds": 2,
                              "request_slots_per_job": 2, "steady_origin_page_ceiling_per_second": 1,
                              "measurement": "before root POST through first SQL observation of all 24 done/documents"}
        config = json.loads(run(*compose, "config", "--format", "json"))
        assert config["name"] == name
        assert all(v["name"].startswith(name + "_") for v in config["volumes"].values())
        assert all(v["name"].startswith(name + "_") for v in config["networks"].values())
        config["services"].pop("test", None)
        for service, value in config["services"].items():
            value["cpus"] = 1.0 if service == "db" else 0.5
            value["mem_limit"] = value["memswap_limit"] = (512 if service == "db" else 128) * 1024**2
            if service != "db": value["image"] = name + ":runtime"
        config["services"]["api"]["ports"] = ["127.0.0.1::8080"]
        config["services"]["fixture"]["ports"] = ["127.0.0.1::8081"]
        config["services"]["worker"]["environment"]["WORKER_CONCURRENCY"] = "4"
        config_path.write_text(json.dumps(config), encoding="utf-8")
        compose = ["docker", "compose", "--project-name", name, "--file", str(config_path)]
        run(*compose, "build", "api", timeout=360)
        run(*compose, "pull", "db", timeout=180)
        image_format = '{"id":{{json .Id}},"digests":{{json .RepoDigests}},"os":{{json .Os}},"architecture":{{json .Architecture}}}'
        report["runtime_image"] = json.loads(run("docker", "image", "inspect", name + ":runtime", "--format", image_format))
        report["database_image"] = json.loads(run("docker", "image", "inspect", config["services"]["db"]["image"], "--format", image_format))
        for service, value in config["services"].items():
            value["image"] = report["database_image" if service == "db" else "runtime_image"]["id"]
            value.pop("build", None)
        config_path.write_text(json.dumps(config), encoding="utf-8")
        for trial in range(1, REPEATS + 1):
            print(f"Workload repetition {trial}/{REPEATS}", flush=True)
            result = {"trial": trial, "status": "running"}
            raw = {"trial": trial, "progress": [], "resources": [], "documents": []}
            report["runs"].append(result); observations.append(raw)
            created = True
            run(*compose, "up", "--no-build", "-d", "--scale", "worker=2", "api", "worker", "fixture", timeout=180)
            base = "http://" + run(*compose, "port", "api", "8080").strip()
            fixture = "http://" + run(*compose, "port", "fixture", "8081").strip()
            deadline = time.monotonic() + 30
            while True:
                try:
                    if request(base, "/healthz")[0] == 200: break
                except OSError: pass
                if time.monotonic() >= deadline: raise RuntimeError("API startup timed out")
                time.sleep(0.1)
            assert request(base, "/admission")[1] == {"limit": 1000, "admitted": 0, "remaining": 1000}
            manifest = request(fixture, "/workload/manifest")[1]
            assert len(manifest["pages"]) == PAGES and manifest["html_bytes"] == HTML_BYTES and manifest["origins"] == 1
            if "corpus" not in report: report["corpus"] = manifest
            assert manifest == report["corpus"]
            ids = run(*compose, "ps", "--quiet").splitlines()
            assert len(ids) == 5 and len(run(*compose, "ps", "--status", "running", "--quiet", "worker").splitlines()) == 2
            inspect_format = '{"name":{{json .Name}},"service":{{json (index .Config.Labels "com.docker.compose.service")}},"number":{{json (index .Config.Labels "com.docker.compose.container-number")}},"image":{{json .Image}},"nano_cpus":{{.HostConfig.NanoCpus}},"memory":{{.HostConfig.Memory}},"memory_swap":{{.HostConfig.MemorySwap}},"oom_killed":{{.State.OOMKilled}},"restarts":{{.RestartCount}}}'
            containers = [json.loads(line) for line in run("docker", "inspect", "--format", inspect_format, *ids).splitlines()]
            for c in containers:
                memory = (512 if c["service"] == "db" else 128) * 1024**2
                assert c["memory"] == c["memory_swap"] == memory and c["nano_cpus"] == (1_000_000_000 if c["service"] == "db" else 500_000_000)
                assert c["image"] == report["database_image" if c["service"] == "db" else "runtime_image"]["id"]
            result["containers"] = containers
            roles = {c["name"].lstrip("/"): c["service"] + "-" + c["number"] for c in containers}
            time.sleep(2)
            started = time.monotonic()
            status, seed = request(base, "/urls", {"url": "http://fixture:8081/workload/pages/000"})
            result["submission_ack_seconds"] = time.monotonic() - started
            assert status == 202
            result["seed_id"] = seed["id"]
            last_sample, previous_start = -4, 0
            while True:
                poll_start = time.monotonic() - started
                snapshot = sql("SELECT json_build_object('jobs',count(*),'done',count(*) FILTER (WHERE state='done'),'failed',count(*) FILTER (WHERE state='failed'),'attempts',coalesce(sum(attempts),0),'documents',(SELECT count(*) FROM documents)) FROM jobs")
                elapsed = time.monotonic() - started
                raw["progress"].append({"start_seconds": poll_start, "end_seconds": elapsed, "snapshot": snapshot})
                assert snapshot["failed"] == 0 and snapshot["jobs"] <= PAGES
                if snapshot["done"] == snapshot["documents"] == snapshot["jobs"] == PAGES:
                    result["observed_elapsed_seconds"] = elapsed
                    result["completion_bracket_seconds"] = [previous_start, elapsed]
                    break
                if elapsed > 120: raise RuntimeError("workload completion timed out")
                previous_start = poll_start
                if elapsed - last_sample >= 4:
                    sample_start = time.monotonic() - started
                    rows = [json.loads(line) for line in run("docker", "stats", "--no-stream", "--format", "{{json .}}", *ids, timeout=15).splitlines()]
                    assert len(rows) == 5
                    raw["resources"].append({"start_seconds": sample_start, "end_seconds": time.monotonic() - started, "rows": rows})
                    last_sample = time.monotonic() - started
                time.sleep(max(0, 1 - (time.monotonic() - started - poll_start)))
            result["pages_per_second"] = PAGES / elapsed
            result["html_bytes_per_second"] = HTML_BYTES / elapsed
            result["max_poll_gap_seconds"] = max(b["start_seconds"] - a["start_seconds"] for a, b in zip(raw["progress"], raw["progress"][1:]))
            verify_start = time.monotonic()
            jobs = sql("SELECT json_agg(json_build_object('id',j.id,'url',j.url,'depth',j.depth,'state',j.state,'attempts',j.attempts,'generation',j.generation,'discovery_rejected',j.discovery_rejected,'document_generation',d.generation) ORDER BY j.id) FROM jobs j JOIN documents d ON d.job_id=j.id")
            expected = {"http://fixture:8081" + p["path"]: p for p in manifest["pages"]}
            assert len(jobs) == PAGES and {j["url"] for j in jobs} == set(expected)
            for j in jobs:
                p = expected[j["url"]]
                assert j["state"] == "done" and j["attempts"] == j["generation"] == j["document_generation"] == 1 and j["discovery_rejected"] == 0
                assert j["depth"] == (0 if p["path"].endswith("000") else 1)
                code, doc = request(base, f"/documents/{j['id']}")
                text_hash = hashlib.sha256(doc["body"].encode()).hexdigest()
                assert code == 200 and doc["title"] == p["title"] and doc["url"] == j["url"] and doc["generation"] == 1 and text_hash == p["text_sha256"], ("document mismatch", j["url"], text_hash, p["text_sha256"])
                code, matches = request(base, "/search?q=" + p["token"])
                assert code == 200 and len(matches) == 1 and matches[0]["id"] == j["id"] and matches[0]["generation"] == 1, ("unique search mismatch", p["token"], code, matches)
                raw["documents"].append({**j, "title": doc["title"], "text_sha256": text_hash, "body_bytes": len(doc["body"].encode())})
            assert request(base, "/search?q=hiddenworkloadmarker")[1] == []
            assert len(request(base, "/search?q=corpusmarker")[1]) == 20
            hits = request(fixture, "/workload/status")[1]
            assert hits == {"page_hits": {p["path"]: 1 for p in manifest["pages"]}, "html_bytes": HTML_BYTES, "wrong_agent": 0}
            policy = request(fixture, "/robots-status")[1]
            assert policy["robots_requests"] == 1 and policy["wrong_agent"] == 0
            assert request(base, "/admission")[1] == {"limit": 1000, "admitted": PAGES, "remaining": 1000-PAGES}
            final_containers = [json.loads(line) for line in run("docker", "inspect", "--format", inspect_format, *ids).splitlines()]
            assert all(not c["oom_killed"] and c["restarts"] == 0 for c in final_containers)
            result.update({"verified_documents": PAGES, "verified_unique_searches": PAGES, "failed_jobs": 0, "page_attempts": PAGES,
                           "fixture": hits, "policy_requests": policy["robots_requests"], "verification_seconds": time.monotonic()-verify_start})
            resource_maxima = {}
            for sample in raw["resources"]:
                for row in sample["rows"]:
                    role = roles[row["Name"]]
                    peak = resource_maxima.setdefault(role, {"sampled_max_cli_cpu_percent": 0, "sampled_max_cli_memory_bytes": 0})
                    peak["sampled_max_cli_cpu_percent"] = max(peak["sampled_max_cli_cpu_percent"], float(row["CPUPerc"].rstrip("%")))
                    peak["sampled_max_cli_memory_bytes"] = max(peak["sampled_max_cli_memory_bytes"], memory_bytes(row["MemUsage"].split(" / ")[0]))
            assert len(resource_maxima) == 5
            result["resource_sample_count"] = len(raw["resources"])
            result["resource_maxima"] = resource_maxima
            cleanup()
            result["owned_stack_removed"] = True
            result["status"] = "passed"
        durations = [r["observed_elapsed_seconds"] for r in report["runs"]]
        report["summary"] = {"elapsed_min": min(durations), "elapsed_median": statistics.median(durations), "elapsed_max": max(durations),
                             "pages_per_second_median": statistics.median(r["pages_per_second"] for r in report["runs"])}
        report["status"] = "passed"
    except Exception as error:
        report["status"] = "failed"; report["error"] = str(error) or type(error).__name__
        if report["runs"]: report["runs"][-1]["status"] = "failed"
        print(str(error), file=sys.stderr)
    finally:
        try: cleanup()
        except Exception as error:
            report["status"] = "failed"; report["cleanup_error"] = str(error)
        report["finished_at"] = datetime.now(timezone.utc).isoformat()
        (output / "report.json").write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
        (output / "observations.json").write_text(json.dumps(observations, indent=2) + "\n", encoding="utf-8")
        log.close()
    print(json.dumps({k: v for k, v in report.items() if k in ("status", "error", "summary", "revision")}, indent=2))
    return 0 if report["status"] == "passed" else 1


if __name__ == "__main__":
    raise SystemExit(main())
