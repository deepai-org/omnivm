from dataclasses import dataclass
from typing import Iterable


@dataclass
class UserScore:
    user_id: str
    score: int


def rank_user(user):
    signals = [len(user.get("email", "")), int(user.get("orders", 0)) * 3]
    return UserScore(user["id"], sum(signals))


sample = {"id": "u-42", "email": "ada@example.com", "orders": 4}
result = rank_user(sample)
print(f"python compat {result.user_id} {result.score}")
