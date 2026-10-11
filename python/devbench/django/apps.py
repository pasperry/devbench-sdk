from __future__ import annotations

import os
import sys
from typing import Any, Optional

from django.apps import AppConfig

MIDDLEWARE_PATH = "devbench.django.middleware.DevbenchMiddleware"


class DevbenchConfig(AppConfig):
    name = "devbench.django"
    label = "devbench"
    verbose_name = "Dev Bench"
    default = True

    def ready(self) -> None:
        # A diagnostics package that can fail a boot is worse than none.
        try:
            from .. import _core, _integration

            if not _core.enabled():
                return
            from django.conf import settings

            _integration.setup(service=_project_name(settings), root=_base_dir(settings))
            insert_middleware(settings)

            from django.core.signals import got_request_exception

            from .middleware import on_request_exception

            got_request_exception.connect(on_request_exception, dispatch_uid="devbench.request_exception")

            # The standard Celery layout imports the Celery app from the
            # project package, so it is loaded by now, in the web process and
            # the worker alike.
            if "celery" in sys.modules:
                from .. import celery as devbench_celery

                devbench_celery.install()
        except Exception:
            pass


def insert_middleware(settings: Any) -> bool:
    """Puts DevbenchMiddleware first, as sentry-sdk instruments the handler,
    so ``"devbench.django"`` in INSTALLED_APPS is the whole install. Not when
    the app already lists it. Runs in ready(), before Django's handler reads
    MIDDLEWARE. Returns True when it was added."""
    current = list(getattr(settings, "MIDDLEWARE", None) or [])
    if MIDDLEWARE_PATH in current:
        return False
    settings.MIDDLEWARE = [MIDDLEWARE_PATH] + current
    return True


def _project_name(settings: Any) -> Optional[str]:
    # DJANGO_SETTINGS_MODULE "acme_shop.settings" -> "acme_shop".
    module = getattr(settings, "SETTINGS_MODULE", None) or os.environ.get("DJANGO_SETTINGS_MODULE") or ""
    head = module.split(".")[0]
    return head if "." in module else None


def _base_dir(settings: Any) -> Optional[str]:
    base = getattr(settings, "BASE_DIR", None)
    return str(base) if base else None
