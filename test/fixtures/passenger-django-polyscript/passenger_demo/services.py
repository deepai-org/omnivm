from poly_feature import describe_request


class PolyResponseService:
    def render(self, request):
        payload = describe_request(request)
        return {
            "status": payload["status"],
            "method": payload["method"],
            "path": payload["path"],
            "user_id": payload["user"]["id"],
            "feature": payload["session"]["feature"],
            "visits": payload["session"]["visits"],
            "request_id": payload["headers"]["request_id"],
            "meta_request_id": payload["meta"]["request_id"],
            "items": [
                {"kind": item["kind"], "value": item["value"]}
                for item in payload["items"]
            ],
        }


def render_poly_response(request):
    return PolyResponseService().render(request)
