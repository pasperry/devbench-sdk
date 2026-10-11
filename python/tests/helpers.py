"""Test support: a real HTTP server standing in for Dev Bench ingest and its
presigned evidence URLs, real servers for the frameworks, and the shared
vectors.

FakeIngest is not a stub of our client: the SDK speaks real HTTP over a real
socket to it, and the tests assert on the bytes that arrived.
"""

from __future__ import annotations

import json
import os
import socket
import threading
import time
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any, Callable, Dict, List, Optional

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
# DEVBENCH_TESTDATA: the public SDK repo's CI, whose layout differs.
TESTDATA = os.environ.get("DEVBENCH_TESTDATA") or os.path.join(ROOT, "..", "..", "..", "testdata")


def vectors(name: str, case: unittest.TestCase) -> Any:
    """A shared vector file. Missing: skipped in a vendored copy, a failure
    when ADT_REQUIRE_VECTORS=1 (CI), so the contract is never skipped
    silently."""
    path = os.path.join(TESTDATA, name)
    if not os.path.exists(path):
        if os.environ.get("ADT_REQUIRE_VECTORS") == "1":
            case.fail("missing %s" % path)
        case.skipTest("shared vectors live in the repo (testdata/), not in a vendored copy")
    with open(path, encoding="utf-8") as f:
        return json.load(f)


def require(module: str, case: unittest.TestCase) -> None:
    """Imports a framework. Missing: a failure when
    DEVBENCH_REQUIRE_FRAMEWORKS=1 (CI), so a test never skips silently."""
    try:
        __import__(module)
    except ImportError:
        if os.environ.get("DEVBENCH_REQUIRE_FRAMEWORKS") == "1":
            case.fail("%s is not installed" % module)
        case.skipTest("%s is not installed" % module)


class Request:
    def __init__(self, method: str, path: str, headers: Dict[str, str], body: bytes) -> None:
        self.method = method
        self.path = path
        self.headers = headers
        self.body = body

    def json(self) -> Any:
        return json.loads(self.body.decode("utf-8"))


