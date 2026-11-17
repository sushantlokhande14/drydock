from ratelimit import TokenBucket


class Clock:
    def __init__(self):
        self.now = 100.0

    def __call__(self):
        return self.now


def test_frequent_polling_still_receives_the_configured_rate():
    clock = Clock()
    bucket = TokenBucket(rate=2, capacity=2, clock=clock)
    assert bucket.allow() and bucket.allow()
    allowed = 0
    for _ in range(8):
        clock.now += 0.25
        if bucket.allow():
            allowed += 1
    # Two seconds at two per second.
    assert allowed == 4


def test_half_a_second_at_two_per_second_is_one_token():
    clock = Clock()
    bucket = TokenBucket(rate=2, capacity=2, clock=clock)
    bucket.allow()
    bucket.allow()
    clock.now += 0.5
    assert bucket.allow()
    assert not bucket.allow()


def test_the_bucket_never_exceeds_its_capacity():
    clock = Clock()
    bucket = TokenBucket(rate=5, capacity=2, clock=clock)
    clock.now += 60
    assert [bucket.allow() for _ in range(3)] == [True, True, False]
