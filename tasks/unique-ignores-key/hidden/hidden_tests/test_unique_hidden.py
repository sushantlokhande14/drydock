from unique import unique


def test_the_key_decides_what_is_a_duplicate():
    assert unique(["a", "A", "b", "B", "a"], key=str.lower) == ["a", "b"]


def test_the_first_item_for_each_key_is_the_one_kept():
    assert unique(["Bob", "bob", "BOB"], key=str.lower) == ["Bob"]


def test_unhashable_items_work_with_a_key():
    records = [{"id": 1, "v": "x"}, {"id": 2, "v": "y"}, {"id": 1, "v": "z"}]
    assert unique(records, key=lambda record: record["id"]) == [{"id": 1, "v": "x"}, {"id": 2, "v": "y"}]


def test_without_a_key_nothing_changes():
    assert unique([3, 1, 3, 2, 1]) == [3, 1, 2]
