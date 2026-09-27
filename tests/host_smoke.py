"""Run the official CPA release binary against synthetic credentials and local TLS."""

from concurrent.futures import ThreadPoolExecutor
import datetime as dt
import hashlib
import http.server
import json
import os
from pathlib import Path
import signal
import ssl
import subprocess
import sys
import tarfile
import tempfile
import threading
import time
import urllib.error
import urllib.request


PLUGIN = "quota-reset-router"
STATUS = "/v0/management/plugins/" + PLUGIN + "/status"
CONFIG = "/v0/management/plugins/" + PLUGIN + "/config"
MODEL = "claude-opus-4-6"


def main():
    assert os.environ.get("PLUGIN_ISOLATED_TEST") == "1", (
        "Requires api.anthropic.com and chatgpt.com pinned to 127.0.0.1, as in the "
        "Linux test container or macOS CI"
    )
    if len(sys.argv) != 4:
        sys.exit(
            "usage: host_smoke.py <cpa-archive> <cpa-checksums.txt> <plugin-library>"
        )
    archive, checksums_path, library = (Path(arg).resolve() for arg in sys.argv[1:])
    soak = float(os.environ.get("SOAK_SECONDS", "0"))
    checksums = dict(
        line.split()[::-1] for line in checksums_path.read_text().splitlines()
    )
    assert hashlib.sha256(archive.read_bytes()).hexdigest() == checksums[archive.name]
    claude_ids = ["a-seven-days", "b-two-days", "c-one-day", "z-five-hours"]
    codex_ids = ["codex-later", "codex-sooner"]
    ids = claude_ids + codex_ids
    durations = dict(zip(claude_ids, [168, 48, 24, 5]))
    durations.update({"codex-later": 48, "codex-sooner": 5})
    exhausted, reject_messages = set(), set()
    quota_failure = False
    fixture_errors = []
    messages = []

    def deadline(hours):
        return (
            dt.datetime.now(dt.timezone.utc) + dt.timedelta(hours=hours)
        ).isoformat()

    class Fixture(http.server.BaseHTTPRequestHandler):
        def account(self):
            auth = self.headers.get("Authorization", "")
            token = auth.removeprefix("Bearer sk-ant-oat-test-").removeprefix(
                "Bearer codex-token-"
            )
            if token not in ids:
                fixture_errors.append("unknown synthetic account")
            return token

        def send_json(self, code, body):
            raw = json.dumps(body).encode()
            self.send_response(code)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

        def do_GET(self):
            if self.path not in ("/api/oauth/usage", "/backend-api/wham/usage"):
                fixture_errors.append("unexpected GET " + self.path)
                return self.send_json(404, {})
            account = self.account()
            if quota_failure:
                return self.send_json(503, {"error": "synthetic quota outage"})
            if self.path == "/api/oauth/usage":
                body = {
                    "seven_day": {
                        "utilization": 20,
                        "resets_at": deadline(durations[account]),
                    },
                    "five_hour": {
                        "utilization": 100 if account in exhausted else 0,
                        "resets_at": deadline(2)
                        if account in exhausted
                        else deadline(1)
                        if account == claude_ids[0]
                        else None,
                    },
                }
            else:
                body = {
                    "plan_type": "plus",
                    "rate_limit": {
                        "primary_window": {
                            "used_percent": 20,
                            "limit_window_seconds": 18000,
                            "reset_after_seconds": 7200,
                        },
                        "secondary_window": {
                            "used_percent": 20,
                            "limit_window_seconds": 604800,
                            "reset_after_seconds": durations[account] * 3600,
                        },
                    },
                }
            self.send_json(200, body)

        def do_POST(self):
            if self.path.split("?")[0] != "/v1/messages":
                fixture_errors.append("unexpected POST " + self.path)
                return self.send_json(404, {})
            account = self.account()
            body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
            messages.append(account)
            if account in reject_messages:
                return self.send_json(
                    429,
                    {
                        "type": "error",
                        "error": {
                            "type": "rate_limit_error",
                            "message": "synthetic account exhausted",
                        },
                    },
                )
            message = {
                "id": "msg_synthetic",
                "type": "message",
                "role": "assistant",
                "model": MODEL,
                "content": [{"type": "text", "text": account}],
                "stop_reason": "end_turn",
                "stop_sequence": None,
                "usage": {"input_tokens": 1, "output_tokens": 1},
            }
            if not body.get("stream"):
                return self.send_json(200, message)
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.end_headers()
            start = dict(message, content=[], stop_reason=None)
            events = [
                ("message_start", {"type": "message_start", "message": start}),
                (
                    "content_block_start",
                    {
                        "type": "content_block_start",
                        "index": 0,
                        "content_block": {"type": "text", "text": ""},
                    },
                ),
                (
                    "content_block_delta",
                    {
                        "type": "content_block_delta",
                        "index": 0,
                        "delta": {"type": "text_delta", "text": account},
                    },
                ),
                ("content_block_stop", {"type": "content_block_stop", "index": 0}),
                (
                    "message_delta",
                    {
                        "type": "message_delta",
                        "delta": {"stop_reason": "end_turn", "stop_sequence": None},
                        "usage": {"output_tokens": 1},
                    },
                ),
                ("message_stop", {"type": "message_stop"}),
            ]
            for event, payload in events:
                self.wfile.write(
                    (
                        "event: " + event + "\ndata: " + json.dumps(payload) + "\n\n"
                    ).encode()
                )
            self.wfile.flush()

        def log_message(self, format, *args):
            pass

    with tempfile.TemporaryDirectory(prefix="cpa-host-smoke-") as tmp:
        root = Path(tmp)
        binary = root / "cli-proxy-api"
        with tarfile.open(archive) as tar:
            member = next(
                m for m in tar if Path(m.name).name == "cli-proxy-api" and m.isfile()
            )
            extracted = tar.extractfile(member)
            assert extracted is not None
            binary.write_bytes(extracted.read())
        binary.chmod(0o700)
        if sys.platform == "darwin":
            # Go on macOS ignores SSL_CERT_FILE; CI trusts this cert in the keychain.
            cert, key = (
                Path(os.environ["FIXTURE_CERT"]),
                Path(os.environ["FIXTURE_KEY"]),
            )
        else:
            cert, key = root / "cert.pem", root / "key.pem"
            subprocess.run(
                [
                    "openssl",
                    "req",
                    "-x509",
                    "-newkey",
                    "rsa:2048",
                    "-nodes",
                    "-days",
                    "1",
                    "-keyout",
                    str(key),
                    "-out",
                    str(cert),
                    "-subj",
                    "/CN=api.anthropic.com",
                    "-addext",
                    "subjectAltName=DNS:api.anthropic.com,DNS:chatgpt.com",
                ],
                check=True,
                capture_output=True,
            )
        # macOS allows unprivileged binds below port 1024 only on the wildcard address.
        bind = "0.0.0.0" if sys.platform == "darwin" else "127.0.0.1"
        server = http.server.ThreadingHTTPServer((bind, 443), Fixture)
        tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        tls.load_cert_chain(cert, key)
        server.socket = tls.wrap_socket(server.socket, server_side=True)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        auths = root / "auths"
        auths.mkdir()
        for account in ids:
            (auths / (account + ".json")).write_text(
                json.dumps(
                    {
                        "type": "claude" if account in claude_ids else "codex",
                        "access_token": (
                            "sk-ant-oat-test-"
                            if account in claude_ids
                            else "codex-token-"
                        )
                        + account,
                        "account_uuid": account if account in claude_ids else "",
                        "account_id": account if account in codex_ids else "",
                        "email": account + "@example.invalid",
                        "expired": "2099-01-01T00:00:00Z",
                        "last_refresh": deadline(0),
                        "disabled": False,
                        "priority": 0,
                    }
                )
            )
        plugin_dir = root / "plugins"
        plugin_dir.mkdir()
        (plugin_dir / (PLUGIN + library.suffix)).write_bytes(library.read_bytes())
        config = root / "config.yaml"
        config.write_text(f"""host: 127.0.0.1
port: 18318
auth-dir: {auths}
api-keys: [synthetic-client-key]
remote-management:
  allow-remote: false
  secret-key: synthetic-management-key
  disable-control-panel: true
routing:
  strategy: fill-first
  session-affinity: false
request-retry: 3
max-retry-credentials: 0
max-retry-interval: 1
plugins:
  enabled: true
  dir: {plugin_dir}
  configs:
    {PLUGIN}:
      enabled: true
      priority: 10
      mode: shadow
      selection_policy: weekly_reset_first
""")
        log = (root / "host.log").open("w+")
        env = dict(os.environ, SSL_CERT_FILE=str(cert), GOMEMLIMIT="256MiB")
        if soak:
            # Force frequent GC in both runtimes while they share the process.
            env["GOGC"] = "10"
        proc = subprocess.Popen(
            [str(binary), "--config", str(config), "--local-model"],
            cwd=root,
            env=env,
            stdout=log,
            stderr=subprocess.STDOUT,
        )
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

        def api(path, method="GET", body=None, management=True):
            key = "synthetic-management-key" if management else "synthetic-client-key"
            request = urllib.request.Request(
                "http://127.0.0.1:18318" + path,
                data=None if body is None else json.dumps(body).encode(),
                headers={
                    "Authorization": "Bearer " + key,
                    "Content-Type": "application/json",
                },
                method=method,
            )
            with opener.open(request, timeout=15) as response:
                return response.read()

        def wait_for(check, label, timeout=25):
            end = time.monotonic() + timeout
            last = None
            while time.monotonic() < end:
                try:
                    last = check()
                    if last:
                        return last
                except (urllib.error.URLError, ValueError) as exc:
                    last = str(exc)
                if proc.poll() is not None:
                    raise AssertionError("CPA exited during " + label)
                time.sleep(0.2)
            raise AssertionError(label + ": " + repr(last))

        def status():
            return json.loads(api(STATUS))

        def ready(mode):
            s = status()
            return (
                s["mode"] == mode
                and len(s["accounts"]) == len(ids)
                and all(a.get("quota") for a in s["accounts"].values())
            )

        def configure(mode, interval="5m"):
            api(CONFIG, "PATCH", {"mode": mode, "poll_interval": interval})
            wait_for(
                lambda: (
                    ready(mode)
                    and status()["poll_interval"]
                    == ("5m0s" if interval == "5m" else "1m0s")
                ),
                "plugin reconfigure",
            )

        def message(stream=False):
            raw = api(
                "/v1/messages",
                "POST",
                {
                    "model": MODEL,
                    "max_tokens": 16,
                    "stream": stream,
                    "messages": [{"role": "user", "content": "Synthetic routing test"}],
                },
                False,
            )
            if stream:
                return raw.decode()
            return json.loads(raw)["content"][0]["text"]

        try:
            wait_for(lambda: ready("shadow"), "startup and quota discovery")
            assert status()["version"] == "0.3.0"
            assert status()["selection_policy"] == "weekly_reset_first"
            assert message() == "a-seven-days", "shadow changed built-in routing"
            assert status()["last_decision"]["auth_id"] == "z-five-hours.json"
            assert status()["last_decision"]["reason"] == "earliest_weekly_reset"
            configure("active")
            assert message() == "z-five-hours"
            assert "z-five-hours" in message(True), "streaming routing failed"
            with ThreadPoolExecutor(max_workers=8) as pool:
                assert (
                    list(pool.map(lambda _: message(), range(32)))
                    == ["z-five-hours"] * 32
                )
            end, soaked = time.monotonic() + soak, 0
            with ThreadPoolExecutor(max_workers=16) as pool:
                while time.monotonic() < end:
                    replies = list(pool.map(lambda i: message(i % 4 == 0), range(64)))
                    assert all("z-five-hours" in r for r in replies), (
                        "soak routing failed"
                    )
                    soaked += len(replies)
            exhausted.add("z-five-hours")
            configure("active", "1m")
            assert message() == "c-one-day", "short-window exhausted account selected"
            exhausted.clear()
            durations["z-five-hours"] = 169
            configure("active")
            assert message() == "c-one-day", "reset rollover ordering failed"
            quota_failure = True
            api(CONFIG, "PATCH", {"poll_interval": "1m"})
            wait_for(
                lambda: (
                    status()["poll_interval"] == "1m0s"
                    and all(
                        a.get("last_error") == "quota_http_503"
                        for a in status()["accounts"].values()
                    )
                ),
                "quota outage",
            )
            assert message() == "a-seven-days", (
                "quota outage did not delegate to fill-first"
            )
            quota_failure = False
            durations["z-five-hours"] = 5
            configure("active")
            reject_messages.add("z-five-hours")
            assert message() == "c-one-day", (
                "host 429 retry did not select next available account"
            )
            api(
                "/v0/management/plugins/" + PLUGIN + "/enabled",
                "PATCH",
                {"enabled": False},
            )
            wait_for(
                lambda: message() == "a-seven-days",
                "disable and original routing recovery",
            )
            for path in auths.glob("*.json"):
                stored = json.loads(path.read_text())
                assert stored.get("priority", 0) == 0 and not stored.get("disabled"), (
                    "credential routing fields changed"
                )
            assert not fixture_errors, fixture_errors
            soak_note = f"{soaked} requests in a {soak:.0f} s soak, " if soak else ""
            print(
                "PASS: official release checksum verified; shadow/active, streaming, 32 concurrent requests, "
                + soak_note
                + "exhaustion, reset rollover, quota outage, 429 failover, hot-disable rollback; RSS "
                + rss_kib(proc.pid)
                + " KiB"
            )
        except BaseException:
            log.flush()
            log.seek(0)
            print(
                "\n".join(
                    line for line in log.read().splitlines() if STATUS not in line
                ),
                file=sys.stderr,
            )
            raise
        finally:
            proc.send_signal(signal.SIGTERM)
            try:
                proc.wait(timeout=15)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait()
                raise AssertionError("CPA shutdown timed out")
            log.close()
            server.shutdown()


def rss_kib(pid):
    if sys.platform == "darwin":
        return subprocess.run(
            ["ps", "-o", "rss=", "-p", str(pid)],
            check=True,
            capture_output=True,
            text=True,
        ).stdout.strip()
    status = Path(f"/proc/{pid}/status").read_text()
    return next(
        line.split()[1] for line in status.splitlines() if line.startswith("VmRSS:")
    )


if __name__ == "__main__":
    main()
