from pathlib import Path

from django.http import HttpResponse
from django.urls import path


def poly_view(request):
    import poly_feature

    marker = Path("/tmp/polyscript-passenger-marker.txt").read_text(encoding="utf-8")
    return HttpResponse(f"{marker}:{Path(poly_feature.__poly_manifest__).suffix}")


urlpatterns = [path("poly/", poly_view)]
