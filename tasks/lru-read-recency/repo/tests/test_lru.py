import pytest

from lru import LRUCache


def test_values_can_be_stored_and_read():
    cache = LRUCache(2)
    cache.put("a", 1)
    assert cache.get("a") == 1
    assert cache.get("missing") is None
    assert cache.get("missing", 0) == 0


def test_the_oldest_write_is_evicted():
    cache = LRUCache(2)
    cache.put("a", 1)
    cache.put("b", 2)
    cache.put("c", 3)
    assert cache.get("a") is None
    assert len(cache) == 2


def test_rewriting_a_key_counts_as_use():
    cache = LRUCache(2)
    cache.put("a", 1)
    cache.put("b", 2)
    cache.put("a", 10)
    cache.put("c", 3)
    assert cache.get("a") == 10
    assert cache.get("b") is None


def test_capacity_is_validated():
    with pytest.raises(ValueError):
        LRUCache(0)
