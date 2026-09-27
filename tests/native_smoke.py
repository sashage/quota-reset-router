"""Exercise the release .so with CPA's exact C ABI and an isolated HTTPS fixture."""

import base64
import ctypes as C
import datetime as dt
import http.server
import json
import os
from pathlib import Path
import ssl
import subprocess
import sys
import tempfile
import threading
import time


class Buffer(C.Structure):
    _fields_ = [("ptr", C.c_void_p), ("length", C.c_size_t)]


HOST_CALL = C.CFUNCTYPE(
    C.c_int, C.c_void_p, C.c_char_p, C.c_void_p, C.c_size_t, C.POINTER(Buffer)
)
FREE = C.CFUNCTYPE(None, C.c_void_p, C.c_size_t)
PLUGIN_CALL = C.CFUNCTYPE(
    C.c_int, C.c_char_p, C.c_void_p, C.c_size_t, C.POINTER(Buffer)
)
SHUTDOWN = C.CFUNCTYPE(None)


class HostAPI(C.Structure):
    _fields_ = [
        ("abi", C.c_uint32),
        ("context", C.c_void_p),
        ("call", HOST_CALL),
        ("free", FREE),
    ]


class PluginAPI(C.Structure):
    _fields_ = [
        ("abi", C.c_uint32),
        ("call", PLUGIN_CALL),
        ("free", FREE),
        ("shutdown", SHUTDOWN),
    ]


