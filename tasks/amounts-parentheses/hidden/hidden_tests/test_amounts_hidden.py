from decimal import Decimal

import pytest

from amounts import parse_amount


def test_parentheses_mean_negative():
    assert parse_amount("(12.00)") == Decimal("-12.00")


def test_parentheses_with_thousands_separators():
    assert parse_amount("(1,000)") == Decimal("-1000")
    assert parse_amount(" (2,500.75) ") == Decimal("-2500.75")


def test_empty_parentheses_are_not_an_amount():
    with pytest.raises(ValueError):
        parse_amount("()")


def test_existing_formats_still_parse():
    assert parse_amount("1,234.50") == Decimal("1234.50")
    assert parse_amount("-12.00") == Decimal("-12.00")
    with pytest.raises(ValueError):
        parse_amount("twelve")
