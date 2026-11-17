import pytest

from ratelimit import TokenBucket


class Clock:
    def __init__(self):
        self.now = 100.0

    def __call__(self):
        return self.now


def test_a_burst_up_to_capacity_is_allowed_then_refused():
    clock = Clock()
    bucket = TokenBucket(rate=1, capacity=3, clock=clock)
    assert [bucket.allow() for _ in range(4)] == [True, True, True, False]


def test_tokens_come_back_after_whole_seconds():
    clock = Clock()
    bucket = TokenBucket(rate=1, capacity=2, clock=clock)
    assert bucket.allow() and bucket.allow()
    clock.now += 2.0
    assert bucket.allow() and bucket.allow()
    assert not bucket.allow()


def test_arguments_are_validated():
    with pytest.raises(ValueError):
        TokenBucket(rate=0, capacity=1)
    with pytest.raises(ValueError):
        TokenBucket(rate=1, capacity=0)
