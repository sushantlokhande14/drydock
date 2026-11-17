from lru import LRUCache


def test_a_read_entry_outlives_an_unread_one():
    cache = LRUCache(2)
    cache.put("a", 1)
    cache.put("b", 2)
    assert cache.get("a") == 1
    cache.put("c", 3)
    assert cache.get("a") == 1
    assert cache.get("b") is None
    assert cache.get("c") == 3


def test_reading_a_missing_key_changes_nothing():
    cache = LRUCache(2)
    cache.put("a", 1)
    cache.put("b", 2)
    cache.get("zzz")
    cache.put("c", 3)
    assert cache.get("a") is None
    assert len(cache) == 2


def test_repeated_reads_keep_an_entry_alive():
    cache = LRUCache(3)
    for key in "abc":
        cache.put(key, key)
    for key in "defgh":
        cache.get("a")
        cache.put(key, key)
    assert cache.get("a") == "a"
