SECRET_KEY = "polyscript-passenger-fixture"
DEBUG = False
ROOT_URLCONF = "passenger_demo.urls"
ALLOWED_HOSTS = ["*"]
MIDDLEWARE = ["passenger_demo.middleware.FixtureRequestMiddleware"]
INSTALLED_APPS = []
DEFAULT_AUTO_FIELD = "django.db.models.AutoField"
