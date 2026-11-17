from pagination import paginate


def test_a_partial_last_page_is_counted():
    assert paginate(list(range(25)), page=1, per_page=10).total_pages == 3


def test_the_page_before_a_partial_last_page_has_a_next_page():
    assert paginate(list(range(25)), page=2, per_page=10).has_next


def test_the_partial_last_page_holds_the_remainder_and_is_last():
    page = paginate(list(range(25)), page=3, per_page=10)
    assert page.items == [20, 21, 22, 23, 24]
    assert not page.has_next


def test_fewer_items_than_a_page_is_one_page():
    assert paginate([1, 2, 3], per_page=10).total_pages == 1


def test_no_items_is_no_pages():
    page = paginate([], per_page=10)
    assert page.total_pages == 0
    assert not page.has_next


def test_exact_multiples_are_unchanged():
    assert paginate(list(range(30)), per_page=10).total_pages == 3
