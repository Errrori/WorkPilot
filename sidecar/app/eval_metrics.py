"""Pure metric helpers for the offline RAG evaluation runner.

Kept dependency-free so the unit tests can run with plain Python.
"""

from __future__ import annotations

import math
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
    "无法确认",
    "无法判断",
    "无法提供",
    "未记录",
    "无记录",
)

# Flexible refusal wording such as "没有 R-06 的相关信息" or "未记录该字段".
REFUSAL_PATTERNS = (
    re.compile(r"没有[^。！？\n]{0,16}(?:信息|记录|提到|说明|数据|相关内容)"),
    re.compile(r"没有[^。！？\n]{0,16}(?:没有|未)"),
    re.compile(r"未(?:记录|找到|提供|提及|说明)"),
    re.compile(r"无法(?:回答|确认|判断|确定|提供)"),
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


def unique_ordered(values: list[str]) -> list[str]:
    seen: set[str] = set()
    out: list[str] = []
    for value in values:
        if value not in seen:
            seen.add(value)
            out.append(value)
    return out


def recall_at_k(retrieved: list[str], gold: list[str], k: int | None = None) -> float:
    """Fraction of unique gold items present in the first k retrieved items."""
    wanted = unique_ordered(gold)
    if not wanted:
        return 0.0
    if k is not None:
        retrieved = retrieved[:k]
    got = set(retrieved)
    return sum(1 for item in wanted if item in got) / len(wanted)


def all_required_at_k(retrieved: list[str], required: list[str], k: int | None = None) -> bool:
    """True only when every required item is present in the first k results."""
    wanted = unique_ordered(required)
    if not wanted:
        return False
    if k is not None:
        retrieved = retrieved[:k]
    got = set(retrieved)
    return all(item in got for item in wanted)


def ndcg_from_relevances(relevances: list[int]) -> float:
    """Binary nDCG from per-result relevance, capped at 1.

    Unlike ndcg_at_k, this treats every retrieved item as a distinct result and
    uses the number of relevant retrieved items as the ideal gain count. It is
    used for span-level ranking where multiple chunks may contain the same span.
    """
    if not relevances:
        return 0.0
    dcg = sum(
        1.0 / math.log2(rank + 1)
        for rank, relevant in enumerate(relevances, start=1)
        if relevant
    )
    relevant_count = sum(1 for relevant in relevances if relevant)
    if relevant_count == 0:
        return 0.0
    idcg = sum(
        1.0 / math.log2(rank + 1)
        for rank in range(1, min(relevant_count, len(relevances)) + 1)
    )
    return min(1.0, dcg / idcg) if idcg else 0.0


def ndcg_at_k(retrieved: list[str], gold: list[str], k: int | None = None) -> float:
    """Binary-gain nDCG@K. Returns 0 when there is no gold or no result."""
    wanted = set(unique_ordered(gold))
    if not wanted:
        return 0.0
    if k is not None:
        retrieved = retrieved[:k]
    if not retrieved:
        return 0.0
    dcg = sum(
        1.0 / math.log2(rank + 1)
        for rank, item in enumerate(retrieved, start=1)
        if item in wanted
    )
    ideal_hits = min(len(wanted), len(retrieved))
    idcg = sum(1.0 / math.log2(rank + 1) for rank in range(1, ideal_hits + 1))
    return dcg / idcg if idcg else 0.0


def wilson_interval(successes: int, total: int, z: float = 1.96) -> tuple[float, float]:
    """Wilson score interval for a binomial proportion, clipped to [0, 1]."""
    if total <= 0:
        return (0.0, 0.0)
    p = successes / total
    denominator = 1 + (z * z) / total
    center = (p + (z * z) / (2 * total)) / denominator
    margin = z * math.sqrt((p * (1 - p) + (z * z) / (4 * total)) / total) / denominator
    return (max(0.0, center - margin), min(1.0, center + margin))


def keyword_present(text: str, keyword: str) -> bool:
    """Check a normalized keyword in normalized text.

    Keywords containing digits need a token boundary so that '6' does not match
    '2026', '1 家' does not match '11 家', and '3 秒' does not match '13 秒'.
    The boundary is applied to the whole literal, which also protects values
    such as 'REQ-102' and '0.5%'.
    """
    normalized = normalize(keyword)
    if not normalized:
        return False
    if any(ch.isdigit() for ch in normalized):
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
    if any(normalize(marker) in text for marker in REFUSAL_MARKERS):
        return True
    return any(pattern.search(text) is not None for pattern in REFUSAL_PATTERNS)


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
