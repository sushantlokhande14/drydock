import pytest

from intervals import merge_intervals


def test_overlapping_intervals_merge():
    assert merge_intervals([(1, 3), (2, 6), (8, 10)]) == [(1, 6), (8, 10)]


def test_touching_intervals_merge():
    assert merge_intervals([(1, 4), (4, 5)]) == [(1, 5)]


def test_separate_intervals_stay_separate():
    assert merge_intervals([(1, 2), (4, 5)]) == [(1, 2), (4, 5)]


def test_empty_input():
    assert merge_intervals([]) == []


def test_an_inverted_interval_is_rejected():
    with pytest.raises(ValueError):
        merge_intervals([(3, 1)])
