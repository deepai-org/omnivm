"""A Python team's code calling INTO the Rust risk kernel.

`risk.pyi` (generated from the Rust signatures by polyc-stubs) lets their
EXISTING mypy type-check these cross-language calls. The calls below are
deliberately wrong; mypy flags them against the generated stub.

Run:  mypy wrong_call.py
"""

from risk import score_batch, lookup_tier

ids = [101, 202, 303]

# ERROR 1: `weight` is float, but a string literal is passed.
#   risk.pyi declares: score_batch(account_ids: list[int], weight: float, normalize: bool) -> list[float]
#   mypy reports:
#     error: Argument 2 to "score_batch" has incompatible type "str"; expected "float"  [arg-type]
scores = score_batch(ids, "0.5", True)

# ERROR 2: passing a list[str] where list[int] is expected.
#   mypy reports (one per bad element):
#     error: List item 0 has incompatible type "str"; expected "int"  [list-item]
bad_scores = score_batch(["a", "b"], 0.5, True)

# ERROR 3: lookup_tier returns `str | None`; calling .upper() ignores None.
#   mypy reports:
#     error: Item "None" of "str | None" has no attribute "upper"  [union-attr]
tier = lookup_tier(202)
shout = tier.upper()

print(scores, bad_scores, shout)
