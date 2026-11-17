from slugs import slugify


def test_surrounding_punctuation_and_space_leave_no_hyphens():
    assert slugify("  Hello, World!  ") == "hello-world"
    assert slugify("...draft") == "draft"
    assert slugify("draft?") == "draft"


def test_truncation_does_not_end_on_a_hyphen():
    assert slugify("alpha beta gamma", max_length=11) == "alpha-beta"


def test_a_title_with_nothing_usable_is_empty():
    assert slugify("!!! ???") == ""
    assert slugify("") == ""


def test_ordinary_titles_are_unchanged():
    assert slugify("Release notes 2.1") == "release-notes-2-1"
