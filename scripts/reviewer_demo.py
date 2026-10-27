import argparse
from datetime import datetime, timezone
import hashlib
import json
from pathlib import Path
import subprocess
import sys
import uuid

ROOT = Path(__file__).resolve().parents[1]


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--output", default=".work/reviewer-" + uuid.uuid4().hex[:8])
    output = Path(parser.parse_args().output).resolve()
    output.mkdir(parents=True, exist_ok=False)
    report = {"status": "running", "started_at": datetime.now(timezone.utc).isoformat()}

    def stage(name, script, *args):
        print(f"Running {name} in an isolated disposable stack", flush=True)
        with (output / (name + "-console.log")).open("wb") as handle:
            result = subprocess.run([sys.executable, str(ROOT / "scripts" / script), "--output", str(output / name), *args], cwd=ROOT, stdout=handle, stderr=subprocess.STDOUT)
        if result.returncode:
            raise RuntimeError(f"{name} failed; inspect {name}-console.log and {name}/report.json")
        data = json.loads((output / name / "report.json").read_text(encoding="utf-8"))
        assert data["status"] == "passed" and data["revision"] == report["revision"], (name, "source/status mismatch")
        report[name + "_source_status"] = data["source_status"]
        return data

    try:
        report["revision"] = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip()
        report["source_status"] = subprocess.check_output(["git", "status", "--short"], cwd=ROOT, text=True).strip()
        corpus = stage("corpus", "workload.py", "--repeats", "1")
        refresh = stage("refresh", "recrawl_smoke.py")
        raw_bytes = (output / "corpus" / "observations.json").read_bytes()
        assert hashlib.sha256(raw_bytes).hexdigest() == corpus["observations_sha256"]
        raw = json.loads(raw_bytes)
        run = corpus["runs"][0]
        first = raw[0]["documents"][0]
        assert len(corpus["runs"]) == 1 and corpus["settings"]["repeats"] == 1
        assert run["verified_documents"] == run["verified_unique_searches"] == 24 and run["owned_stack_removed"]
        assert first["id"] == run["seed_id"] and first["search_match"]["id"] == first["id"]
        assert refresh["concurrent_duplicates"] == 16 and refresh["queued_survived_restart"] and refresh["failure_survived_restart"]
        assert refresh["initial_document"]["title"] == "Oldword" and refresh["replaced_document"]["title"] == "Freshword"
        assert refresh["failed_job"]["state"] == "failed" and refresh["failed_job"]["attempts"] == 3
        assert refresh["robots_denied_job"]["last_error"] == "robots disallowed" and refresh["requests_before_robots_denial"] == 5
        assert refresh["recovered_job"]["generation"] == 5 and refresh["recovered_job"]["state"] == "done"
        assert refresh["budget"] == {"limit": 1, "admitted": 1, "remaining": 0} and refresh["owned_stack_removed"]
        report["showcase"] = {"document": {k: first[k] for k in ("id", "url", "title", "generation", "body_excerpt", "text_sha256")},
                              "search_match": first["search_match"], "verified_documents": 24, "verified_unique_searches": 24,
                              "refresh_generations": [refresh[k]["generation"] for k in ("initial_job", "replaced_job", "failed_job", "robots_denied_job", "recovered_job")],
                              "refresh_url": refresh["replaced_document"]["url"], "refresh_budget": refresh["budget"], "owned_stacks_removed": 2}
        changed = refresh["replaced_document"]
        failed = refresh["failed_job"]
        recovered = refresh["recovered_job"]
        lines = ["CrawlIndex reviewer demo", "Source: " + report["revision"],
                 "Source status: " + ("clean" if not report["source_status"] else "modified; exact status retained in report.json"),
                 "Two independent disposable stacks: corpus and refresh. Job IDs are local to each database.", "",
                 "1. Submit and inspect the controlled corpus",
                 f"POST /urls -> root job {run['seed_id']}; 24 pages, 393216 HTML bytes, one origin, two workers x four goroutines",
                 "GET /documents/{id} -> excerpt/hash summary (full body verified): " + json.dumps(report["showcase"]["document"], ensure_ascii=False),
                 "GET /search?q=" + corpus["corpus"]["pages"][0]["token"] + " -> first-match fields: " + json.dumps(first["search_match"]),
                 "Checked: all 24 document text hashes and 24 unique-token searches; hidden script absent; common query capped at 20.",
                 f"Observed corpus window: {run['observed_elapsed_seconds']:.3f}s; one demo repetition, policy-limited, not a capacity measurement.", "",
                 "2. In a separate fresh stack, refresh changed content using one URL and admission slot",
                 "Refresh fixture URL: " + changed["url"],
                 "Initial refresh fixture: generation 1, title " + refresh["initial_document"]["title"],
                 f"POST /jobs/{changed['id']}/recrawl with generation 1 -> generation {changed['generation']}, title {changed['title']}",
                 "GET /documents/{id} after refresh -> " + json.dumps(changed),
                 "16 concurrent requests coalesced; old search term disappeared and the new term returned one document.",
                 "Queued generation 2 and its prior document survived a database/API restart before workers resumed.", "",
                 "3. Observe failure, retained content and explicit recovery",
                 f"Generation {failed['generation']} -> {failed['state']} after {failed['attempts']} HTTP 503 attempts.",
                 f"Generation {changed['generation']} document {changed['title']} remained searchable; terminal failure survived database/API restart.",
                 "Generation 4 was robots-denied at attempt 1 with zero additional page fetches; prior indexed content remained.",
                 f"A later explicit refresh -> generation {recovered['generation']}, {recovered['state']}, attempt {recovered['attempts']}, title {refresh['recovered_document']['title']}.",
                 "Admission remained limit=1, admitted=1, remaining=0 across all five refresh generations.", "",
                 "Both owned disposable stacks and their volumes were removed. Existing demo data was not reset.",
                 "For actual killed-process/pod lease recovery, policy/egress bounds and repeated measurements, see docs/walkthrough.md."]
        (output / "transcript.txt").write_bytes(("\n".join(lines) + "\n").encode("utf-8"))
        assets = ("transcript.txt", "corpus/report.json", "corpus/observations.json", "refresh/report.json")
        report["assets_sha256"] = {p: hashlib.sha256((output / p).read_bytes()).hexdigest() for p in assets}
        report["status"] = "passed"
        print("\n".join(lines))
    except Exception as error:
        report["status"] = "failed"; report["error"] = str(error) or type(error).__name__
        print(report["error"], file=sys.stderr)
    finally:
        report["finished_at"] = datetime.now(timezone.utc).isoformat()
        (output / "report.json").write_bytes((json.dumps(report, indent=2) + "\n").encode("utf-8"))
    return 0 if report["status"] == "passed" else 1


if __name__ == "__main__":
    raise SystemExit(main())
