import json
import time
import urllib.request


def request(path, payload=None):
    data = None if payload is None else json.dumps(payload).encode()
    req = urllib.request.Request(
        "http://127.0.0.1:8080" + path,
        data=data,
        headers={"Content-Type": "application/json"},
    )
    with urllib.request.urlopen(req, timeout=5) as response:
        return json.load(response)


deadline = time.monotonic() + 30
while True:
    try:
        assert request("/healthz")["status"] == "ok"
        break
    except (OSError, AssertionError):
        if time.monotonic() >= deadline:
            raise
        time.sleep(0.2)

first = request("/urls", {"url": "http://fixture:8081/#first"})
duplicate = request("/urls", {"url": "http://fixture:8081/#duplicate"})
assert first == duplicate, (first, duplicate)
while True:
    job = request("/jobs/" + str(first["id"]))
    results = request("/search?q=PostgreSQL")
    if job["state"] == "done" and len(results) == 2:
        break
    if time.monotonic() >= deadline:
        raise RuntimeError({"job": job, "results": results})
    time.sleep(0.2)

document = request("/documents/" + str(first["id"]))
assert document["title"] == "CrawlIndex fixture", document
assert {d["title"] for d in results} == {"Worker leases", "Document search"}
assert request("/search?q=invisibleword") == []
print(json.dumps({"deduplicated_job": job, "document": document, "search": results}, indent=2))
