"""Turn titles into URL slugs."""
import re
import unicodedata

_SEPARATORS = re.compile("[^a-z0-9]+")


def slugify(title: str, max_length: int = 60) -> str:
    """Lower-case, ASCII-only, words joined by single hyphens."""
    normalized = unicodedata.normalize("NFKD", title)
    ascii_only = normalized.encode("ascii", "ignore").decode("ascii")
    slug = _SEPARATORS.sub("-", ascii_only.lower())
    return slug[:max_length]