class FakeIngest:
    """Answers POST /v1/flush, POST /v1/logs, POST /v1/check and PUT
    /evidence/<fp> (the presigned URL it hands out)."""

    def __init__(self) -> None:
        self.requests: List[Request] = []
        self.lock = threading.Lock()
        self.arrived = threading.Condition(self.lock)
        # Status for POST /v1/flush; a callable gets the request.
        self.flush_status: Any = 200
        self.ask_evidence = False
        # Trace keys every flush response asks lines for (need_logs).
        self.need_logs: List[str] = []
        # Offer need_logs only on an empty flush (a poll).
        self.need_logs_only_on_poll = False
        # The learned rule set every flush response carries, or None to omit.
        self.redaction: Optional[list] = None
        self.logs_status = 200
        self.check_status = 200
        self.delay = 0.0
        self._asked: Dict[str, bool] = {}
        ingest = self

        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *args: Any) -> None:
                pass

            def _read(self) -> Request:
                length = int(self.headers.get("content-length") or 0)
                body = self.rfile.read(length) if length else b""
                headers = {k.lower(): v for k, v in self.headers.items()}
                return Request(self.command, self.path, headers, body)

            def _answer(self, status: int, body: Any = None) -> None:
                data = b"" if body is None else json.dumps(body).encode("utf-8")
                self.send_response(status)
                self.send_header("content-type", "application/json")
                self.send_header("content-length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

            def do_POST(self) -> None:  # noqa: N802
                req = self._read()
                if ingest.delay:
                    time.sleep(ingest.delay)
                ingest._record(req)
                status, body = ingest._respond(req, "http://%s:%d" % self.server.server_address[:2])
                self._answer(status, body)

            def do_PUT(self) -> None:  # noqa: N802
                req = self._read()
                ingest._record(req)
                self._answer(200, {})

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.server.daemon_threads = True
        self.port = self.server.server_address[1]
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    @property
    def base(self) -> str:
        return "http://127.0.0.1:%d" % self.port

    def dsn(self, public: str = "adt_client_testpub", secret: str = "adt_server_testsecret") -> str:
        return "http://%s:%s@127.0.0.1:%d" % (public, secret, self.port)

    def close(self) -> None:
        self.server.shutdown()
        self.server.server_close()

    def _record(self, req: Request) -> None:
        with self.arrived:
            self.requests.append(req)
            self.arrived.notify_all()

    def _respond(self, req: Request, base: str) -> "tuple[int, Any]":
        if req.path == "/v1/check":
            return self.check_status, {"tenant": "acme", "environment": "production"} if self.check_status == 200 else {"error": "invalid key"}
        if req.path == "/v1/logs":
            return self.logs_status, {}
        status = self.flush_status(req) if callable(self.flush_status) else self.flush_status
        if status != 200:
            return status, {"error": "refused"}
        body = req.json()
        answer: Dict[str, Any] = {}
        if self.ask_evidence:
            asks = []
            for c in body.get("counts", []):
                if not self._asked.get(c["fp"]):
                    self._asked[c["fp"]] = True
                    asks.append({"fp": c["fp"], "claim": "c_" + c["fp"][:6], "url": "%s/evidence/%s" % (base, c["fp"]),
                                 "expires": int(time.time()) + 600})
            answer["need_evidence"] = asks
        if self.need_logs and (not self.need_logs_only_on_poll or not body.get("counts")):
            answer["need_logs"] = [{"trace": k} for k in self.need_logs]
        if self.redaction is not None:
            answer["redaction"] = self.redaction
        return 200, answer

    # ---- What arrived

    def all(self) -> List[Request]:
        with self.lock:
            return list(self.requests)

    def flushes(self) -> List[Request]:
        return [r for r in self.all() if r.method == "POST" and r.path == "/v1/flush"]

    def counts(self) -> List[dict]:
        return [c for r in self.flushes() for c in r.json().get("counts", [])]

    def evidence(self) -> List[Request]:
        return [r for r in self.all() if r.method == "PUT"]

    def log_deliveries(self) -> List[Request]:
        return [r for r in self.all() if r.method == "POST" and r.path == "/v1/logs"]

    def slices(self) -> List[dict]:
        return [s for r in self.log_deliveries() for s in r.json()["slices"]]

    def wait_for(self, predicate: Callable[[], bool], timeout: float = 10.0) -> bool:
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if predicate():
                return True
            time.sleep(0.02)
        return predicate()


def dead_port() -> int:
    """A port nothing listens on."""
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def http(method: str, url: str, headers: Optional[Dict[str, str]] = None, body: Optional[bytes] = None,
         timeout: float = 10.0) -> "tuple[int, Dict[str, str], bytes]":
    """A real HTTP request to a test server. Never raises on a status."""
    import urllib.error
    import urllib.request

    req = urllib.request.Request(url, data=body, headers=headers or {}, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return resp.status, {k.lower(): v for k, v in resp.headers.items()}, resp.read()
    except urllib.error.HTTPError as e:
        return e.code, {k.lower(): v for k, v in e.headers.items()}, e.read()


def serve_wsgi(app: Any) -> "tuple[str, Callable[[], None]]":
    """Serves a WSGI app over real HTTP on 127.0.0.1 in a thread. Returns the
    base URL and a stop function."""
    from wsgiref.simple_server import WSGIRequestHandler, WSGIServer, make_server
    from socketserver import ThreadingMixIn

    class Server(ThreadingMixIn, WSGIServer):
        daemon_threads = True

    class Quiet(WSGIRequestHandler):
        def log_message(self, *args: Any) -> None:
            pass

    server = make_server("127.0.0.1", 0, app, server_class=Server, handler_class=Quiet)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()

    def stop() -> None:
        server.shutdown()
        server.server_close()

    return "http://127.0.0.1:%d" % server.server_port, stop


def fresh(dsn: Optional[str] = None, **config: Any) -> None:
    """Resets the SDK and configures it for one test."""
    import devbench
    from devbench import _core, logs

    _core.reset()
    logs.set_buffer(logs.Buffer())
    if dsn is not None:
        devbench.configure(dsn=dsn, **config)
    elif config:
        devbench.configure(**config)


def serve_asgi(app: Any) -> "tuple[str, Callable[[], None]]":
    """Serves an ASGI app over real HTTP with uvicorn in a thread."""
    import uvicorn

    port = dead_port()
    server = uvicorn.Server(uvicorn.Config(app, host="127.0.0.1", port=port, log_level="warning", lifespan="off"))
    thread = threading.Thread(target=server.run, daemon=True)
    thread.start()
    deadline = time.monotonic() + 10
    while not server.started:
        if time.monotonic() > deadline:
            raise RuntimeError("uvicorn did not start")
        time.sleep(0.02)

    def stop() -> None:
        server.should_exit = True
        thread.join(5)

    return "http://127.0.0.1:%d" % port, stop
