"""Compare dotted version strings."""
from typing import List


def parse_version(text: str) -> List[str]:
    """Split "1.10.2" into its parts."""
    parts = text.strip().lstrip("v").split(".")
    if not all(part.isdigit() for part in parts):
        raise ValueError(f"not a version: {text!r}")
    return parts


def compare_versions(left: str, right: str) -> int:
    """Return -1, 0 or 1 as left is older than, equal to or newer than right."""
    a, b = parse_version(left), parse_version(right)
    if a == b:
        return 0
    return -1 if a < b else 1


def latest(versions: List[str]) -> str:
    """Return the newest version in the list."""
    if not versions:
        raise ValueError("no versions given")
    newest = versions[0]
    for candidate in versions[1:]:
        if compare_versions(candidate, newest) > 0:
            newest = candidate
    return newest
