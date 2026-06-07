# Vertical Order Review App

`vertical-order-review-app.poly` is the canonical public example for Poly:
one order-review workflow spanning Django, FastAPI, Pydantic, Express, Zod,
React server rendering, Java/Jackson futures, Ruby ActiveRecord casting,
and Go workers.

Compile it to an OmniVM manifest:

```sh
npm run polyc -- examples/vertical-order-review-app.poly -o /tmp/vertical-order-review-app.json
```

Run it with a Docker-built OmniVM image:

```sh
docker run --rm \
  -v /tmp/vertical-order-review-app.json:/tmp/vertical-order-review-app.json:ro \
  --entrypoint manifest-runner omnivm \
  /tmp/vertical-order-review-app.json
```

The app prints one summary line:

```text
Vertical order app order=ord-42 routes=6 django=200 react=71 java=priority ruby=review-active workers=2 adjustment=7
```

That line is checked in as `test/fixtures/vertical-order-review-app.output.txt`
so docs, tests, and the runtime smoke stay aligned.

This example is intended to stay small enough to read end to end while showing
Poly’s core promise: framework objects remain in their native runtimes, typed
validation and rendering stay library-local, Java and Ruby service logic can be
called in the same workflow, and Go worker handles synchronize back into the
final Python response.
