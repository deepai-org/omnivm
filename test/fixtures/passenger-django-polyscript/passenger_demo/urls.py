from django.http import HttpResponse
from django.urls import path


def poly_view(request):
    import poly_feature

    return HttpResponse(poly_feature.describe_request(request))


urlpatterns = [path("poly/", poly_view)]
