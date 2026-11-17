import pytest

from versions import compare_versions, latest


def test_equal_versions():
    assert compare_versions("1.2.3", "1.2.3") == 0
    assert compare_versions("v1.2.3", "1.2.3") == 0


def test_single_digit_parts_compare():
    assert compare_versions("1.2.3", "1.2.4") == -1
    assert compare_versions("2.0.0", "1.9.9") == 1


def test_latest_of_simple_versions():
    assert latest(["1.0.0", "1.2.0", "1.1.0"]) == "1.2.0"


def test_invalid_versions_are_rejected():
    with pytest.raises(ValueError):
        compare_versions("1.x", "1.0")
    with pytest.raises(ValueError):
        latest([])
