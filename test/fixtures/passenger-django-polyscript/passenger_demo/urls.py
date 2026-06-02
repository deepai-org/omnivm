from django.http import HttpResponse
from django.urls import path


def poly_view(request):
    from poly_feature import describe_request

    return HttpResponse(describe_request(request))


urlpatterns = [path("poly/", poly_view)]
