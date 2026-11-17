from slugs import slugify


def test_words_are_joined_by_hyphens():
    assert slugify("Hello World") == "hello-world"


def test_accents_are_removed():
    assert slugify("Creme brulee") == "creme-brulee"
    assert slugify("Crème brûlée") == "creme-brulee"


def test_runs_of_separators_collapse():
    assert slugify("one   two -- three") == "one-two-three"


def test_long_titles_are_cut():
    assert len(slugify("word " * 40)) <= 60
