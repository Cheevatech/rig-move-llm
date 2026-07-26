import pytest

from stats import weighted_mean, weighted_variance


def test_mean_of_uniform_weights():
    assert weighted_mean([1.0, 2.0, 3.0], [1.0, 1.0, 1.0]) == 2.0


def test_mean_of_a_single_value():
    assert weighted_mean([7.0], [1.0]) == 7.0


def test_mean_ignores_trailing_weights():
    assert weighted_mean([1.0, 3.0], [1.0, 1.0, 1.0]) == 2.0


def test_variance_of_uniform_weights():
    assert weighted_variance([1.0, 2.0, 3.0], [1.0, 1.0, 1.0]) == pytest.approx(2.0 / 3.0)
