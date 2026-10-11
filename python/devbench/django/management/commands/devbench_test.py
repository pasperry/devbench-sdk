"""``python manage.py devbench_test``: ``python -m devbench test`` with the
project's settings loaded, so the service name is the project's."""

from __future__ import annotations

from typing import Any

from django.core.management.base import BaseCommand, CommandError


class Command(BaseCommand):
    help = "Check DEVBENCH_DSN against Dev Bench and print the HTTP result"

    def handle(self, *args: Any, **options: Any) -> None:
        from devbench import _core

        if not _core.self_test(self.stdout):
            raise CommandError("Dev Bench test failed (see above)")
