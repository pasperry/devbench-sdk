"""FastAPI (and Starlette)::

    from fastapi import FastAPI
    from devbench.fastapi import DevbenchMiddleware

    app = FastAPI()
    app.add_middleware(DevbenchMiddleware)

The same middleware as devbench.asgi.
"""

from .asgi import DevbenchMiddleware

__all__ = ["DevbenchMiddleware"]
