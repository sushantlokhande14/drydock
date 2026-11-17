from versions import compare_versions, latest


def test_two_digit_parts_are_compared_as_numbers():
    assert compare_versions("1.10.0", "1.9.0") == 1
    assert compare_versions("1.9.0", "1.10.0") == -1


def test_latest_handles_two_digit_parts():
    assert latest(["1.9.0", "1.10.0", "1.2.0"]) == "1.10.0"


def test_leading_zeros_do_not_matter():
    assert compare_versions("1.02.0", "1.2.0") == 0


def test_a_larger_major_wins_over_many_minors():
    assert compare_versions("v2.0", "1.99") == 1


def test_single_digit_behaviour_is_unchanged():
    assert compare_versions("1.2.3", "1.2.4") == -1
    assert compare_versions("1.2.3", "1.2.3") == 0
