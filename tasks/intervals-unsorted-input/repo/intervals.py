"""Merge overlapping intervals."""
from typing import Iterable, List, Tuple

Interval = Tuple[int, int]


def merge_intervals(intervals: Iterable[Interval]) -> List[Interval]:
    """Merge intervals that overlap or touch. Each interval is (start, end) with start <= end."""
    merged: List[Interval] = []
    for start, end in intervals:
        if start > end:
            raise ValueError(f"invalid interval ({start}, {end})")
        if merged and start <= merged[-1][1]:
            merged[-1] = (merged[-1][0], max(merged[-1][1], end))
        else:
            merged.append((start, end))
    return merged
