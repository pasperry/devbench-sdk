"""Where reports go, parsed from DEVBENCH_DSN, and everything else read at
start (docs/SERVER_SDK_SPEC.md, "Dev Bench naming and configuration";
DECISIONS #161)."""

from __future__ import annotations

import os
import re
from typing import Mapping, Optional
from urllib.parse import quote, unquote, urlsplit

SHAPE = "https://<public>:<secret>@<host>"
_LOOPBACK = re.compile(r"\A(?:localhost|127(?:\.[0-9]{1,3}){3}|::1|\[::1\])\Z|\.localhost\Z", re.IGNORECASE)


class InvalidDSN(ValueError):
    """A DSN that does not parse. The message says what to fix and never
    contains either key."""


class DSN:
    """``https://<public>:<secret>@<host>[:port]`` from ``adt dsn create``, or
    a single-key ``https://<key>@<host>``.

    ``scheme://host[:port]`` is the ingest base. The secret (a server key) is
    what this process authenticates with; the public part (a browser key) is
    only ever handed to a page, via ``browser_dsn``. A path or query, if
    present, is ignored: the keys alone identify tenant, environment and
    source.
    """

    __slots__ = ("base", "key", "public_key")

    def __init__(self, base: str, key: str, public_key: Optional[str] = None) -> None:
        self.base = base
        self.key = key
        self.public_key = public_key

    @property
    def flush_url(self) -> str:
        return self.base + "/v1/flush"

    @property
    def check_url(self) -> str:
        """The self-test endpoint: validates the key, stores nothing."""
        return self.base + "/v1/check"

    @property
    def logs_url(self) -> str:
        return self.base + "/v1/logs"

    @property
    def browser_dsn(self) -> Optional[str]:
        """The DSN a browser may hold: https://<public>@host, or None when
        there is no public part."""
        if self.public_key is None:
            return None
        scheme, rest = self.base.split("://", 1)
        return "%s://%s@%s" % (scheme, quote(self.public_key, safe=""), rest)

    def __str__(self) -> str:
        # The DSN without its keys. The secret must never be printed.
        return self.base

    __repr__ = __str__

    @classmethod
    def parse(cls, raw: object) -> "DSN":
        text = ("" if raw is None else str(raw)).strip()
        if not text:
            raise InvalidDSN("is empty")
        try:
            parts = urlsplit(text)
            port = parts.port
        except ValueError:
            raise InvalidDSN("is not a URL (expected %s)" % SHAPE) from None

        scheme = (parts.scheme or "").lower()
        if scheme not in ("http", "https"):
            raise InvalidDSN("must start with https:// (expected %s)" % SHAPE)
        host = parts.hostname or ""
        if not host:
            raise InvalidDSN("has no host (expected %s)" % SHAPE)

        user = unquote(parts.username or "").strip()
        secret = unquote(parts.password or "").strip()
        if not user and not secret:
            raise InvalidDSN("has no key (expected %s)" % SHAPE)

        # The secret is a real secret on a server. Over plain http it would
        # cross the network readable by anything on the path, so http is
        # accepted only for a loopback ingest (local development and tests).
        if scheme == "http" and not _LOOPBACK.search(host):
            raise InvalidDSN("uses http:// for %s: the key would cross the network in plaintext; use https://" % host)

        default_port = 443 if scheme == "https" else 80
        port_text = ":%d" % port if port is not None and port != default_port else ""
        if ":" in host and not host.startswith("["):
            host = "[%s]" % host
        base = "%s://%s%s" % (scheme, host, port_text)
        if not secret:
            return cls(base, user)
        return cls(base, secret, user or None)


# Tried in order after the REVISION file when no release is configured. Common
# deploy tools and platforms only; never an app's own variable. The same order
# as the Ruby and Go SDKs (#23).
RELEASE_FALLBACKS = ("HEROKU_SLUG_COMMIT", "KAMAL_VERSION", "GITHUB_SHA", "GIT_SHA", "SOURCE_VERSION", "RENDER_GIT_COMMIT")
# Capistrano-style deploys write the deployed commit here, in the app root.
REVISION_FILE = "REVISION"
# A commit or tag, not a document: read no more than this.
REVISION_MAX_BYTES = 255
_OFF = ("false", "0", "no", "off")


def _presence(value: object) -> Optional[str]:
    if value is None:
        return None
    text = str(value).strip()
    return text or None


class Configuration:
    """Everything Dev Bench reads at start. From the environment by default;
    ``devbench.configure(...)`` sets the same fields from code.

    * ``DEVBENCH_DSN``: where to send (fallback ``ADT_DSN``). Unset: nothing
      is sent (this SDK has no sidecar mode).
    * ``DEVBENCH_SERVICE``: this app's name. Default: what the framework
      knows (the Django project, the Flask app's import name), else "app";
      with "-celery" appended in a Celery worker.
    * ``DEVBENCH_RELEASE``: the deployed version. Default, first found: a
      REVISION file in the app root, then RELEASE_FALLBACKS, else "".
    * ``DEVBENCH_ENABLED``: "false" turns everything off.
    """

    def __init__(self, env: Optional[Mapping[str, str]] = None) -> None:
        env = os.environ if env is None else env
        self.dsn: Optional[str] = _presence(env.get("DEVBENCH_DSN")) or _presence(env.get("ADT_DSN"))
        self.service: Optional[str] = _presence(env.get("DEVBENCH_SERVICE"))
        self.release: Optional[str] = _presence(env.get("DEVBENCH_RELEASE"))
        self.enabled: bool = (env.get("DEVBENCH_ENABLED") or "").strip().lower() not in _OFF
        # Seconds between background flushes (spec: 60).
        self.flush_interval: float = 60.0
        self._release_env = next((v for v in (_presence(env.get(n)) for n in RELEASE_FALLBACKS) if v), None)
        # Set by the integrations, never by the app: the framework's own name
        # for the app, the app root (REVISION, relative frame paths), and
        # whether this process is a Celery worker.
        self.framework_service: Optional[str] = None
        self.app_root: Optional[str] = None
        self.worker: Optional[str] = None
        self._detected_release: Optional[str] = None

    def resolved_service(self) -> str:
        """The service name sent with every flush and folded into every
        fingerprint. An explicit service always wins; a worker process
        defaults to "<app>-<worker>", so an app's web and job processes are
        told apart with no DEVBENCH_SERVICE at all."""
        explicit = _presence(self.service)
        if explicit:
            return explicit
        base = _presence(self.framework_service) or "app"
        return "%s-%s" % (base, self.worker) if self.worker else base

    def resolved_root(self) -> str:
        return _presence(self.app_root) or os.getcwd()

    def resolved_release(self) -> str:
        """Explicit (configure or DEVBENCH_RELEASE) wins; else the REVISION
        file in the app root, read once; else RELEASE_FALLBACKS; else ""."""
        explicit = _presence(self.release)
        if explicit:
            return explicit
        if self._detected_release is None:
            self._detected_release = self._revision_file() or self._release_env or ""
        return self._detected_release

    def _revision_file(self) -> Optional[str]:
        try:
            path = os.path.join(self.resolved_root(), REVISION_FILE)
            if not os.path.isfile(path):
                return None
            with open(path, "rb") as f:
                head = f.read(REVISION_MAX_BYTES)
            lines = head.decode("utf-8", "replace").splitlines()
            return _presence(lines[0]) if lines else None
        except Exception:
            return None
