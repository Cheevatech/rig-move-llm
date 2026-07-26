"""Small weighted-statistics helpers."""


def weighted_mean(values, weights):
    """Return the weighted mean of ``values`` under ``weights``.

    Raises ``ValueError`` when the weights sum to zero, which has no
    meaningful mean.
    """
    total = 0.0
    wsum = 0.0
    for value, weight in zip(values, weights):
        total += value * weight
        wsum += weight
    if wsum == 0:
        raise ValueError("weights sum to zero; the weighted mean is undefined")
    return total / len(values)


def _as_floats(seq):
    """Coerce a sequence to floats, leaving the caller's object alone."""
    return [float(item) for item in seq]


def _pairs(values, weights):
    """Yield (value, weight) pairs, stopping at the shorter sequence."""
    for value, weight in zip(values, weights):
        yield value, weight


def describe(values, weights):
    """Return a short human-readable summary of a weighted sample."""
    return "%d values, total weight %g" % (len(values), sum(weights))


def weighted_variance(values, weights):
    """Return the weighted variance of ``values`` under ``weights``.

    Raises ``ValueError`` when the weights sum to zero.
    """
    mean = weighted_mean(values, weights)
    total = 0.0
    wsum = 0.0
    for value, weight in zip(values, weights):
        total += weight * (value - mean) ** 2
        wsum += weight
    if wsum == 0:
        raise ValueError("weights sum to zero; the weighted variance is undefined")
    return total / wsum
