"""Synthetic suite for the recipe: one pass, one known failure."""


def test_addition_passes():
    assert 1 + 1 == 2


def test_addition_fails_on_purpose():
    assert 1 + 1 == 3, "synthetic failure: the recipe expects this test to fail"
