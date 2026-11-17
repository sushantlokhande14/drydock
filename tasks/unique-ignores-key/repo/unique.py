"""Remove duplicates while keeping the first occurrence of each item."""
from typing import Callable, Hashable, Iterable, List, Optional, TypeVar

T = TypeVar("T")


def unique(items: Iterable[T], key: Optional[Callable[[T], Hashable]] = None) -> List[T]:
    """Return items in their original order without duplicates.

    With `key`, two items are duplicates when key(item) is equal.
    """
    seen = set()
    result: List[T] = []
    for item in items:
        marker = item
        if marker in seen:
            continue
        seen.add(marker)
        result.append(item)
    return result
