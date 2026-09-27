"""Load a release zip into the official CLIProxyAPI binary on this machine.

Usage: load_smoke.py <cpa-archive> <cpa-checksums.txt> <plugin-zip>

Installs the plugin where the CLIProxyAPI plugin store puts it
(plugins/<goos>/<goarch>/<id>-v<version>.<ext>), starts CLIProxyAPI with no
credentials, and checks that the plugin registers and serves its status
route. Without credentials the plugin makes no network requests.
"""

import hashlib
import json
import re
import socket
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.error
import urllib.request
import zipfile
from pathlib import Path

PLUGIN = "quota-reset-router"
EXTENSIONS = {"linux": ".so", "darwin": ".dylib", "windows": ".dll"}
CPA_ARCH = {"amd64": "amd64", "arm64": "aarch64"}
MANAGEMENT_KEY = "load-smoke-management-key"
STATUS_PATH = f"/v0/management/plugins/{PLUGIN}/status"


def verify_checksum(archive, checksums):
    expected = {}
    for line in checksums.read_text().splitlines():
        parts = line.split()
        if len(parts) == 2:
            expected[parts[1]] = parts[0]
    if expected.get(archive.name) != hashlib.sha256(archive.read_bytes()).hexdigest():
        sys.exit(f"checksum mismatch for {archive.name}")


def extract_cpa(archive, dest):
    if archive.name.endswith(".zip"):
        name = "cli-proxy-api.exe"
        with zipfile.ZipFile(archive) as bundle:
            data = bundle.read(name)
    else:
        name = "cli-proxy-api"
        with tarfile.open(archive) as bundle:
            member = bundle.extractfile(name)
            if member is None:
                sys.exit(f"{name} is not a regular file in {archive.name}")
            data = member.read()
    binary = dest / name
    binary.write_bytes(data)
    binary.chmod(0o755)
    return binary


def install_plugin(plugin_zip, plugins_dir, cpa_archive):
    match = re.fullmatch(
        rf"{PLUGIN}_(\d+(?:\.\d+)+)_(linux|darwin|windows)_(amd64|arm64)\.zip",
        plugin_zip.name,
    )
    if match is None:
        sys.exit(f"unexpected plugin archive name {plugin_zip.name}")
    version, goos, goarch = match.groups()
    if f"_{goos}_{CPA_ARCH[goarch]}." not in cpa_archive.name:
        sys.exit(f"{cpa_archive.name} is not the CLIProxyAPI build for {goos}/{goarch}")
    library = PLUGIN + EXTENSIONS[goos]
    with zipfile.ZipFile(plugin_zip) as bundle:
        libraries = [
            n
            for n in bundle.namelist()
            if n.lower().endswith((".so", ".dylib", ".dll"))
        ]
        if libraries != [library]:
            sys.exit(
                f"{plugin_zip.name} must contain only {library} at its root, found {libraries}"
            )
        data = bundle.read(library)
    target = plugins_dir / goos / goarch / f"{PLUGIN}-v{version}{EXTENSIONS[goos]}"
    target.parent.mkdir(parents=True)
    target.write_bytes(data)
    target.chmod(0o755)
    return version, f"{goos}/{goarch}"


def free_port():
    with socket.socket() as probe:
        probe.bind(("127.0.0.1", 0))
        return probe.getsockname()[1]


def write_config(path, port, auth_dir, plugins_dir):
    lines = [
        "host: 127.0.0.1",
        f"port: {port}",
        f"auth-dir: {json.dumps(auth_dir.as_posix())}",
        "api-keys: [load-smoke-client-key]",
        "remote-management:",
        "  allow-remote: false",
        f"  secret-key: {MANAGEMENT_KEY}",
        "  disable-control-panel: true",
        "plugins:",
        "  enabled: true",
        f"  dir: {json.dumps(plugins_dir.as_posix())}",
        "  configs:",
        f"    {PLUGIN}:",
        "      enabled: true",
        "      priority: 10",
        "      mode: shadow",
    ]
    path.write_text("\n".join(lines) + "\n")


def read_status(port, proc, timeout):
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    request = urllib.request.Request(
        f"http://127.0.0.1:{port}{STATUS_PATH}",
        headers={"Authorization": "Bearer " + MANAGEMENT_KEY},
    )
    deadline = time.monotonic() + timeout
    last = "no response"
    while time.monotonic() < deadline:
        if proc.poll() is not None:
            raise AssertionError(f"CLIProxyAPI exited with code {proc.returncode}")
        try:
            with opener.open(request, timeout=5) as response:
                return json.load(response)
        except urllib.error.HTTPError as error:
            last = f"HTTP {error.code}"
        except (urllib.error.URLError, OSError, ValueError) as error:
            last = str(error)
        time.sleep(0.5)
    raise AssertionError(f"plugin status unavailable after {timeout}s: {last}")


def check_status(status, version):
    expected = {
        "version": version,
        "mode": "shadow",
        "selection_policy": "quota_balanced",
        "accounts": {},
        "worker_failed": False,
    }
    for key, value in expected.items():
        if status.get(key) != value:
            raise AssertionError(f"status {key} = {status.get(key)!r}, want {value!r}")
    if "list_error" in status:
        raise AssertionError(f"credential list callback failed: {status['list_error']}")


def stop(proc):
    if proc.poll() is None:
        proc.terminate()
        try:
            proc.wait(timeout=15)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait()


def main():
    if len(sys.argv) != 4:
        sys.exit(__doc__)
    cpa_archive, cpa_checksums, plugin_zip = (
        Path(arg).resolve() for arg in sys.argv[1:]
    )
    verify_checksum(cpa_archive, cpa_checksums)
    with tempfile.TemporaryDirectory(
        prefix="qrr-load-", ignore_cleanup_errors=True
    ) as tmp:
        root = Path(tmp)
        binary = extract_cpa(cpa_archive, root)
        plugins_dir = root / "plugins"
        version, platform = install_plugin(plugin_zip, plugins_dir, cpa_archive)
        auth_dir = root / "auths"
        auth_dir.mkdir()
        port = free_port()
        config = root / "config.yaml"
        write_config(config, port, auth_dir, plugins_dir)
        log_path = root / "cpa.log"
        with log_path.open("w") as log:
            proc = subprocess.Popen(
                [str(binary), "--config", str(config), "--local-model"],
                cwd=root,
                stdout=log,
                stderr=subprocess.STDOUT,
            )
            try:
                check_status(read_status(port, proc, 120), version)
                # Read again after the first quota refresh has had time to run.
                time.sleep(3)
                check_status(read_status(port, proc, 30), version)
                print(
                    f"PASS: {PLUGIN} {version} loaded and registered in {cpa_archive.name} ({platform})"
                )
            except BaseException:
                log.flush()
                print(log_path.read_text(errors="replace"), file=sys.stderr)
                raise
            finally:
                stop(proc)


if __name__ == "__main__":
    main()
