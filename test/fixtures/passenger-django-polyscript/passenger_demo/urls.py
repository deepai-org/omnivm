from django.http import JsonResponse
from django.urls import path
from django.views import View

from passenger_demo.services import render_poly_response


class PolyView(View):
    def get(self, request):
        return JsonResponse(render_poly_response(request))


urlpatterns = [path("poly/", PolyView.as_view())]
