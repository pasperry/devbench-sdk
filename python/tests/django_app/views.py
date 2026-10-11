import logging

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
