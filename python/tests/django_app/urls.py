from django.urls import include, path

from . import views

urlpatterns = [
    path("checkout", views.checkout),
    path("handled", views.handled),
    path("missing", views.missing),
    path("fine", views.fine),
    path("async-checkout", views.async_checkout),
    path("fail/<int:order>", views.sig_fail),
    path("boom", views.sig_boom),
    path("captured", views.sig_captured),
    path("sig-handled", views.sig_handled),
    path("slow/<int:ms>", views.sig_slow),
    path("slow-captured", views.sig_slow_captured),
    path("stream", views.sig_stream),
    path("stream-fail", views.sig_stream_fail),
    path("limited", views.sig_limited),
    path("real-slow", views.sig_real_slow),
    path("v2/", include([path("orders/<slug:ref>/async-fail", views.sig_async_fail)])),
    path("broken", views.sig_broken),
]
