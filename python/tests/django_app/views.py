import logging
import time

from django.http import Http404, HttpResponse, JsonResponse

import devbench

log = logging.getLogger("acme.shop")


class PaymentDeclined(Exception):
    pass


def _user(request):
    devbench.set_user(email=request.headers.get("X-Demo-User"), account="acct-1182")


def checkout(request):
    _user(request)
    log.info("Charging card for %s on order 1182", request.headers.get("X-Demo-User"))
    raise PaymentDeclined("card declined for %s" % request.headers.get("X-Demo-User"))


def handled(request):
    try:
        raise ValueError("duplicate")
    except ValueError as e:
        devbench.report_handled(e, symbol="shop.handled", reason="validation")
    resp = HttpResponse("ok")
    resp["Access-Control-Expose-Headers"] = "x-request-id"
    return resp


def missing(request):
    raise Http404("no such order")


def fine(request):
    return JsonResponse({"trace": str(devbench.current_trace()), "headers": devbench.trace_headers()})


async def async_checkout(request):
    _user(request)
    log.info("async charge for %s", request.headers.get("X-Demo-User"))
    raise PaymentDeclined("async declined")


# Capability 2b (tests/request_cases.py): the same endpoints in every
# framework.


def _signals_user():
    from tests.request_cases import USER

    devbench.set_user(**USER)


def _advance(ms):
    from tests.request_cases import CLOCK

    CLOCK.advance_ms(ms)


def sig_fail(request, order):
    _signals_user()
    return HttpResponse("inventory unavailable", status=503)


def sig_boom(request):
    raise PaymentDeclined("boom")


def sig_captured(request):
    devbench.capture_exception(ValueError("upstream refused"))
    return HttpResponse("bad gateway", status=502)


def sig_handled(request):
    devbench.report_handled(ValueError("dup"), symbol="shop.signals.handled")
    return HttpResponse("unavailable", status=503)


def sig_slow(request, ms):
    _signals_user()
    _advance(ms)
    return HttpResponse("ok")


def sig_slow_captured(request):
    devbench.capture_exception(ValueError("cache miss"))
    _advance(4000)
    return HttpResponse("ok")


def sig_stream(request):
    _advance(60000)
    return HttpResponse("data: x\n\n", content_type="Text/Event-Stream; charset=utf-8")


def sig_stream_fail(request):
    return HttpResponse("data: x\n\n", status=503, content_type="text/event-stream")


def sig_limited(request):
    _signals_user()
    return HttpResponse("slow down", status=429)


def sig_real_slow(request):
    time.sleep(3.1)
    return HttpResponse("ok")


async def sig_async_fail(request, ref):
    _signals_user()
    return HttpResponse("unavailable", status=504)


class _BrokenHeaders(HttpResponse):
    def get(self, header, alternate=None):
        raise RuntimeError("a response whose headers cannot be read")


def sig_broken(request):
    return _BrokenHeaders("still answered", status=503)
