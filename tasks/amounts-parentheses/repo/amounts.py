"""Parse money amounts as they appear in exported bank statements."""
from decimal import Decimal, InvalidOperation


def parse_amount(text: str) -> Decimal:
    """Parse amounts such as "1,234.50", "-12.00" and "(12.00)".

    Commas are thousands separators. Parentheses mean a negative amount.
    """
    cleaned = text.strip()
    negative = cleaned.startswith("-")
    cleaned = cleaned.lstrip("-").replace(",", "")
    try:
        value = Decimal(cleaned)
    except InvalidOperation:
        raise ValueError(f"not an amount: {text!r}") from None
    return -value if negative else value
