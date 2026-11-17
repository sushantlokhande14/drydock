from decimal import Decimal

import pytest

from amounts import parse_amount


def test_plain_amounts():
    assert parse_amount("12.50") == Decimal("12.50")
    assert parse_amount(" 7 ") == Decimal("7")


def test_thousands_separators():
    assert parse_amount("1,234.50") == Decimal("1234.50")
    assert parse_amount("1,000,000") == Decimal("1000000")


def test_a_leading_minus_is_negative():
    assert parse_amount("-12.00") == Decimal("-12.00")


def test_text_is_rejected():
    with pytest.raises(ValueError):
        parse_amount("abc")