def main():
    library = Path(sys.argv[1]).resolve()
    assert os.environ.get("PLUGIN_ISOLATED_TEST") == "1", (
        "Run only inside the network-isolated test container"
    )
    libc = C.CDLL(None)
    libc.malloc.argtypes, libc.malloc.restype = [C.c_size_t], C.c_void_p
    libc.free.argtypes = [C.c_void_p]
    claude_ids = ["a-seven-days", "b-two-days", "c-one-day", "z-five-hours"]
    codex_ids = ["codex-later", "codex-sooner"]
    ids = claude_ids + codex_ids
    durations = dict(zip(claude_ids, [168, 48, 24, 5]))
    durations.update({"codex-later": 48, "codex-sooner": 5})
    exhausted = set()
    allocations = set()
    callback_errors = []
    methods = set()
    upstream_requests = []

    def deadline(hours):
        return (
            dt.datetime.now(dt.timezone.utc) + dt.timedelta(hours=hours)
        ).isoformat()

    class Fixture(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            token = (
                self.headers.get("Authorization", "")
                .removeprefix("Bearer test-token-")
                .removeprefix("Bearer codex-token-")
            )
            assert token in durations
            upstream_requests.append(token)
            if self.path == "/api/oauth/usage":
                assert self.headers.get("anthropic-beta") == "oauth-2025-04-20"
                body = json.dumps(
                    {
                        "seven_day": {
                            "utilization": 20,
                            "resets_at": deadline(durations[token]),
                        },
                        "five_hour": {
                            "utilization": 100 if token in exhausted else 0,
                            "resets_at": deadline(2)
                            if token in exhausted
                            else deadline(1)
                            if token == claude_ids[0]
                            else None,
                        },
                    }
                ).encode()
            else:
                assert self.path == "/backend-api/wham/usage"
                body = json.dumps(
                    {
                        "plan_type": "plus",
                        "rate_limit": {
                            "primary_window": {
                                "used_percent": 20,
                                "limit_window_seconds": 18000,
                                "reset_after_seconds": 3600
                                if token == "codex-later"
                                else 14400,
                            },
                            "secondary_window": {
                                "used_percent": 20,
                                "limit_window_seconds": 604800,
                                "reset_after_seconds": durations[token] * 3600,
                            },
                        },
                    }
                ).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, format, *args):
            pass

    @HOST_CALL
    def host_call(context, method, request, length, output):
        try:
            assert context == 0x123456
            method = method.decode()
            methods.add(method)
            data = json.loads(C.string_at(request, length))
            if method == "host.auth.list":
                result = {
                    "files": [
                        {
                            "id": i,
                            "name": i + ".json",
                            "auth_index": i,
                            "provider": "claude" if i in claude_ids else "codex",
                        }
                        for i in ids
                    ]
                }
            elif method == "host.auth.get":
                i = data["auth_index"]
                assert i in ids
                result = {
                    "auth_index": i,
                    "json": {
                        "type": "claude" if i in claude_ids else "codex",
                        "access_token": (
                            "test-token-" if i in claude_ids else "codex-token-"
                        )
                        + i,
                        "account_uuid": i if i in claude_ids else "",
                        "account_id": i if i in codex_ids else "",
                    },
                }
            else:
                raise AssertionError("Unexpected host capability: " + method)
            raw = json.dumps({"ok": True, "result": result}).encode()
            pointer = libc.malloc(len(raw))
            C.memmove(pointer, raw, len(raw))
            allocations.add(pointer)
            output[0] = Buffer(pointer, len(raw))
            return 0
        except BaseException as exc:
            callback_errors.append(str(exc))
            return 1

    @FREE
    def host_free(pointer, length):
        if pointer not in allocations:
            callback_errors.append("unrecognized host buffer freed")
            return
        allocations.remove(pointer)
        libc.free(pointer)

    with tempfile.TemporaryDirectory(prefix="quota-reset-router-native-") as tmp:
        cert, key = Path(os.environ["SSL_CERT_FILE"]), Path(tmp) / "key.pem"
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
        os.environ["SSL_CERT_FILE"] = str(cert)
        server = http.server.ThreadingHTTPServer(("127.0.0.1", 443), Fixture)
        tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        tls.load_cert_chain(cert, key)
        server.socket = tls.wrap_socket(server.socket, server_side=True)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        lib = C.CDLL(str(library))
        init = lib.cliproxy_plugin_init
        init.argtypes, init.restype = (
            [C.POINTER(HostAPI), C.POINTER(PluginAPI)],
            C.c_int,
        )
        host, api = HostAPI(1, 0x123456, host_call, host_free), PluginAPI()
        invalid_host = HostAPI(999, 0x123456, host_call, host_free)
        assert init(C.byref(invalid_host), C.byref(api)) != 0
        assert init(C.byref(host), C.byref(api)) == 0
        assert api.abi == 1

        def call(method, request):
            raw = json.dumps(request).encode()
            input_buffer = C.create_string_buffer(raw)
            output = Buffer()
            code = api.call(method.encode(), input_buffer, len(raw), C.byref(output))
            assert output.ptr
            try:
                envelope = json.loads(C.string_at(output.ptr, output.length))
            finally:
                api.free(output.ptr, output.length)
            assert code == 0 and envelope["ok"], envelope
            return envelope["result"]

        def status():
            result = call(
                "management.handle",
                {
                    "Method": "GET",
                    "Path": "/v0/management/plugins/quota-reset-router/status",
                },
            )
            return json.loads(base64.b64decode(result["Body"]))

        def configure(mode, method="plugin.reconfigure", policy="weekly_reset_first"):
            payload = {
                "config_yaml": base64.b64encode(("selection_policy: " + policy + "\nmode: " + mode).encode()).decode()
            }
            reg = call(method, payload)
            assert reg["schema_version"] == 6
            assert reg["capabilities"] == {"scheduler": True, "management_api": True}
            end = time.monotonic() + 20
            while time.monotonic() < end:
                data = status()
                if len(data["accounts"]) == len(ids) and all(
                    a.get("quota") for a in data["accounts"].values()
                ):
                    return
                time.sleep(0.02)
            raise AssertionError(
                "quota refresh did not complete: "
                + json.dumps(status())
                + " callbacks="
                + repr(callback_errors)
                + " upstream="
                + repr(upstream_requests)
            )

        request = {
            "Provider": "claude",
            "Providers": ["claude"],
            "Model": "claude-sonnet-4-6",
            "Candidates": [
                {
                    "ID": i,
                    "Provider": "claude",
                    "Status": "active",
                    "Metadata": {"account_uuid": i},
                }
                for i in claude_ids
            ],
        }
        configure("shadow", "plugin.register")
        assert status()["version"] == "0.3.0"
        assert status()["selection_policy"] == "weekly_reset_first"
        assert not call("scheduler.pick", request)["Handled"]
        assert status()["last_decision"]["auth_id"] == "z-five-hours"
        assert status()["last_decision"]["reason"] == "earliest_weekly_reset"
        configure("active")
        for expected in reversed(claude_ids):
            result = call("scheduler.pick", request)
            assert result["Handled"] and result["AuthID"] == expected, result
            request["Candidates"] = [
                c for c in request["Candidates"] if c["ID"] != expected
            ]
        request["Candidates"] = [
            {"ID": i, "Provider": "claude", "Metadata": {"account_uuid": i}}
            for i in claude_ids
        ]
        exhausted.add("z-five-hours")
        configure("active")
        assert call("scheduler.pick", request)["AuthID"] == "c-one-day"
        exhausted.clear()
        durations["z-five-hours"] = 169
        configure("active")
        assert call("scheduler.pick", request)["AuthID"] == "c-one-day"
        for i in range(500):
            assert call("scheduler.pick", request)["AuthID"] == "c-one-day"
        codex_request = {
            "Provider": "codex",
            "Providers": ["codex"],
            "Model": "gpt-5-codex",
            "Candidates": [
                {"ID": i, "Provider": "codex", "Status": "active"} for i in codex_ids
            ],
        }
        assert call("scheduler.pick", codex_request)["AuthID"] == "codex-sooner"
        codex_request["Candidates"] = [
            {"ID": "codex-later", "Provider": "codex", "Status": "active"}
        ]
        assert call("scheduler.pick", codex_request)["AuthID"] == "codex-later"
        assert all(
            p not in json.dumps(status()) for p in ("test-token-", "codex-token-")
        )
        exhausted.clear()
        for account in ids:
            durations[account] = 72
        configure("active", policy="quota_balanced")
        balanced_counts = {}
        for _ in range(400):
            picked = call("scheduler.pick", request)["AuthID"]
            balanced_counts[picked] = balanced_counts.get(picked, 0) + 1
        assert len(balanced_counts) == len(claude_ids), balanced_counts
        assert max(balanced_counts.values()) - min(balanced_counts.values()) <= 2, balanced_counts
        assert status()["selection_policy"] == "quota_balanced"
        assert len(status()["last_decision"]["traffic_shares"]) == len(claude_ids)
        call("plugin.quiesce", {})
        assert not call("scheduler.pick", request)["Handled"]
        api.shutdown()
        assert not allocations, "host response buffer leak"
        assert not callback_errors, callback_errors
        assert methods == {"host.auth.list", "host.auth.get"}, methods
        assert len(upstream_requests) >= 16, upstream_requests
        server.shutdown()
        print(
            "PASS: release .so native ABI, TLS quota reads, legacy and balanced routing, reset rollover, exhaustion, 900 picks, quiesce, buffer ownership; no host writes"
        )


if __name__ == "__main__":
    main()
