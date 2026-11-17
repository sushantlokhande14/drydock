"""A small least-recently-used cache."""
from collections import OrderedDict
from typing import Any, Hashable, Optional


class LRUCache:
    def __init__(self, capacity: int) -> None:
        if capacity < 1:
            raise ValueError("capacity must be at least 1")
        self.capacity = capacity
        self._entries: "OrderedDict[Hashable, Any]" = OrderedDict()

    def __len__(self) -> int:
        return len(self._entries)

    def get(self, key: Hashable, default: Optional[Any] = None) -> Any:
        if key not in self._entries:
            return default
        return self._entries[key]

    def put(self, key: Hashable, value: Any) -> None:
        if key in self._entries:
            self._entries.move_to_end(key)
        self._entries[key] = value
        if len(self._entries) > self.capacity:
            self._entries.popitem(last=False)
