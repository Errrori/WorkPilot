"""Dependency-free retrieval baselines for offline evaluation."""

from __future__ import annotations

import math

from app.eval_metrics import normalize


def tokenize(text: str) -> list[str]:
    """Character bigrams work for Chinese without a word segmenter."""
    normalized = normalize(text)
    if not normalized:
        return []
    if len(normalized) == 1:
        return [normalized]
    return [normalized[index:index + 2] for index in range(len(normalized) - 1)]


class BM25Index:
    def __init__(self, docs: list[tuple[str, str]], k1: float = 1.5, b: float = 0.75):
        self.docs = docs
        self.k1 = k1
        self.b = b
        self.tokens = [tokenize(text) for _, text in docs]
        self.lengths = [len(tokens) for tokens in self.tokens]
        self.avg_length = (sum(self.lengths) / len(self.lengths)) if self.lengths else 0.0
        self.doc_freq: dict[str, int] = {}
        for tokens in self.tokens:
            for token in set(tokens):
                self.doc_freq[token] = self.doc_freq.get(token, 0) + 1

    def _idf(self, token: str) -> float:
        total = len(self.docs)
        freq = self.doc_freq.get(token, 0)
        return math.log(1 + (total - freq + 0.5) / (freq + 0.5))

    def search(self, query: str, top_k: int) -> list[str]:
        if not self.docs or top_k <= 0:
            return []
        query_tokens = set(tokenize(query))
        if not query_tokens:
            return []
        scored: list[tuple[float, int]] = []
        for index, tokens in enumerate(self.tokens):
            if not tokens:
                continue
            term_freq: dict[str, int] = {}
            for token in tokens:
                if token in query_tokens:
                    term_freq[token] = term_freq.get(token, 0) + 1
            if not term_freq:
                continue
            length = self.lengths[index] or 1
            score = 0.0
            for token, freq in term_freq.items():
                denominator = freq + self.k1 * (1 - self.b + self.b * length / (self.avg_length or 1))
                score += self._idf(token) * freq * (self.k1 + 1) / denominator
            scored.append((score, index))
        scored.sort(key=lambda item: (-item[0], item[1]))
        return [self.docs[index][0] for _, index in scored[:top_k]]
