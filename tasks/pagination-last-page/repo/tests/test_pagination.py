import pytest

from pagination import paginate


def test_first_page_of_an_exact_multiple():
    page = paginate(list(range(20)), page=1, per_page=10)
    assert page.items == list(range(10))
    assert page.total_pages == 2
    assert page.has_next
    assert not page.has_previous


def test_second_page_of_an_exact_multiple():
    page = paginate(list(range(20)), page=2, per_page=10)
    assert page.items == list(range(10, 20))
    assert not page.has_next
    assert page.has_previous


def test_arguments_are_validated():
    with pytest.raises(ValueError):
        paginate([1, 2, 3], page=0)
    with pytest.raises(ValueError):
        paginate([1, 2, 3], per_page=0)
