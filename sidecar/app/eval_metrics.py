"""Pure metric helpers for the offline RAG evaluation runner.

Kept dependency-free so the unit tests can run with plain Python.
"""

from __future__ import annotations

import re

CITATION_RE = re.compile(r"\[(\d+)\]")


def normalize(text: str) -> str:
    """Lowercase and drop all whitespace, so '10 月 23 日' matches '10月23日'."""
    return re.sub(r"\s+", "", text.lower())


def source_files(sources: list[dict]) -> list[str]:
    return [str(item.get("file_name", "")) for item in sources if isinstance(item, dict)]


def hit_at_k(sources: list[dict], expected_files: list[str]) -> bool:
    if not expected_files:
        return False
    names = {normalize(name) for name in source_files(sources)}
    return any(normalize(name) in names for name in expected_files)


def first_hit_rank(sources: list[dict], expected_files: list[str]) -> int:
    if not expected_files:
        return 0
    wanted = {normalize(name) for name in expected_files}
    for rank, name in enumerate(source_files(sources), start=1):
        if normalize(name) in wanted:
            return rank
    return 0


def keyword_hits(answer: str, keywords: list[str]) -> tuple[int, int]:
    if not keywords:
        return (0, 0)
    text = normalize(answer)
    hits = sum(1 for keyword in keywords if normalize(keyword) in text)
    return (hits, len(keywords))


def citation_valid(answer: str, source_count: int) -> bool:
    if not answer.strip() or source_count <= 0:
        return False
    for match in CITATION_RE.finditer(answer):
        if 1 <= int(match.group(1)) <= source_count:
            return True
    return False


def refused(answer: str, source_count: int) -> bool:
    """True when an unanswerable question is declined without fabricating sources."""
    if source_count != 0:
        return False
    text = normalize(answer)
    return bool(text) and ("无法回答" in text or "没有可检索" in text or "缺少" in text)


def percentile(values: list[float], p: float) -> float:
    if not values:
        return 0.0
    ordered = sorted(values)
    if len(ordered) == 1:
        return ordered[0]
    rank = (len(ordered) - 1) * p
    low = int(rank)
    high = min(low + 1, len(ordered) - 1)
    return ordered[low] + (ordered[high] - ordered[low]) * (rank - low)
