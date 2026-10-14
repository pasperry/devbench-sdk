"""The test servers admit every client of the busiest test while their accept
loop is not running (#528).

A test that drives 16 concurrent clients at serve_wsgi failed under load with
TimeoutError(60, 'Operation timed out'): the server listened with
socketserver's default backlog of 5, so whenever its accept loop fell behind
the kernel dropped SYNs, and a client whose retransmits kept hitting the full
queue gave up its connect. Here nothing ever accepts: each connect must be
admitted by the kernel alone, within a short timeout, or the test fails.
"""

from __future__ import annotations

import socket
import unittest
from typing import Any, Callable, List

from .helpers import LISTEN_BACKLOG, WSGITestServer, _IngestServer, make_server

# The most concurrent clients any test here drives at one server
# (test_requests: 16 threads).
BUSIEST_TEST_CLIENTS = 16


def _app(environ: Any, start_response: Callable[..., Any]) -> List[bytes]:
    start_response("200 OK", [])
    return [b""]


class TestServersAdmitEveryClientTest(unittest.TestCase):
    def assert_admits_without_accepting(self, port: int) -> None:
        conns: List[socket.socket] = []
        try:
            for i in range(BUSIEST_TEST_CLIENTS):
                try:
                    conns.append(socket.create_connection(("127.0.0.1", port), timeout=2))
                except OSError as e:
                    self.fail("connect %d of %d was not admitted while nothing accepted: %r"
                              % (i + 1, BUSIEST_TEST_CLIENTS, e))
        finally:
            for c in conns:
                c.close()

    def test_backlog_covers_the_busiest_test(self) -> None:
        self.assertGreater(LISTEN_BACKLOG, BUSIEST_TEST_CLIENTS)

    def test_wsgi_server(self) -> None:
        server = make_server("127.0.0.1", 0, _app, server_class=WSGITestServer)
        try:
            self.assert_admits_without_accepting(server.server_port)
        finally:
            server.server_close()

    def test_fake_ingest_server(self) -> None:
        from http.server import BaseHTTPRequestHandler

        server = _IngestServer(("127.0.0.1", 0), BaseHTTPRequestHandler)
        try:
            self.assert_admits_without_accepting(server.server_address[1])
        finally:
            server.server_close()


if __name__ == "__main__":
    unittest.main()
