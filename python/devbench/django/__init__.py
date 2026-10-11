"""Django: add one line to settings.py::

    INSTALLED_APPS = [
        "devbench.django",
        ...
    ]

At startup (``AppConfig.ready``) this puts DevbenchMiddleware first in
MIDDLEWARE (unless you listed it yourself), subscribes to
``got_request_exception``, starts log capture, and, when Celery is loaded,
installs the Celery hooks. ``python manage.py devbench_test`` checks the DSN.
"""
