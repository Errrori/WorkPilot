"""Pure metric helpers for the offline RAG evaluation runner.

Kept dependency-free so the unit tests can run with plain Python.
"""

from __future__ import annotations

import re

CITATION_RE = re.compile(r"\[(\d+)\]")

# Markers that indicate the model declined or stated the corpus does not cover
# the question. Used as the fallback when a case defines no refusal_keywords.
REFUSAL_MARKERS = (
    "无法回答",
    "没有可检索",
    "没有找到",
    "未找到",
    "缺少",
    "未包含",
    "不包含",
    "未提及",
    "没有提到",
    "未提供",
)


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


def keyword_present(text: str, keyword: str) -> bool:
    """Check a normalized keyword in normalized text.

    Digit-only keywords need a token boundary so that '6' does not match the
    '6' inside '2026' or '10月16日'.
    """
    normalized = normalize(keyword)
    if not normalized:
        return False
    if normalized.isdigit():
        return re.search(rf"(?<!\d){re.escape(normalized)}(?!\d)", text) is not None
    return normalized in text


def keyword_hits(answer: str, keywords: list[str]) -> tuple[int, int]:
    if not keywords:
        return (0, 0)
    text = normalize(answer)
    hits = sum(1 for keyword in keywords if keyword_present(text, keyword))
    return (hits, len(keywords))


def citation_valid(answer: str, source_count: int) -> bool:
    if not answer.strip() or source_count <= 0:
        return False
    for match in CITATION_RE.finditer(answer):
        if 1 <= int(match.group(1)) <= source_count:
            return True
    return False


def citation_files(answer: str, sources: list[dict]) -> list[str]:
    """Map in-range [n] citations to the file names of the referenced sources."""
    files: list[str] = []
    for match in CITATION_RE.finditer(answer):
        index = int(match.group(1))
        if 1 <= index <= len(sources):
            item = sources[index - 1]
            if isinstance(item, dict):
                files.append(str(item.get("file_name", "")))
    return files


def citation_hit(answer: str, sources: list[dict], expected_files: list[str]) -> bool:
    """True when at least one citation points at an expected source file."""
    if not expected_files:
        return False
    wanted = {normalize(name) for name in expected_files}
    return any(normalize(name) in wanted for name in citation_files(answer, sources))


def refused(answer: str, source_count: int, refusal_keywords: list[str] | None = None) -> bool:
    """True when the model declined instead of fabricating an answer.

    source_count is kept for call compatibility but no longer gates the result:
    scope-limited questions ("this release excludes X") may still retrieve a
    relevant chunk while the correct behaviour is to state the absence.
    """
    text = normalize(answer)
    if not text:
        return False
    if refusal_keywords:
        return all(normalize(keyword) in text for keyword in refusal_keywords)
    return any(normalize(marker) in text for marker in REFUSAL_MARKERS)


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
