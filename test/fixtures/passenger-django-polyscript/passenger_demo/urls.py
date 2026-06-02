from django.http import HttpResponse
from django.urls import path

from passenger_demo.services import render_poly_response


def poly_view(request):
    return HttpResponse(render_poly_response(request))


urlpatterns = [path("poly/", poly_view)]
