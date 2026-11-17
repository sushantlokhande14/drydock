"""A token bucket rate limiter."""
import time
from typing import Callable


class TokenBucket:
    """Allows `rate` operations per second on average, with bursts up to `capacity`."""

    def __init__(self, rate: float, capacity: int, clock: Callable[[], float] = time.monotonic) -> None:
        if rate <= 0 or capacity < 1:
            raise ValueError("rate must be positive and capacity at least 1")
        self.rate = rate
        self.capacity = capacity
        self._clock = clock
        self._tokens = float(capacity)
        self._updated = clock()

    def _refill(self) -> None:
        now = self._clock()
        elapsed = int(now - self._updated)
        self._tokens = min(self.capacity, self._tokens + elapsed * self.rate)
        self._updated = now

    def allow(self, cost: int = 1) -> bool:
        self._refill()
        if self._tokens >= cost:
            self._tokens -= cost
            return True
        return False
