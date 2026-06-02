class FixtureRequestMiddleware:
    def __init__(self, get_response):
        self.get_response = get_response

    def __call__(self, request):
        request.fixture_user_id = "u-42"
        request.session = {"feature": "poly", "visits": 3}
        response = self.get_response(request)
        response["X-Poly-Fixture"] = "middleware"
        return response
