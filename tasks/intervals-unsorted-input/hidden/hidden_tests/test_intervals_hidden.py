from intervals import merge_intervals


def test_unsorted_input_is_merged_and_returned_in_order():
    assert merge_intervals([(5, 7), (1, 3), (2, 4)]) == [(1, 4), (5, 7)]


def test_an_interval_inside_an_earlier_one_is_absorbed():
    assert merge_intervals([(1, 5), (8, 9), (2, 3)]) == [(1, 5), (8, 9)]


def test_reverse_order_input():
    assert merge_intervals([(9, 10), (5, 6), (1, 2)]) == [(1, 2), (5, 6), (9, 10)]


def test_a_generator_is_accepted():
    assert merge_intervals((start, start + 2) for start in (6, 0, 1)) == [(0, 3), (6, 8)]


def test_sorted_input_is_unchanged():
    assert merge_intervals([(1, 3), (2, 6), (8, 10)]) == [(1, 6), (8, 10)]
