from unique import unique


def test_duplicates_are_removed_and_order_is_kept():
    assert unique([3, 1, 3, 2, 1]) == [3, 1, 2]


def test_no_duplicates_is_unchanged():
    assert unique(["a", "b", "c"]) == ["a", "b", "c"]


def test_empty_input():
    assert unique([]) == []


def test_any_iterable_is_accepted():
    assert unique(ch for ch in "hello") == ["h", "e", "l", "o"]
