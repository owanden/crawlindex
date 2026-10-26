import hashlib
import os
from pathlib import Path
import platform as host
import sys
import urllib.request


if sys.platform not in ("win32", "linux") or host.machine().lower() not in ("amd64", "x86_64"):
    raise SystemExit("AMD64 Windows or Linux required")
target = Path(sys.argv[1]).resolve()
target.mkdir(parents=True, exist_ok=True)
platform = "windows" if os.name == "nt" else "linux"
suffix = ".exe" if os.name == "nt" else ""
assets = [
    ("kind", f"https://github.com/kubernetes-sigs/kind/releases/download/v0.30.0/kind-{platform}-amd64", ".sha256sum"),
    ("kubectl", f"https://dl.k8s.io/release/v1.34.0/bin/{platform}/amd64/kubectl{suffix}", ".sha256"),
]
for name, url, checksum_suffix in assets:
    with urllib.request.urlopen(url, timeout=60) as response:
        data = response.read()
    with urllib.request.urlopen(url + checksum_suffix, timeout=30) as response:
        expected = response.read().decode().split()[0]
    actual = hashlib.sha256(data).hexdigest()
    if actual != expected:
        raise RuntimeError(f"checksum mismatch: {name}")
    path = target / (name + suffix)
    path.write_bytes(data)
    path.chmod(0o755)
    print(f"{name}: {actual}")
