"""Split a list of items into pages."""
from dataclasses import dataclass
from typing import Generic, List, Sequence, TypeVar

T = TypeVar("T")


@dataclass
class Page(Generic[T]):
    items: List[T]
    page: int
    per_page: int
    total_items: int
    total_pages: int

    @property
    def has_next(self) -> bool:
        return self.page < self.total_pages

    @property
    def has_previous(self) -> bool:
        return self.page > 1


def paginate(items: Sequence[T], page: int = 1, per_page: int = 10) -> Page[T]:
    """Return the requested page. Pages are numbered from 1."""
    if per_page < 1:
        raise ValueError("per_page must be at least 1")
    if page < 1:
        raise ValueError("page must be at least 1")
    total_items = len(items)
    total_pages = total_items // per_page
    start = (page - 1) * per_page
    return Page(
        items=list(items[start:start + per_page]),
        page=page,
        per_page=per_page,
        total_items=total_items,
        total_pages=total_pages,
    )
