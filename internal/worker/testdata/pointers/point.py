"""Geometric point primitives."""

from functools import reduce

ORIGIN_CACHE = {"origin": None}
DEFAULT_DIM = 2


def _norm(seq):
    return [x for x in seq]


class Point:
    """An n-dimensional point."""

    is_Point = True

    def __new__(cls, *args, **kwargs):
        coords = _norm(args)
        return object.__new__(cls)

    def __add__(self, other):
        s, o = self._normalize(other)
        return Point([a + b for a, b in zip(s, o)])

    def __mul__(self, factor):
        coords = [simplify(x * factor) for x in self.args]
        return Point(coords, evaluate=False)

    @property
    def bounds(self):
        return (self.x, self.y)

    def _normalize(self, other):
        def pad(seq, n):
            if n <= len(seq):
                return list(seq)
            filler = [0] * (n - len(seq))
            return seq + filler

        return pad(self.args, 2), pad(other.args, 2)


class Point2D(Point):
    def rotate(self, angle):
        return self

    def scale(self, factor):
        return self * factor


async def load_points(path):
    await _warm(path)
    return []


def top_level():
    c = 1
    return c
