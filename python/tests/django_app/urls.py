from django.urls import path

from . import views

urlpatterns = [
    path("checkout", views.checkout),
    path("handled", views.handled),
    path("missing", views.missing),
    path("fine", views.fine),
    path("async-checkout", views.async_checkout),
]
