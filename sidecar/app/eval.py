"""Offline evaluation runner for RAG QA.

Uploads the fixture corpus into a fresh group, waits for parse + index, then
runs every dataset question through `/search` (retrieval only) or `/ask`
(full QA) and writes a Markdown/JSON report.

Run from sidecar/ with the venv active:

    .venv/Scripts/python -m app.eval --base-url http://localhost:8080
    .venv/Scripts/python -m app.eval --split smoke --tag local --runs 3
    .venv/Scripts/python -m app.eval --dataset eval/datasets/bench-test.jsonl \
        --fixtures eval/corpus/bench --split benchmark --retrieval-only --k-sweep 1,3,5

The default dataset is the small smoke set. It validates the end-to-end path;
it is not a retrieval benchmark because the corpus is smaller than TopK.

Both `/ask` and `/search` receive explicit top_k/min_score overrides so the
report measures the parameters it records. BM25 uses the same top_k and no
score threshold.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import mimetypes
import os
import random
import re
import statistics
import subprocess
import time
from collections import Counter
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path

import httpx

from app import eval_baseline
from app import eval_metrics as metrics
from app import judge as judge_mod

try:
    from dotenv import load_dotenv

    load_dotenv(Path(__file__).resolve().parents[2] / ".env")
except ImportError:  # dotenv is optional for metric-only tests
    pass

EVAL_DIR = Path(__file__).resolve().parents[1] / "eval"
DEFAULT_DATASET = EVAL_DIR / "datasets" / "smoke.jsonl"
DEFAULT_FIXTURES = EVAL_DIR / "fixtures"
DEFAULT_REPORT_DIR = EVAL_DIR / "reports"
EVAL_VERSION = "3.1"
SCHEMA_VERSION = 3
SPLIT_LABELS = {"smoke": "链路冒烟", "benchmark": "检索基准"}
CLAIM_PRECISION_TARGET = 0.8
BOOTSTRAP_ROUNDS = 2000
BOOTSTRAP_SEED = 20261021

MIME_TYPES = {
    ".md": "text/markdown",
    ".txt": "text/plain",
    ".csv": "text/csv",
    ".pdf": "application/pdf",
    ".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
    ".doc": "application/msword",
    ".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
    ".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
    ".html": "text/html",
}

SCOPE_MARKERS = ("不包含", "不在本期", "未纳入", "不涉及", "未包含", "不在范围")
# A refusal may quote contextual numbers ("保留 24 小时"); only flag a concrete
# asserted answer or explicit speculation after the refusal.
ASSERTIVE_ANSWER_RE = re.compile(
    r"(?:是|为|在|于|预计|计划|将于|将在|完成于|上线于|定为)\s*\d"
)
SPECULATION_RE = re.compile(
    r"(?:据(?:经验|了解|推测)|一般情况下|一般来说|应该是|大概是|通常|估计|猜测)"
)


@dataclass
class Case:
    id: str
    question: str
    expected_files: list[str] = field(default_factory=list)
    required_files: list[str] = field(default_factory=list)
    acceptable_files: list[str] = field(default_factory=list)
    gold_spans: list[str] = field(default_factory=list)
    answer_keywords: list[str] = field(default_factory=list)
    expected_answers: list[str] = field(default_factory=list)
    reference_answer: str = ""
    answerable: bool = True
    refusal_keywords: list[str] = field(default_factory=list)
    refusal_type: str = ""
    refusal_subtype: str = ""
    reasoning_type: str = ""
    difficulty: str = ""
    split: str = ""
    tags: list[str] = field(default_factory=list)

    @property
    def required(self) -> list[str]:
        return self.required_files or self.expected_files

    @property
    def expected(self) -> list[str]:
        return self.expected_files or self.required_files

    @property
    def hit_files(self) -> list[str]:
        """Files that count as a retrieval hit, including alternative sources."""
        return metrics.unique_ordered(self.expected + self.acceptable_files)

    @property
    def evidence_files(self) -> list[str]:
        """Files that may legitimately contain a gold span."""
        return metrics.unique_ordered(self.expected + self.required + self.acceptable_files)


@dataclass
class CaseResult:
    case: Case
    sources: list[dict]
    answer: str
    latency_ms: int
    error: str = ""
    mode: str = "qa"
    top_k: int = 0

    @property
    def impossible(self) -> bool:
        """True when the run cannot satisfy all required files at this K."""
        return self.top_k > 0 and len(self.case.required) > self.top_k

    @property
    def hit(self) -> bool:
        return metrics.hit_at_k(self.sources, self.case.hit_files)

    @property
    def rank(self) -> int:
        return metrics.first_hit_rank(self.sources, self.case.hit_files)

    @property
    def recall(self) -> float:
        return metrics.recall_at_k(metrics.source_files(self.sources), self.case.required)

    @property
    def all_required(self) -> bool:
        return metrics.all_required_at_k(metrics.source_files(self.sources), self.case.required)

    @property
    def keyword_hits(self) -> tuple[int, int]:
        return metrics.keyword_hits(self.answer, self.case.answer_keywords)

    @property
    def keyword_ok(self) -> bool:
        hits, total = self.keyword_hits
        return total == 0 or hits == total

    @property
    def expected_answer_ok(self) -> bool:
        if not self.case.expected_answers:
            return True
        text = metrics.normalize(self.answer)
        return all(
            metrics.keyword_present(text, expected)
            for expected in self.case.expected_answers
        )

    @property
    def citation_ok(self) -> bool:
        return metrics.citation_valid(self.answer, len(self.sources))

    @property
    def citation_hit(self) -> bool:
        return metrics.citation_hit(self.answer, self.sources, self.case.hit_files)

    @property
    def retrieved_texts(self) -> list[str]:
        return [str(s.get("content") or s.get("snippet") or "") for s in self.sources]

    @property
    def expected_names(self) -> set[str]:
        """Normalized files that may count as evidence (required/expected/acceptable)."""
        return {metrics.normalize(name) for name in self.case.evidence_files if name}

    def _source_matches_expected(self, source: dict) -> bool:
        if not self.expected_names:
            return True
        return metrics.normalize(str(source.get("file_name", ""))) in self.expected_names

    @property
    def span_matches(self) -> list[bool]:
        """One boolean per gold span: found in a chunk from an expected file."""
        if not self.case.gold_spans:
            return []
        texts = [
            metrics.normalize(str(source.get("content") or source.get("snippet") or ""))
            for source in self.sources
            if self._source_matches_expected(source)
        ]
        return [
            any(metrics.normalize(span) in text for text in texts)
            for span in self.case.gold_spans
        ]

    @property
    def span_hit(self) -> bool:
        matches = self.span_matches
        return bool(matches) and any(matches)

    @property
    def span_recall(self) -> float:
        matches = self.span_matches
        return (sum(matches) / len(matches)) if matches else 0.0

    @property
    def all_spans(self) -> bool:
        matches = self.span_matches
        return bool(matches) and all(matches)

    @property
    def span_rank(self) -> int:
        wanted = [metrics.normalize(span) for span in self.case.gold_spans]
        if not wanted:
            return 0
        for rank, source in enumerate(self.sources, start=1):
            if not self._source_matches_expected(source):
                continue
            normalized = metrics.normalize(str(source.get("content") or source.get("snippet") or ""))
            if any(span in normalized for span in wanted):
                return rank
        return 0

    @property
    def span_ndcg(self) -> float:
        wanted = [metrics.normalize(span) for span in self.case.gold_spans]
        if not wanted:
            return 0.0
        relevances = []
        for source in self.sources:
            normalized = metrics.normalize(str(source.get("content") or source.get("snippet") or ""))
            relevant = self._source_matches_expected(source) and any(
                span in normalized for span in wanted
            )
            relevances.append(1 if relevant else 0)
        return metrics.ndcg_from_relevances(relevances)

    def _citation_supported_by_source(
        self, start: int, end: int, source_text: str, source_file: str = ""
    ) -> bool:
        # Any retrieved chunk may support a citation. Whether it belongs to an
        # expected file is tracked separately by citation_hit; restricting
        # support to expected files marks valid cross-file citations as wrong.
        source = metrics.normalize(source_text)
        if not source:
            return False
        if self.case.gold_spans:
            return any(metrics.normalize(span) in source for span in self.case.gold_spans)
        sentence = _sentence_at(self.answer, start, end)
        sentence_norm = metrics.normalize(sentence)
        candidates = [
            keyword for keyword in self.case.answer_keywords
            if metrics.normalize(keyword) in sentence_norm
        ]
        if not candidates:
            candidates = self.case.answer_keywords
        if not candidates:
            return True
        return any(metrics.normalize(keyword) in source for keyword in candidates)

    @property
    def citation_support(self) -> tuple[int, int]:
        """(supported, in-range citations) for claim-level citation support."""
        supported = 0
        total = 0
        for match in metrics.CITATION_RE.finditer(self.answer):
            index = int(match.group(1))
            if not (1 <= index <= len(self.sources)):
                continue
            total += 1
            source = self.sources[index - 1]
            source_text = str(source.get("content") or source.get("snippet") or "")
            if self._citation_supported_by_source(
                match.start(), match.end(), source_text, str(source.get("file_name", ""))
            ):
                supported += 1
        return supported, total

    @property
    def citation_precision(self) -> float:
        supported, total = self.citation_support
        return supported / total if total else 0.0

    @property
    def citation_supported(self) -> bool:
        supported, total = self.citation_support
        return total > 0 and supported > 0

    def _claim_terms(self, claim: str) -> list[str]:
        normalized = metrics.normalize(claim)
        terms = [
            metrics.normalize(keyword)
            for keyword in self.case.answer_keywords
            if metrics.normalize(keyword) in normalized
        ]
        terms += re.findall(r"\d{1,2}月\d{1,2}日", normalized)
        terms += re.findall(
            r"\d+(?:\.\d+)?(?:%|秒|分钟|小时|天|万元|元|条|个|家|毫秒)",
            normalized,
        )
        terms += re.findall(r"(?:req|r)-\d+", normalized)
        return list(dict.fromkeys(term for term in terms if term))

    def _claim_supported_by_source(self, claim: str, source_text: str, source_file: str) -> bool:
        content = metrics.normalize(source_text)
        terms = self._claim_terms(claim)
        if terms:
            return any(term in content for term in terms)
        if self.case.gold_spans:
            return any(metrics.normalize(span) in content for span in self.case.gold_spans)
        return bool(content)

    @property
    def claim_citation_support(self) -> tuple[int, int]:
        """(supported claims, claims that carry at least one citation)."""
        supported = 0
        total = 0
        for claim in split_claims(self.answer):
            indexes = [
                int(match.group(1)) for match in metrics.CITATION_RE.finditer(claim)
            ]
            if not indexes:
                continue
            total += 1
            for index in dict.fromkeys(indexes):
                if not (1 <= index <= len(self.sources)):
                    continue
                source = self.sources[index - 1]
                if self._claim_supported_by_source(
                    claim,
                    str(source.get("content") or source.get("snippet") or ""),
                    str(source.get("file_name", "")),
                ):
                    supported += 1
                    break
        return supported, total

    @property
    def claim_citation_precision(self) -> float:
        supported, total = self.claim_citation_support
        return supported / total if total else 0.0

    @property
    def all_claims_supported(self) -> bool:
        supported, total = self.claim_citation_support
        return total > 0 and supported == total

    @property
    def refused(self) -> bool:
        return metrics.refused(self.answer, len(self.sources), self.case.refusal_keywords)

    @property
    def refused_clean(self) -> bool:
        """A refusal must decline and not add a concrete fabricated value."""
        if not self.refused:
            return False
        if self.case.refusal_keywords:
            return True
        text = metrics.normalize(self.answer)
        return (
            ASSERTIVE_ANSWER_RE.search(text) is None
            and SPECULATION_RE.search(text) is None
        )

    @property
    def scope_handled(self) -> bool:
        text = metrics.normalize(self.answer)
        return any(metrics.normalize(marker) in text for marker in SCOPE_MARKERS)

    @property
    def handled(self) -> bool:
        if self.case.answerable:
            return self.passed
        if self.case.refusal_type == "out_of_scope":
            return self.scope_handled or self.refused
        return self.refused_clean

    @property
    def retrieval_ok(self) -> bool:
        if self.error or self.impossible or not self.case.answerable:
            return False
        if not (self.hit and self.all_required):
            return False
        if self.case.gold_spans:
            return self.span_hit and self.all_spans
        return True

    @property
    def answer_ok(self) -> bool:
        if self.case.expected_answers:
            return self.expected_answer_ok
        return self.keyword_ok

    @property
    def citation_sufficient(self) -> bool:
        if not (self.citation_ok and self.citation_hit and self.citation_supported):
            return False
        return self.claim_citation_precision >= CLAIM_PRECISION_TARGET

    @property
    def passed(self) -> bool:
        if self.error or self.impossible:
            return False
        if self.mode == "retrieval":
            return self.retrieval_ok
        if not self.case.answerable:
            return self.handled
        return self.retrieval_ok and self.answer_ok and self.citation_sufficient

    def _evidence_in_unexpected_source(self) -> bool:
        evidence = self.case.gold_spans or self.case.answer_keywords
        if not evidence:
            return False
        expected = self.expected_names
        for source in self.sources:
            name = metrics.normalize(str(source.get("file_name", "")))
            if name in expected:
                continue
            text = metrics.normalize(str(source.get("content") or source.get("snippet") or ""))
            if any(metrics.normalize(item) in text for item in evidence):
                return True
        return False

    def _retrieval_miss_reason(self) -> str:
        if not self.sources:
            return "retrieval_empty"
        expected_projects = {_project_name(name) for name in self.case.hit_files if name}
        got_projects = {_project_name(str(source.get("file_name", ""))) for source in self.sources}
        if expected_projects and got_projects.isdisjoint(expected_projects):
            expected_types = {_doc_type(name) for name in self.case.hit_files if name}
            got_types = {_doc_type(str(source.get("file_name", ""))) for source in self.sources}
            if expected_types & got_types:
                return "retrieval_wrong_project"
            return "retrieval_wrong_doc"
        if expected_projects - got_projects:
            return "retrieval_missing_project"
        if self._evidence_in_unexpected_source():
            return "retrieval_ambiguous_label"
        if self.top_k > 0 and len(self.sources) < self.top_k:
            return "retrieval_below_min_score"
        return "retrieval_wrong_doc"

    @property
    def failure_reason(self) -> str:
        if self.error:
            return "error"
        if self.impossible:
            return "impossible_k_lt_required"
        if self.mode == "retrieval":
            if not self.hit:
                return self._retrieval_miss_reason()
            if not self.all_required:
                return "retrieval_missing_required"
            if self.case.gold_spans and not self.span_hit:
                return "retrieval_span_miss"
            if self.case.gold_spans and not self.all_spans:
                return "retrieval_partial_spans"
            return ""
        if not self.case.answerable:
            if self.handled:
                return ""
            return "scope_miss" if self.case.refusal_type == "out_of_scope" else "refusal_miss"
        if not self.hit:
            return self._retrieval_miss_reason()
        if not self.all_required:
            return "retrieval_missing_required"
        if self.case.gold_spans and not self.span_hit:
            return "retrieval_span_miss"
        if self.case.gold_spans and not self.all_spans:
            return "retrieval_partial_spans"
        if not self.answer_ok:
            return "answer_expected" if self.case.expected_answers else "answer_keyword"
        if not self.citation_ok:
            return "citation_format"
        if not self.citation_hit:
            return "citation_file"
        if not self.citation_supported:
            return "citation_support"
        if self.claim_citation_precision < CLAIM_PRECISION_TARGET:
            return "citation_claim"
        return ""


def _project_name(file_name: str) -> str:
    return metrics.normalize(file_name).split("-", 1)[0]


def _doc_type(file_name: str) -> str:
    stem = Path(metrics.normalize(file_name)).stem
    return stem.rsplit("-", 1)[-1]


CLAIM_SPLIT_RE = re.compile(r"(?<=[。！？\n])")


def split_claims(text: str) -> list[str]:
    return [part.strip() for part in CLAIM_SPLIT_RE.split(text) if part.strip()]


def _sentence_at(text: str, start: int, end: int) -> str:
    left = max(text.rfind(ch, 0, start) for ch in "。！？\n") + 1
    rights = [pos for ch in "。！？\n" if (pos := text.find(ch, end)) != -1]
    right = min(rights) if rights else len(text)
    return text[left:right]


def load_dataset(path: Path) -> list[Case]:
    cases: list[Case] = []
    for line_no, line in enumerate(path.read_text(encoding="utf-8").splitlines(), start=1):
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        try:
            raw = json.loads(line)
        except json.JSONDecodeError as exc:
            raise SystemExit(f"dataset {path}:{line_no}: invalid json: {exc}") from exc
        question = str(raw.get("question") or "").strip()
        if not question:
            raise SystemExit(f"dataset {path}:{line_no}: question is required")
        expected_files = [str(x) for x in raw.get("expected_files") or []]
        answerable = bool(raw.get("answerable", True))
        cases.append(Case(
            id=str(raw.get("id") or f"case-{line_no}"),
            question=question,
            expected_files=expected_files,
            required_files=[str(x) for x in raw.get("required_files") or []] or expected_files,
            acceptable_files=[str(x) for x in raw.get("acceptable_files") or []],
            gold_spans=[str(x) for x in raw.get("gold_spans") or []],
            answer_keywords=[str(x) for x in raw.get("answer_keywords") or []],
            expected_answers=(
                [str(x) for x in raw.get("expected_answers") or []]
                or ([str(raw["expected_answer"])] if raw.get("expected_answer") else [])
            ),
            reference_answer=str(raw.get("reference_answer") or ""),
            answerable=answerable,
            refusal_keywords=[str(x) for x in raw.get("refusal_keywords") or []],
            refusal_type=str(raw.get("refusal_type") or ("none" if answerable else "not_in_corpus")),
            refusal_subtype=str(raw.get("refusal_subtype") or ""),
            reasoning_type=str(raw.get("reasoning_type") or ""),
            difficulty=str(raw.get("difficulty") or ""),
            split=str(raw.get("split") or ""),
            tags=[str(x) for x in raw.get("tags") or []],
        ))
    if not cases:
        raise SystemExit(f"dataset {path} is empty")
    validate_cases(cases, path)
    return cases


def validate_cases(cases: list[Case], path: Path) -> None:
    seen: set[str] = set()
    for case in cases:
        if case.id in seen:
            raise SystemExit(f"dataset {path}: duplicate case id {case.id}")
        seen.add(case.id)
        if not case.answerable and case.refusal_type == "none":
            case.refusal_type = "not_in_corpus"


TEXT_SUFFIXES = {".md", ".txt", ".csv", ".html", ".htm"}


def fixture_texts(fixtures_dir: Path) -> dict[str, str]:
    texts: dict[str, str] = {}
    for path in sorted(fixtures_dir.glob("*")):
        if path.is_file() and path.suffix.lower() in TEXT_SUFFIXES:
            texts[path.name] = path.read_text(encoding="utf-8", errors="replace")
    return texts


def validate_gold_spans(cases: list[Case], fixtures_dir: Path) -> None:
    """Fail fast when a gold span drifts out of the corpus.

    Binary fixtures (PDF/DOCX) are checked by the parser at run time; their raw
    bytes are not searchable here.
    """
    texts = fixture_texts(fixtures_dir)
    missing: list[str] = []
    for case in cases:
        if not case.gold_spans:
            continue
        # Mixed text/binary cases need the parser output, which is not available
        # before uploading; skip them here and rely on the run-time check.
        if any(name not in texts for name in case.evidence_files):
            continue
        normalized = "\n".join(metrics.normalize(texts[name]) for name in case.evidence_files)
        for span in case.gold_spans:
            if metrics.normalize(span) not in normalized:
                missing.append(f"{case.id}: {span[:48]}")
    if missing:
        raise SystemExit(
            "gold spans not found in fixtures: " + "; ".join(missing[:8])
        )


def infer_split(dataset: Path, requested: str | None) -> str:
    if requested:
        return requested
    name = dataset.name.lower()
    if "bench" in name:
        return "benchmark"
    return "smoke"


def hash_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as fh:
        for block in iter(lambda: fh.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def hash_dir(path: Path) -> str:
    digest = hashlib.sha256()
    for item in sorted(p for p in path.glob("*") if p.is_file()):
        digest.update(item.name.encode("utf-8"))
        digest.update(b"\0")
        digest.update(item.read_bytes())
        digest.update(b"\0")
    return digest.hexdigest()


def _git(*args: str) -> str:
    repo = Path(__file__).resolve().parents[2]
    try:
        result = subprocess.run(
            ["git", *args],
            cwd=repo,
            capture_output=True,
            text=True,
            timeout=10,
            check=False,
        )
    except (OSError, subprocess.SubprocessError):
        return ""
    return result.stdout.strip() if result.returncode == 0 else ""


def git_info() -> dict:
    return {
        "commit": _git("rev-parse", "HEAD"),
        "branch": _git("rev-parse", "--abbrev-ref", "HEAD"),
        "dirty": bool(_git("status", "--porcelain")),
    }


def _env_int(key: str, fallback: int) -> int:
    try:
        return int(os.environ.get(key, ""))
    except ValueError:
        return fallback


def _env_float(key: str, fallback: float) -> float:
    try:
        return float(os.environ.get(key, ""))
    except ValueError:
        return fallback


def config_snapshot() -> dict:
    """Record the eval client's view of config without exposing secrets."""
    return {
        "source": "process/.env on the eval host; the server may use different values",
        "llm": {
            "provider": os.environ.get("LLM_PROVIDER", "openai"),
            "model": os.environ.get("LLM_MODEL", "qwen2.5:7b"),
            "base_url": os.environ.get("LLM_BASE_URL", "http://localhost:11434/v1"),
            "timeout_seconds": _env_int("LLM_TIMEOUT_SECONDS", 120),
        },
        "embedding": {
            "provider": os.environ.get("EMBEDDING_PROVIDER", "ollama"),
            "model": os.environ.get("EMBEDDING_MODEL", "bge-m3"),
            "dim": _env_int("EMBEDDING_DIM", 1024),
        },
        "chunk": {
            "size": _env_int("CHUNK_SIZE", 800),
            "overlap": _env_int("CHUNK_OVERLAP", 100),
        },
        "retrieval": {
            "mode": os.environ.get("RETRIEVAL_MODE", "hybrid"),
            "top_k": _env_int("RETRIEVAL_TOP_K", 6),
            "candidates": _env_int("RETRIEVAL_CANDIDATES", 30),
            "min_score": _env_float("RETRIEVAL_MIN_SCORE", 0.42),
            "hybrid": {
                "rrf_k": _env_float("HYBRID_RRF_K", 60),
                "dense_weight": _env_float("HYBRID_DENSE_WEIGHT", 1),
                "sparse_weight": _env_float("HYBRID_SPARSE_WEIGHT", 1),
            },
        },
    }


def validate_referenced_files(cases: list[Case], fixture_names: list[str]) -> None:
    known = set(fixture_names)
    missing: set[str] = set()
    for case in cases:
        missing.update(name for name in case.evidence_files if name not in known)
    if missing:
        raise SystemExit("dataset references files not uploaded: " + ", ".join(sorted(missing)))


def create_group(client: httpx.Client, name: str) -> str:
    resp = client.post("/api/groups", json={"name": name})
    resp.raise_for_status()
    return resp.json()["group"]["id"]


def ensure_group(client: httpx.Client, group_id: str) -> None:
    resp = client.get("/api/groups")
    resp.raise_for_status()
    groups = resp.json().get("groups") or []
    if not any(g.get("id") == group_id for g in groups):
        raise SystemExit(f"group {group_id} not found")


def upload_fixtures(client: httpx.Client, group_id: str, fixtures_dir: Path, user: str) -> list[str]:
    names: list[str] = []
    for path in sorted(fixtures_dir.glob("*")):
        if not path.is_file():
            continue
        content_type = MIME_TYPES.get(path.suffix.lower())
        if content_type is None:
            content_type = mimetypes.guess_type(path.name)[0] or "application/octet-stream"
        with path.open("rb") as fh:
            resp = client.post(
                f"/api/groups/{group_id}/files",
                params={"user": user},
                files={"file": (path.name, fh, content_type)},
            )
        if resp.status_code != 201:
            raise SystemExit(f"upload {path.name} failed: HTTP {resp.status_code}: {resp.text[:300]}")
        names.append(path.name)
    if not names:
        raise SystemExit(f"no fixtures found in {fixtures_dir}")
    return names


def wait_until_indexed(client: httpx.Client, group_id: str, names: list[str], timeout_s: float) -> int:
    deadline = time.monotonic() + timeout_s
    while True:
        resp = client.get(f"/api/groups/{group_id}/files")
        resp.raise_for_status()
        by_name = {f["file_name"]: f for f in resp.json().get("files") or []}

        failed: list[str] = []
        pending: list[str] = []
        for name in names:
            record = by_name.get(name)
            if record is None:
                pending.append(name)
                continue
            if record.get("parse_status") in ("failed", "unsupported") or record.get("index_status") == "failed":
                reason = record.get("parse_error") or record.get("index_error") or ""
                failed.append(f"{name} (parse={record.get('parse_status')}, index={record.get('index_status')}) {reason}".strip())
            elif record.get("index_status") != "indexed":
                pending.append(name)
        if failed:
            raise SystemExit("fixtures failed to parse/index: " + "; ".join(failed))
        if not pending:
            return sum(int(by_name.get(name, {}).get("chunk_count") or 0) for name in names)
        if time.monotonic() > deadline:
            raise SystemExit("timed out waiting for indexing: " + ", ".join(pending))
        time.sleep(2)


def fetch_chunks(client: httpx.Client, group_id: str, names: list[str]) -> tuple[dict[str, str], list[dict]]:
    """Load full chunk texts so span/citation checks do not depend on snippets."""
    resp = client.get(f"/api/groups/{group_id}/files")
    resp.raise_for_status()
    records = {item["file_name"]: item for item in resp.json().get("files") or []}
    texts: dict[str, str] = {}
    chunks: list[dict] = []
    for name in names:
        record = records.get(name)
        if not record or not record.get("id") or not record.get("chunk_count"):
            continue
        chunk_resp = client.get(f"/api/groups/{group_id}/files/{record['id']}/chunks")
        if chunk_resp.status_code != 200:
            continue
        for chunk in chunk_resp.json().get("chunks") or []:
            index = chunk.get("chunk_index")
            content = str(chunk.get("content") or "")
            key = f"{record['id']}:{index}"
            texts[key] = content
            chunks.append({
                "key": key,
                "file_id": record["id"],
                "file_name": name,
                "chunk_index": index,
                "content": content,
            })
    return texts, chunks


def enrich_sources(sources: list[dict], chunk_texts: dict[str, str]) -> list[dict]:
    enriched: list[dict] = []
    for source in sources:
        item = dict(source)
        key = f"{source.get('file_id')}:{source.get('chunk_index')}"
        item["content"] = chunk_texts.get(key) or source.get("snippet") or ""
        enriched.append(item)
    return enriched


def search(
    client: httpx.Client, group_id: str, question: str, top_k: int, min_score: float | None = None
) -> list[dict]:
    body = {"query": question, "top_k": top_k}
    if min_score is not None:
        body["min_score"] = min_score
    resp = client.post(f"/api/groups/{group_id}/search", json=body)
    resp.raise_for_status()
    return resp.json().get("sources") or []


def ask(
    client: httpx.Client,
    group_id: str,
    user: str,
    question: str,
    timeout_s: float,
    top_k: int,
    min_score: float | None = None,
) -> tuple[list[dict], str]:
    sources: list[dict] = []
    chunks: list[str] = []
    error: str | None = None
    body = {"user": user, "question": question, "top_k": top_k}
    if min_score is not None:
        body["min_score"] = min_score
    with client.stream(
        "POST",
        f"/api/groups/{group_id}/ask",
        json=body,
        timeout=httpx.Timeout(timeout_s, connect=10.0),
    ) as resp:
        if resp.status_code != 200:
            body = resp.read().decode("utf-8", "replace")
            raise RuntimeError(f"ask failed: HTTP {resp.status_code}: {body[:300]}")
        event: str | None = None
        for line in resp.iter_lines():
            if not line:
                continue
            if line.startswith("event:"):
                event = line[len("event:"):].strip()
                continue
            if not line.startswith("data:"):
                continue
            payload = json.loads(line[len("data:"):].strip() or "{}")
            if event == "sources":
                sources = payload.get("sources") or []
            elif event == "delta":
                chunks.append(payload.get("text") or "")
            elif event == "error":
                error = payload.get("error") or "unknown error"
            event = None
    if error:
        raise RuntimeError(error)
    return sources, "".join(chunks)


def fetch_usage(client: httpx.Client, group_id: str, since: datetime, until: datetime) -> dict:
    resp = client.get(
        f"/api/groups/{group_id}/usage",
        params={"from": since.isoformat(), "to": until.isoformat()},
    )
    resp.raise_for_status()
    return resp.json()


def run(
    client: httpx.Client,
    group_id: str,
    user: str,
    cases: list[Case],
    mode: str,
    top_k: int,
    ask_timeout: float,
    min_score: float | None = None,
    chunk_texts: dict[str, str] | None = None,
) -> list[CaseResult]:
    results: list[CaseResult] = []
    for index, case in enumerate(cases, start=1):
        print(f"[{index}/{len(cases)}] {case.id} {case.question[:32]}…", flush=True)
        start = time.perf_counter()
        try:
            if mode == "retrieval":
                sources = search(client, group_id, case.question, top_k, min_score)
                answer = ""
            else:
                sources, answer = ask(
                    client, group_id, user, case.question, ask_timeout, top_k, min_score
                )
            sources = enrich_sources(sources, chunk_texts or {})
            latency_ms = int((time.perf_counter() - start) * 1000)
            results.append(CaseResult(
                case=case, sources=sources, answer=answer, latency_ms=latency_ms,
                mode=mode, top_k=top_k,
            ))
        except Exception as exc:  # keep going, the report shows per-case failures
            latency_ms = int((time.perf_counter() - start) * 1000)
            results.append(CaseResult(
                case=case, sources=[], answer="", latency_ms=latency_ms,
                error=str(exc), mode=mode, top_k=top_k,
            ))
    return results


def bm25_baseline(
    cases: list[Case], chunk_records: list[dict], top_k: int
) -> tuple[dict, list[CaseResult]] | None:
    if not chunk_records:
        return None
    index = eval_baseline.BM25Index([
        (record["key"], record["content"]) for record in chunk_records
    ])
    by_key = {record["key"]: record for record in chunk_records}
    results: list[CaseResult] = []
    for case in cases:
        keys = index.search(case.question, top_k)
        sources = [
            {
                "file_name": by_key[key]["file_name"],
                "chunk_index": by_key[key]["chunk_index"],
                "content": by_key[key]["content"],
                "score": 0.0,
            }
            for key in keys
        ]
        results.append(CaseResult(
            case=case, sources=sources, answer="", latency_ms=0, mode="retrieval", top_k=top_k
        ))
    return summarize(results, "retrieval"), results


def run_judge(
    client: httpx.Client,
    results: list[CaseResult],
    config,
    limit: int,
    report_dir: Path,
) -> tuple[list[dict], dict, Path]:
    selected = [r for r in results if r.case.answerable and not r.error and not r.impossible]
    if limit > 0:
        selected = selected[:limit]
    rows: list[dict] = []
    for result in selected:
        context = "\n\n".join(
            f"[{index + 1}] {source.get('file_name', '')}：{str(source.get('content') or source.get('snippet') or '')[:1500]}"
            for index, source in enumerate(result.sources)
        )
        reference = (
            result.case.reference_answer
            or "；".join(result.case.gold_spans)
            or "；".join(result.case.answer_keywords)
        )
        try:
            row = judge_mod.judge_answer(
                client, config, result.case.question, reference, context, result.answer
            )
            row.update({"id": result.case.id, "question": result.case.question, "answer": result.answer, "error": ""})
        except Exception as exc:
            row = {
                "id": result.case.id,
                "question": result.case.question,
                "answer": result.answer,
                "error": str(exc)[:300],
            }
        rows.append(row)
    summary = judge_mod.summarize(rows)
    review_path = report_dir / "judge-review.csv"
    judge_mod.write_review(review_path, rows)
    return rows, summary, review_path


def paired_bootstrap(
    rag_values: list[float],
    baseline_values: list[float],
    rounds: int = BOOTSTRAP_ROUNDS,
    seed: int = BOOTSTRAP_SEED,
) -> dict:
    """Bootstrap the paired difference (RAG - baseline) over cases."""
    count = min(len(rag_values), len(baseline_values))
    if count == 0:
        return {}
    rag = rag_values[:count]
    baseline = baseline_values[:count]
    diffs = [left - right for left, right in zip(rag, baseline)]
    rng = random.Random(seed)
    samples: list[float] = []
    for _ in range(rounds):
        picks = [rng.randrange(count) for _ in range(count)]
        samples.append(statistics.fmean(diffs[index] for index in picks))
    samples.sort()
    return {
        "n": count,
        "rag_mean": statistics.fmean(rag),
        "baseline_mean": statistics.fmean(baseline),
        "mean_diff": statistics.fmean(diffs),
        "ci95": [
            round(metrics.percentile(samples, 0.025), 6),
            round(metrics.percentile(samples, 0.975), 6),
        ],
        "wins": sum(1 for diff in diffs if diff > 0),
        "losses": sum(1 for diff in diffs if diff < 0),
        "ties": sum(1 for diff in diffs if diff == 0),
    }


def compare_retrieval(rag_results: list[CaseResult], baseline_results: list[CaseResult]) -> dict:
    baseline_by_id = {result.case.id: result for result in baseline_results}
    pairs = [
        (result, baseline_by_id[result.case.id])
        for result in rag_results
        if result.case.id in baseline_by_id and not result.impossible
        and result.case.answerable
    ]
    if not pairs:
        return {}
    comparison = {"n": len(pairs)}
    comparison["file_hit"] = paired_bootstrap(
        [1.0 if rag.hit else 0.0 for rag, _ in pairs],
        [1.0 if base.hit else 0.0 for _, base in pairs],
    )
    comparison["all_required"] = paired_bootstrap(
        [1.0 if rag.all_required else 0.0 for rag, _ in pairs],
        [1.0 if base.all_required else 0.0 for _, base in pairs],
    )
    span_pairs = [(rag, base) for rag, base in pairs if rag.case.gold_spans]
    if span_pairs:
        comparison["span_hit"] = paired_bootstrap(
            [1.0 if rag.span_hit else 0.0 for rag, _ in span_pairs],
            [1.0 if base.span_hit else 0.0 for _, base in span_pairs],
        )
        comparison["span_ndcg"] = paired_bootstrap(
            [rag.span_ndcg for rag, _ in span_pairs],
            [base.span_ndcg for _, base in span_pairs],
        )
    return comparison


def corpus_oracle(cases: list[Case], chunk_records: list[dict]) -> dict:
    """Corpus coverage upper bound: can the labels be satisfied at all?

    A perfect reranker can at best reach these values at this corpus; lower
    values mean the corpus or labels are broken, not that retrieval failed.
    """
    by_file: dict[str, list[str]] = {}
    for record in chunk_records:
        by_file.setdefault(record["file_name"], []).append(record["content"])

    rows: dict[str, dict] = {}
    required_indexed = 0
    file_hits = 0
    span_cases = 0
    span_recall_total = 0.0
    all_spans_ok = 0
    answerable = [case for case in cases if case.answerable]
    for case in answerable:
        required = case.required
        hit_files = case.hit_files
        texts = [
            content
            for name in case.evidence_files
            for content in by_file.get(name, [])
        ]
        normalized = [metrics.normalize(text) for text in texts]
        spans = [
            any(metrics.normalize(span) in text for text in normalized)
            for span in case.gold_spans
        ]
        files_ok = {name: bool(by_file.get(name)) for name in required}
        hit_ok = {name: bool(by_file.get(name)) for name in hit_files}
        row = {
            "file_hit": any(hit_ok.values()),
            "all_required": all(files_ok.values()) if files_ok else False,
            "span_matches": spans,
            "span_recall": (sum(spans) / len(spans)) if spans else None,
            "all_spans": all(spans) if spans else None,
        }
        rows[case.id] = row
        file_hits += 1 if row["file_hit"] else 0
        required_indexed += 1 if row["all_required"] else 0
        if spans:
            span_cases += 1
            span_recall_total += row["span_recall"]
            all_spans_ok += 1 if row["all_spans"] else 0

    total = len(answerable)
    summary = {
        "cases": total,
        "file_hit_rate": (file_hits / total) if total else 0.0,
        "all_required_rate": (required_indexed / total) if total else 0.0,
        "span_cases": span_cases,
        "span_recall_rate": (span_recall_total / span_cases) if span_cases else 0.0,
        "all_spans_rate": (all_spans_ok / span_cases) if span_cases else 0.0,
        "note": "perfect-rerank upper bound over the indexed corpus; low values indicate corpus/label gaps",
    }
    return {"summary": summary, "by_id": rows}


def _ci(successes: int, total: int) -> list[float]:
    return [round(value, 6) for value in metrics.wilson_interval(successes, total)]


def load_report_cases(path: Path) -> tuple[dict[str, dict], int]:
    """Load per-case metrics from a previous report JSON for A/B comparison."""
    data = json.loads(path.read_text(encoding="utf-8"))
    top_k = int((data.get("meta") or {}).get("top_k") or 0)
    cases = {
        str(case.get("id")): case
        for case in (data.get("cases") or [])
        if isinstance(case, dict) and case.get("id")
    }
    return cases, top_k


def compare_saved_report(
    results: list[CaseResult], baseline_cases: dict[str, dict], baseline_top_k: int
) -> dict:
    """Pair the current run with a saved report and bootstrap the metric deltas."""
    pairs = []
    for result in results:
        baseline = baseline_cases.get(result.case.id)
        if not baseline or not result.case.answerable or result.impossible:
            continue
        if baseline_top_k and len(result.case.required) > baseline_top_k:
            continue
        pairs.append((result, baseline))
    if not pairs:
        return {}

    comparison = {
        "n": len(pairs),
        "current_top_k": results[0].top_k if results else 0,
        "baseline_top_k": baseline_top_k,
    }
    comparison["file_hit"] = paired_bootstrap(
        [1.0 if rag.hit else 0.0 for rag, _ in pairs],
        [1.0 if base.get("hit") else 0.0 for _, base in pairs],
    )
    comparison["all_required"] = paired_bootstrap(
        [1.0 if rag.all_required else 0.0 for rag, _ in pairs],
        [1.0 if base.get("all_required") else 0.0 for _, base in pairs],
    )
    span_pairs = [(rag, base) for rag, base in pairs if rag.case.gold_spans]
    if span_pairs:
        comparison["span_hit"] = paired_bootstrap(
            [1.0 if rag.span_hit else 0.0 for rag, _ in span_pairs],
            [1.0 if base.get("span_hit") else 0.0 for _, base in span_pairs],
        )
        comparison["span_recall"] = paired_bootstrap(
            [rag.span_recall for rag, _ in span_pairs],
            [float(base.get("span_recall") or 0.0) for _, base in span_pairs],
        )
        comparison["span_ndcg"] = paired_bootstrap(
            [rag.span_ndcg for rag, _ in span_pairs],
            [float(base.get("span_ndcg") or 0.0) for _, base in span_pairs],
        )
    return comparison


def summarize(results: list[CaseResult], mode: str, include_by_tag: bool = True) -> dict:
    impossible = [r for r in results if r.impossible]
    evaluable = [r for r in results if not r.impossible]
    answerable = [r for r in evaluable if r.case.answerable]
    unanswerable = [r for r in evaluable if not r.case.answerable]
    scorable = answerable if mode == "retrieval" else evaluable
    latencies = [r.latency_ms for r in results]

    hits = sum(1 for r in answerable if r.hit)
    ranks = [r.rank for r in answerable if r.rank > 0]
    recalls = [r.recall for r in answerable if r.case.required]
    all_required = sum(1 for r in answerable if r.all_required)

    span_cases = [r for r in answerable if r.case.gold_spans]
    span_hits = sum(1 for r in span_cases if r.span_hit)
    span_recalls = [r.span_recall for r in span_cases]
    all_spans = sum(1 for r in span_cases if r.all_spans)
    span_ranks = [r.span_rank for r in span_cases if r.span_rank > 0]
    span_ndcg = (sum(r.span_ndcg for r in span_cases) / len(span_cases)) if span_cases else 0.0
    span_mrr = (sum(1 / rank for rank in span_ranks) / len(span_cases)) if span_cases else 0.0

    keyword_hits = sum(r.keyword_hits[0] for r in answerable)
    keyword_total = sum(r.keyword_hits[1] for r in answerable)
    expected_cases = [r for r in answerable if r.case.expected_answers]
    expected_ok = sum(1 for r in expected_cases if r.expected_answer_ok)
    citations = sum(1 for r in answerable if r.citation_ok)
    citation_hits = sum(1 for r in answerable if r.citation_hit)
    citation_pairs = [r.citation_support for r in answerable]
    citation_supported = sum(pair[0] for pair in citation_pairs)
    citation_total = sum(pair[1] for pair in citation_pairs)
    citation_supported_cases = sum(1 for r in answerable if r.citation_supported)

    claim_pairs = [r.claim_citation_support for r in answerable]
    claim_total = sum(pair[1] for pair in claim_pairs)
    claim_supported = sum(pair[0] for pair in claim_pairs)
    claim_cases = sum(1 for pair in claim_pairs if pair[1] > 0)
    claim_fully_supported = sum(1 for r in answerable if r.all_claims_supported)

    refusal_cases = [r for r in unanswerable if r.case.refusal_type != "out_of_scope"]
    scope_cases = [r for r in unanswerable if r.case.refusal_type == "out_of_scope"]
    refusals = sum(1 for r in refusal_cases if r.refused_clean)
    scope_ok = sum(1 for r in scope_cases if r.handled)
    handled = sum(1 for r in unanswerable if r.handled)
    false_refusals = sum(1 for r in answerable if r.refused and not r.answer_ok)
    refusal_tp = handled
    refusal_fp = false_refusals
    refusal_fn = len(unanswerable) - handled
    passed = sum(1 for r in scorable if r.passed)
    retrieval_passed = sum(1 for r in answerable if r.retrieval_ok)
    answer_passed = sum(1 for r in answerable if r.answer_ok)
    citation_passed = sum(1 for r in answerable if r.citation_sufficient)

    summary = {
        "cases": len(results),
        "evaluable": len(evaluable),
        "impossible_cases": len(impossible),
        "impossible_ids": [r.case.id for r in impossible],
        "answerable": len(answerable),
        "unanswerable": len(unanswerable),
        "scorable": len(scorable),
        "errors": sum(1 for r in results if r.error),
        "hit_rate": (hits / len(answerable)) if answerable else 0.0,
        "hit_ci95": _ci(hits, len(answerable)),
        "hits": hits,
        "mrr": (sum(1 / r for r in ranks) / len(answerable)) if answerable else 0.0,
        "recall_rate": (sum(recalls) / len(answerable)) if answerable else 0.0,
        "all_required_rate": (all_required / len(answerable)) if answerable else 0.0,
        "all_required_ci95": _ci(all_required, len(answerable)),
        "all_required_ok": all_required,
        "span_cases": len(span_cases),
        "span_hit_rate": (span_hits / len(span_cases)) if span_cases else 0.0,
        "span_hit_ci95": _ci(span_hits, len(span_cases)),
        "span_hits": span_hits,
        "span_recall_rate": (sum(span_recalls) / len(span_cases)) if span_cases else 0.0,
        "all_spans_rate": (all_spans / len(span_cases)) if span_cases else 0.0,
        "all_spans_ci95": _ci(all_spans, len(span_cases)),
        "all_spans_ok": all_spans,
        "ndcg": span_ndcg,
        "span_mrr": span_mrr,
        "keyword_rate": (keyword_hits / keyword_total) if keyword_total else 0.0,
        "keyword_ci95": _ci(keyword_hits, keyword_total),
        "keyword_hits": keyword_hits,
        "keyword_total": keyword_total,
        "expected_cases": len(expected_cases),
        "expected_answer_rate": (expected_ok / len(expected_cases)) if expected_cases else 0.0,
        "expected_answer_ci95": _ci(expected_ok, len(expected_cases)),
        "expected_answer_ok": expected_ok,
        "citation_rate": (citations / len(answerable)) if answerable else 0.0,
        "citation_ci95": _ci(citations, len(answerable)),
        "citation_ok": citations,
        "citation_hit_rate": (citation_hits / len(answerable)) if answerable else 0.0,
        "citation_hit_ci95": _ci(citation_hits, len(answerable)),
        "citation_hit_ok": citation_hits,
        "citation_support_rate": (citation_supported_cases / len(answerable)) if answerable else 0.0,
        "citation_support_ci95": _ci(citation_supported_cases, len(answerable)),
        "citation_supported_cases": citation_supported_cases,
        "citation_precision": (citation_supported / citation_total) if citation_total else 0.0,
        "citation_precision_ci95": _ci(citation_supported, citation_total),
        "citation_supported": citation_supported,
        "citation_total": citation_total,
        "claim_cases": claim_cases,
        "claim_total": claim_total,
        "claim_supported": claim_supported,
        "claim_precision": (claim_supported / claim_total) if claim_total else 0.0,
        "claim_precision_ci95": _ci(claim_supported, claim_total),
        "claim_fully_supported_cases": claim_fully_supported,
        "claim_fully_supported_rate": (claim_fully_supported / claim_cases) if claim_cases else 0.0,
        "claim_fully_supported_ci95": _ci(claim_fully_supported, claim_cases),
        "refusal_cases": len(refusal_cases),
        "refusal_rate": (refusals / len(refusal_cases)) if refusal_cases else 0.0,
        "refusal_ci95": _ci(refusals, len(refusal_cases)),
        "refusal_ok": refusals,
        "scope_cases": len(scope_cases),
        "scope_rate": (scope_ok / len(scope_cases)) if scope_cases else 0.0,
        "scope_ci95": _ci(scope_ok, len(scope_cases)),
        "scope_ok": scope_ok,
        "handled_rate": (handled / len(unanswerable)) if unanswerable else 0.0,
        "handled_ci95": _ci(handled, len(unanswerable)),
        "handled_ok": handled,
        "false_refusal_rate": (false_refusals / len(answerable)) if answerable else 0.0,
        "false_refusal_ci95": _ci(false_refusals, len(answerable)),
        "false_refusal": false_refusals,
        "refusal_precision": (refusal_tp / (refusal_tp + refusal_fp)) if (refusal_tp + refusal_fp) else 0.0,
        "refusal_precision_ci95": _ci(refusal_tp, refusal_tp + refusal_fp),
        "refusal_recall": (refusal_tp / (refusal_tp + refusal_fn)) if (refusal_tp + refusal_fn) else 0.0,
        "refusal_recall_ci95": _ci(refusal_tp, refusal_tp + refusal_fn),
        "retrieval_passed": retrieval_passed,
        "retrieval_pass_rate": (retrieval_passed / len(answerable)) if answerable else 0.0,
        "answer_passed": answer_passed,
        "answer_pass_rate": (answer_passed / len(answerable)) if answerable else 0.0,
        "citation_passed": citation_passed,
        "citation_pass_rate": (citation_passed / len(answerable)) if answerable else 0.0,
        "passed": passed,
        "pass_rate": (passed / len(scorable)) if scorable else 0.0,
        "pass_ci95": _ci(passed, len(scorable)),
        "failures": dict(Counter(r.failure_reason for r in scorable if r.failure_reason)),
        "latency_p50_ms": int(metrics.percentile(latencies, 0.5)),
        "latency_p95_ms": int(metrics.percentile(latencies, 0.95)),
    }
    if mode == "retrieval":
        for key in (
            "keyword_rate", "keyword_ci95", "keyword_hits", "keyword_total",
            "expected_cases", "expected_answer_rate", "expected_answer_ci95", "expected_answer_ok",
            "citation_rate", "citation_ci95", "citation_ok",
            "citation_hit_rate", "citation_hit_ci95", "citation_hit_ok",
            "citation_support_rate", "citation_support_ci95", "citation_supported_cases",
            "citation_precision", "citation_precision_ci95", "citation_supported", "citation_total",
            "claim_cases", "claim_total", "claim_supported", "claim_precision", "claim_precision_ci95",
            "claim_fully_supported_cases", "claim_fully_supported_rate", "claim_fully_supported_ci95",
            "refusal_cases", "refusal_rate", "refusal_ci95", "refusal_ok",
            "scope_cases", "scope_rate", "scope_ci95", "scope_ok",
            "handled_rate", "handled_ci95", "handled_ok",
            "false_refusal_rate", "false_refusal_ci95", "false_refusal",
            "refusal_precision", "refusal_precision_ci95",
            "refusal_recall", "refusal_recall_ci95",
            "retrieval_passed", "retrieval_pass_rate",
            "answer_passed", "answer_pass_rate",
            "citation_passed", "citation_pass_rate",
        ):
            summary.pop(key, None)
    if include_by_tag:
        tag_results = answerable if mode == "retrieval" else evaluable
        tags = sorted({tag for r in tag_results for tag in r.case.tags if tag})
        summary["by_tag"] = {
            tag: summarize([r for r in tag_results if tag in r.case.tags], mode, include_by_tag=False)
            for tag in tags
        }
        reasoning_types = sorted({
            r.case.reasoning_type for r in tag_results if r.case.reasoning_type
        })
        if reasoning_types:
            summary["by_reasoning"] = {
                reasoning_type: summarize(
                    [r for r in tag_results if r.case.reasoning_type == reasoning_type],
                    mode,
                    include_by_tag=False,
                )
                for reasoning_type in reasoning_types
            }
    return summary


STABILITY_KEYS = (
    "hit_rate", "mrr", "recall_rate", "all_required_rate",
    "span_hit_rate", "span_recall_rate", "all_spans_rate", "ndcg", "span_mrr",
    "keyword_rate", "expected_answer_rate", "citation_rate", "citation_hit_rate",
    "citation_support_rate", "citation_precision",
    "claim_precision", "claim_fully_supported_rate",
    "refusal_rate", "scope_rate", "handled_rate", "false_refusal_rate",
    "refusal_precision", "refusal_recall",
    "retrieval_pass_rate", "answer_pass_rate", "citation_pass_rate",
    "pass_rate", "latency_p50_ms", "latency_p95_ms",
)


def aggregate_stability(summaries: list[dict]) -> dict:
    out: dict[str, dict] = {}
    for key in STABILITY_KEYS:
        values = [float(s[key]) for s in summaries if key in s]
        if not values:
            continue
        out[key] = {
            "mean": statistics.fmean(values),
            "std": statistics.pstdev(values) if len(values) > 1 else 0.0,
            "min": min(values),
            "max": max(values),
            "values": values,
        }
    return out


STABILITY_LABELS = {
    "hit_rate": "文件 Hit@K",
    "mrr": "文件 MRR",
    "recall_rate": "文件 Recall@K",
    "all_required_rate": "文件 AllRequired",
    "span_hit_rate": "Span Hit@K",
    "span_recall_rate": "Span Recall@K",
    "all_spans_rate": "Span AllRequired",
    "ndcg": "Span nDCG@K",
    "span_mrr": "Span MRR",
    "keyword_rate": "关键词",
    "expected_answer_rate": "精确答案",
    "citation_rate": "引用格式",
    "citation_hit_rate": "引用命中文件",
    "citation_support_rate": "引用支持",
    "citation_precision": "引用精确率",
    "claim_precision": "Claim 引用支持",
    "claim_fully_supported_rate": "Claim 全部有据",
    "refusal_rate": "拒答",
    "scope_rate": "范围否定",
    "handled_rate": "不可答处理",
    "false_refusal_rate": "误拒答",
    "refusal_precision": "拒答精确率",
    "refusal_recall": "拒答召回率",
    "retrieval_pass_rate": "检索通过率",
    "answer_pass_rate": "答案通过率",
    "citation_pass_rate": "引用通过率",
    "pass_rate": "用例通过率",
    "latency_p50_ms": "P50 延迟",
    "latency_p95_ms": "P95 延迟",
}


def _ci_text(ci: list[float] | None) -> str:
    if not ci:
        return ""
    return f"，95% CI {ci[0]:.1%}–{ci[1]:.1%}"


def _stability_text(stability: dict | None, key: str, percent: bool = True) -> str:
    if not stability or key not in stability:
        return ""
    item = stability[key]
    if percent:
        return f" | 均值 {item['mean']:.1%} ± {item['std']:.1%}（{item['min']:.1%}–{item['max']:.1%}）"
    return f" | 均值 {item['mean']:.3f} ± {item['std']:.3f}（{item['min']:.3f}–{item['max']:.3f}）"


def print_report(
    summary: dict,
    results: list[CaseResult],
    usage: dict | None,
    mode: str,
    group_id: str,
    dataset: Path,
    split: str,
    run_label: str,
    run_summaries: list[dict],
    stability: dict | None,
    corpus_chunks: int,
    retrieval_ceiling: bool,
    top_k: int = 0,
    min_score: float | None = None,
    oracle: dict | None = None,
    comparison: dict | None = None,
    baselines: dict[str, dict] | None = None,
    k_sweep: dict[str, dict] | None = None,
    judge_summary: dict | None = None,
) -> None:
    print("\n==== WorkPilot 离线评测 ====")
    print(f"评测类型: {SPLIT_LABELS.get(split, split)}")
    if run_label:
        print(f"运行标签: {run_label}")
    print(f"群组    : {group_id}")
    print(f"数据集  : {dataset}")
    print(f"语料    : {corpus_chunks} chunks")
    print(f"检索参数: top_k={top_k}，min_score={'disabled' if min_score == 0 else min_score}")
    if retrieval_ceiling:
        print(f"警告    : 语料 chunk 数 <= top_k，Hit@K/MRR 不能作为检索质量结论")
    print(f"模式    : {'仅检索' if mode == 'retrieval' else '完整问答'}")
    print(f"轮次    : {len(run_summaries)}")
    print(f"用例    : {summary['cases']}（可答 {summary['answerable']} / 不可答 {summary['unanswerable']}）")
    if summary.get("impossible_cases"):
        print(
            f"警告    : {summary['impossible_cases']} 个用例的 required_files 多于 top_k，"
            "已标记 impossible 且不计入通过率"
        )
    print("\n指标：")
    print(
        f"  Hit@K 命中率      : {summary['hits']}/{summary['answerable']} "
        f"({summary['hit_rate']:.1%}{_ci_text(summary['hit_ci95'])})"
        f"{_stability_text(stability, 'hit_rate')}"
    )
    print(f"  MRR               : {summary['mrr']:.3f}{_stability_text(stability, 'mrr', percent=False)}")
    print(
        f"  Recall@K          : {summary['recall_rate']:.1%}"
        f"{_stability_text(stability, 'recall_rate')}"
    )
    print(
        f"  AllRequired       : {summary['all_required_ok']}/{summary['answerable']} "
        f"({summary['all_required_rate']:.1%}{_ci_text(summary['all_required_ci95'])})"
        f"{_stability_text(stability, 'all_required_rate')}"
    )
    if summary["span_cases"]:
        print(
            f"  Span Hit@K        : {summary['span_hits']}/{summary['span_cases']} "
            f"({summary['span_hit_rate']:.1%}{_ci_text(summary['span_hit_ci95'])})"
            f"{_stability_text(stability, 'span_hit_rate')}"
        )
        print(
            f"  Span Recall@K     : {summary['span_recall_rate']:.1%}"
            f"{_stability_text(stability, 'span_recall_rate')}"
        )
        print(
            f"  Span AllRequired  : {summary['all_spans_ok']}/{summary['span_cases']} "
            f"({summary['all_spans_rate']:.1%}{_ci_text(summary['all_spans_ci95'])})"
            f"{_stability_text(stability, 'all_spans_rate')}"
        )
        print(
            f"  Span nDCG / MRR   : {summary['ndcg']:.3f} / {summary['span_mrr']:.3f}"
            f"{_stability_text(stability, 'ndcg', percent=False)}"
        )
    if mode != "retrieval":
        print(
            f"  关键词命中率      : {summary['keyword_hits']}/{summary['keyword_total']} "
            f"({summary['keyword_rate']:.1%}{_ci_text(summary['keyword_ci95'])})"
            f"{_stability_text(stability, 'keyword_rate')}"
        )
        if summary["expected_cases"]:
            print(
                f"  精确答案匹配      : {summary['expected_answer_ok']}/{summary['expected_cases']} "
                f"({summary['expected_answer_rate']:.1%}{_ci_text(summary['expected_answer_ci95'])})"
                f"{_stability_text(stability, 'expected_answer_rate')}"
            )
        print(
            f"  引用格式有效性    : {summary['citation_ok']}/{summary['answerable']} "
            f"({summary['citation_rate']:.1%}{_ci_text(summary['citation_ci95'])})"
            f"{_stability_text(stability, 'citation_rate')}"
        )
        print(
            f"  引用命中期望文件  : {summary['citation_hit_ok']}/{summary['answerable']} "
            f"({summary['citation_hit_rate']:.1%}{_ci_text(summary['citation_hit_ci95'])})"
            f"{_stability_text(stability, 'citation_hit_rate')}"
        )
        print(
            f"  引用支持率/精确率 : {summary['citation_supported_cases']}/{summary['answerable']} "
            f"({summary['citation_support_rate']:.1%}) / {summary['citation_precision']:.1%}"
            f"{_stability_text(stability, 'citation_precision')}"
        )
        print(
            f"  Claim 引用支持    : {summary['claim_supported']}/{summary['claim_total']} "
            f"({summary['claim_precision']:.1%}); 全部有据 "
            f"{summary['claim_fully_supported_cases']}/{summary['claim_cases']} "
            f"({summary['claim_fully_supported_rate']:.1%})；严格阈值 {CLAIM_PRECISION_TARGET:.0%}"
        )
        print(
            f"  分项通过          : 检索 {summary['retrieval_passed']}/{summary['answerable']} "
            f"({summary['retrieval_pass_rate']:.1%})；答案 {summary['answer_passed']}/{summary['answerable']} "
            f"({summary['answer_pass_rate']:.1%})；引用 {summary['citation_passed']}/{summary['answerable']} "
            f"({summary['citation_pass_rate']:.1%})"
        )
        print(
            f"  拒答正确率        : {summary['refusal_ok']}/{summary['refusal_cases']} "
            f"({summary['refusal_rate']:.1%}{_ci_text(summary['refusal_ci95'])})"
            f"{_stability_text(stability, 'refusal_rate')}"
        )
        print(
            f"  范围否定处理      : {summary['scope_ok']}/{summary['scope_cases']} "
            f"({summary['scope_rate']:.1%})；不可答处理 {summary['handled_ok']}/{summary['unanswerable']}"
            f" ({summary['handled_rate']:.1%})"
        )
        print(
            f"  误拒答 / 拒答精确率: {summary['false_refusal']}/{summary['answerable']} "
            f"({summary['false_refusal_rate']:.1%}) / {summary['refusal_precision']:.1%}"
            f"；拒答召回率 {summary['refusal_recall']:.1%}"
            f"{_stability_text(stability, 'false_refusal_rate')}"
        )
    print(f"  延迟 P50 / P95    : {summary['latency_p50_ms']} / {summary['latency_p95_ms']} ms")
    pass_label = "可答用例通过" if mode == "retrieval" else "用例通过"
    print(
        f"  {pass_label:<12}: {summary['passed']}/{summary['scorable']} "
        f"({summary['pass_rate']:.1%}{_ci_text(summary['pass_ci95'])})"
        f"{_stability_text(stability, 'pass_rate')}"
    )
    if usage:
        s = usage.get("summary") or {}
        currency = (usage.get("pricing") or {}).get("currency") or ""
        print(f"  LLM 调用          : {s.get('calls', 0)} 次（失败 {s.get('failed', 0)}），"
              f"tokens {s.get('total_tokens', 0)}，成本 {s.get('cost', 0)} {currency}")

    failures = summary.get("failures") or {}
    if failures:
        print("\n失败分类：")
        for reason, count in sorted(failures.items(), key=lambda item: (-item[1], item[0])):
            print(f"  {reason:<28} {count}")

    if oracle and oracle.get("summary"):
        print("\nOracle（语料/标签覆盖上界）：")
        item = oracle["summary"]
        print(
            f"  可答 {item['cases']} 题：FileHit={item['file_hit_rate']:.1%} "
            f"AllRequired={item['all_required_rate']:.1%} "
            f"SpanRecall={item['span_recall_rate']:.1%} SpanAllRequired={item['all_spans_rate']:.1%}"
        )

    if baselines:
        print("\n检索 baseline（同 K，BM25 不使用 min_score）：")
        for name, item in baselines.items():
            print(
                f"  {name:<10} FileHit={item['hit_rate']:.1%} FileMRR={item['mrr']:.3f} "
                f"SpanHit={item['span_hit_rate']:.1%} SpanRecall={item['span_recall_rate']:.1%} "
                f"nDCG={item['ndcg']:.3f} SpanMRR={item['span_mrr']:.3f}"
            )

    if comparison:
        print(f"\nRAG - BM25（配对 bootstrap 95% CI，n={comparison['n']}）：")
        for key, label in (
            ("file_hit", "FileHit"), ("all_required", "AllRequired"),
            ("span_hit", "SpanHit"), ("span_ndcg", "Span nDCG"),
        ):
            item = comparison.get(key)
            if not item:
                continue
            if key == "span_ndcg":
                print(
                    f"  {label:<12} {item['mean_diff']:+.3f} "
                    f"[{item['ci95'][0]:+.3f}, {item['ci95'][1]:+.3f}] "
                    f"（赢/输/平 {item['wins']}/{item['losses']}/{item['ties']}）"
                )
            else:
                print(
                    f"  {label:<12} {item['mean_diff']:+.1%} "
                    f"[{item['ci95'][0]:+.1%}, {item['ci95'][1]:+.1%}] "
                    f"（赢/输/平 {item['wins']}/{item['losses']}/{item['ties']}）"
                )

    if k_sweep:
        print("\nK sweep（retrieval-only）：")
        for k, item in k_sweep.items():
            print(
                f"  K={k:<2} FileHit={item['hit_rate']:.1%} SpanHit={item['span_hit_rate']:.1%} "
                f"SpanRecall={item['span_recall_rate']:.1%} nDCG={item['ndcg']:.3f} "
                f"SpanMRR={item['span_mrr']:.3f}"
            )

    if judge_summary:
        print(
            f"\nLLM judge：{judge_summary.get('pass', 0)}/{judge_summary.get('scored', 0)} pass，"
            f"correctness={judge_summary.get('correctness', 0):.2f}，"
            f"faithfulness={judge_summary.get('faithfulness', 0):.2f}，"
            f"completeness={judge_summary.get('completeness', 0):.2f}，"
            f"citation_support={judge_summary.get('citation_support', 0):.2f}"
        )
        agreement = judge_summary.get("human_agreement")
        if agreement:
            print(
                f"judge/人工一致率：{agreement['agreement']:.1%}（n={agreement['n']}，"
                f"kappa={agreement['kappa']:.3f}）"
            )

    tags = summary.get("by_tag") or {}
    if tags:
        print("\n按标签：")
        for tag, item in tags.items():
            print(
                f"  {tag:<10} cases={item['cases']:<3} hit={item['hit_rate']:.1%} "
                f"pass={item['pass_rate']:.1%}"
            )

    reasoning = summary.get("by_reasoning") or {}
    if reasoning:
        print("\n按推理类型：")
        for rtype, item in reasoning.items():
            print(
                f"  {rtype:<20} cases={item['cases']:<3} "
                f"span_hit={item['span_hit_rate']:.1%} pass={item['pass_rate']:.1%}"
            )

    print("\n明细：")
    for r in results:
        skipped = mode == "retrieval" and not r.case.answerable
        if r.error:
            status = f"调用失败：{r.error[:60]}"
        elif skipped:
            status = "不参与"
        elif not r.case.answerable:
            label = "范围" if r.case.refusal_type == "out_of_scope" else "拒答"
            status = f"通过（{label}）" if r.passed else "未通过"
        else:
            status = "通过" if r.passed else "未通过"
        keywords = f"关键词 {r.keyword_hits[0]}/{r.keyword_hits[1]}" if mode != "retrieval" else ""
        hit = "-" if skipped else str(int(r.hit))
        rank = "-" if skipped else str(r.rank)
        print(f"  {r.case.id:<4} {status:<12} hit={hit} rank={rank} {keywords:<16} {r.latency_ms:>5} ms  {r.case.question[:28]}")


def _md_stability_text(stability: dict | None, key: str, percent: bool = True) -> str:
    if not stability or key not in stability:
        return ""
    item = stability[key]
    if percent:
        return f"；{len(item['values'])} 轮均值 {item['mean']:.1%} ± {item['std']:.1%}"
    return f"；{len(item['values'])} 轮均值 {item['mean']:.3f} ± {item['std']:.3f}"


def write_reports(
    report_dir: Path,
    summary: dict,
    results: list[CaseResult],
    usage: dict | None,
    meta: dict,
    run_summaries: list[dict],
    stability: dict | None,
    case_pass_counts: dict[str, list[bool]],
    baselines: dict[str, dict] | None = None,
    k_sweep: dict[str, dict] | None = None,
    judge_rows: list[dict] | None = None,
    judge_summary: dict | None = None,
    judge_review_path: Path | None = None,
    oracle: dict | None = None,
    comparison: dict | None = None,
) -> tuple[Path, Path]:
    report_dir.mkdir(parents=True, exist_ok=True)
    stamp = datetime.now().strftime("%Y%m%d-%H%M%S")
    json_path = report_dir / f"eval-{stamp}.json"
    md_path = report_dir / f"eval-{stamp}.md"

    payload = {
        "schema_version": SCHEMA_VERSION,
        "meta": meta,
        "summary": summary,
        "stability": stability,
        "baselines": baselines or {},
        "baseline_comparison": comparison or {},
        "oracle": oracle or {},
        "k_sweep": k_sweep or {},
        "judge": {
            "summary": judge_summary,
            "review_path": str(judge_review_path) if judge_review_path else "",
            "cases": judge_rows or [],
        },
        "runs": [
            {"index": index + 1, "summary": item}
            for index, item in enumerate(run_summaries)
        ],
        "usage": usage,
        "cases": [
            {
                "id": r.case.id,
                "question": r.case.question,
                "tags": r.case.tags,
                "answerable": r.case.answerable,
                "expected_files": r.case.expected,
                "required_files": r.case.required,
                "acceptable_files": r.case.acceptable_files,
                "hit_files": r.case.hit_files,
                "gold_spans": r.case.gold_spans,
                "reference_answer": r.case.reference_answer,
                "answer_keywords": r.case.answer_keywords,
                "expected_answers": r.case.expected_answers,
                "expected_answer_ok": r.expected_answer_ok,
                "refusal_keywords": r.case.refusal_keywords,
                "refusal_type": r.case.refusal_type,
                "refusal_subtype": r.case.refusal_subtype,
                "reasoning_type": r.case.reasoning_type,
                "difficulty": r.case.difficulty,
                "split": r.case.split,
                "sources": [
                    {
                        "file_name": s.get("file_name"),
                        "chunk_index": s.get("chunk_index"),
                        "score": s.get("score"),
                        "content": s.get("content") or s.get("snippet") or "",
                    }
                    for s in r.sources
                ],
                "oracle": (oracle or {}).get("by_id", {}).get(r.case.id, {}),
                "answer": r.answer,
                "latency_ms": r.latency_ms,
                "impossible": r.impossible,
                "hit": r.hit,
                "rank": r.rank,
                "recall": r.recall,
                "all_required": r.all_required,
                "span_matches": r.span_matches,
                "span_hit": r.span_hit,
                "span_recall": r.span_recall,
                "all_spans": r.all_spans,
                "span_rank": r.span_rank,
                "span_ndcg": r.span_ndcg,
                "keyword_hits": list(r.keyword_hits),
                "citation_ok": r.citation_ok,
                "citation_hit": r.citation_hit,
                "citation_support": list(r.citation_support),
                "citation_precision": r.citation_precision,
                "claim_citation_support": list(r.claim_citation_support),
                "claim_citation_precision": r.claim_citation_precision,
                "all_claims_supported": r.all_claims_supported,
                "retrieval_ok": r.retrieval_ok,
                "answer_ok": r.answer_ok,
                "citation_sufficient": r.citation_sufficient,
                "refused": r.refused,
                "refused_clean": r.refused_clean,
                "scope_handled": r.scope_handled,
                "handled": r.handled,
                "passed": r.passed,
                "failure_reason": r.failure_reason,
                "pass_runs": sum(case_pass_counts.get(r.case.id, [])),
                "run_count": len(case_pass_counts.get(r.case.id, [])),
                "error": r.error,
            }
            for r in results
        ],
    }
    json_path.write_text(json.dumps(payload, ensure_ascii=False, indent=2), encoding="utf-8")

    lines = [
        "# WorkPilot 离线评测报告",
        "",
        f"- 时间：{meta['finished_at']}",
        f"- 评测类型：{SPLIT_LABELS.get(meta['split'], meta['split'])}",
        f"- 运行标签：{meta.get('run_label') or '-'}",
        f"- 轮次：{meta['runs']}",
        f"- 群组：`{meta['group_id']}`",
        f"- 数据集：`{meta['dataset']}`",
        f"- 语料：{meta['fixtures_count']} 文件 / {meta['corpus_chunks']} chunks",
        f"- 检索参数：top_k={meta.get('top_k')}，min_score={'disabled' if meta.get('min_score') == 0 else meta.get('min_score')}（请求覆盖；服务端配置快照见 JSON `meta.config`）",
        f"- 检索上限：{'是（语料 chunk 数 <= top_k，Hit@K/MRR 不能作为检索质量结论）' if meta['retrieval_ceiling'] else '否'}",
        f"- 模式：{'仅检索' if meta['mode'] == 'retrieval' else '完整问答'}",
        f"- 用例：{summary['cases']}（可答 {summary['answerable']} / 不可答 {summary['unanswerable']} / impossible {summary.get('impossible_cases', 0)}）",
        "",
        "## 指标",
        "",
        f"- 文件级 Hit@K：{summary['hits']}/{summary['answerable']}（{summary['hit_rate']:.1%}{_ci_text(summary['hit_ci95'])}）{_md_stability_text(stability, 'hit_rate')}",
        f"- 文件级 MRR：{summary['mrr']:.3f}{_md_stability_text(stability, 'mrr', percent=False)}",
        f"- 文件级 Recall@K：{summary['recall_rate']:.1%}{_md_stability_text(stability, 'recall_rate')}",
        f"- 文件级 AllRequired：{summary['all_required_ok']}/{summary['answerable']}（{summary['all_required_rate']:.1%}{_ci_text(summary['all_required_ci95'])}）{_md_stability_text(stability, 'all_required_rate')}",
    ]
    if summary["span_cases"]:
        lines += [
            f"- Span Hit@K：{summary['span_hits']}/{summary['span_cases']}（{summary['span_hit_rate']:.1%}{_ci_text(summary['span_hit_ci95'])}）{_md_stability_text(stability, 'span_hit_rate')}",
            f"- Span Recall@K：{summary['span_recall_rate']:.1%}{_md_stability_text(stability, 'span_recall_rate')}",
            f"- Span AllRequired：{summary['all_spans_ok']}/{summary['span_cases']}（{summary['all_spans_rate']:.1%}{_ci_text(summary['all_spans_ci95'])}）{_md_stability_text(stability, 'all_spans_rate')}",
            f"- Span nDCG@K / MRR：{summary['ndcg']:.3f} / {summary['span_mrr']:.3f}{_md_stability_text(stability, 'ndcg', percent=False)}",
        ]
    if meta["mode"] != "retrieval":
        lines += [
            f"- 关键词命中率：{summary['keyword_hits']}/{summary['keyword_total']}（{summary['keyword_rate']:.1%}{_ci_text(summary['keyword_ci95'])}）{_md_stability_text(stability, 'keyword_rate')}",
            f"- 精确答案匹配：{summary['expected_answer_ok']}/{summary['expected_cases']}（{summary['expected_answer_rate']:.1%}{_ci_text(summary['expected_answer_ci95'])}）{_md_stability_text(stability, 'expected_answer_rate')}",
            f"- 引用格式有效性：{summary['citation_ok']}/{summary['answerable']}（{summary['citation_rate']:.1%}{_ci_text(summary['citation_ci95'])}）{_md_stability_text(stability, 'citation_rate')}",
            f"- 引用命中期望文件：{summary['citation_hit_ok']}/{summary['answerable']}（{summary['citation_hit_rate']:.1%}{_ci_text(summary['citation_hit_ci95'])}）{_md_stability_text(stability, 'citation_hit_rate')}",
            f"- 引用支持率：{summary['citation_supported_cases']}/{summary['answerable']}（{summary['citation_support_rate']:.1%}）；引用精确率：{summary['citation_precision']:.1%}",
            f"- Claim 引用支持：{summary['claim_supported']}/{summary['claim_total']}（{summary['claim_precision']:.1%}）；全部有据：{summary['claim_fully_supported_cases']}/{summary['claim_cases']}（{summary['claim_fully_supported_rate']:.1%}）",
            f"- 分项通过：检索 {summary['retrieval_passed']}/{summary['answerable']}（{summary['retrieval_pass_rate']:.1%}）；答案 {summary['answer_passed']}/{summary['answerable']}（{summary['answer_pass_rate']:.1%}）；引用 {summary['citation_passed']}/{summary['answerable']}（{summary['citation_pass_rate']:.1%}）",
            f"- 拒答正确率：{summary['refusal_ok']}/{summary['refusal_cases']}（{summary['refusal_rate']:.1%}{_ci_text(summary['refusal_ci95'])}）{_md_stability_text(stability, 'refusal_rate')}",
            f"- 范围否定处理：{summary['scope_ok']}/{summary['scope_cases']}（{summary['scope_rate']:.1%}）；不可答处理：{summary['handled_ok']}/{summary['unanswerable']}（{summary['handled_rate']:.1%}）",
            f"- 误拒答：{summary['false_refusal']}/{summary['answerable']}（{summary['false_refusal_rate']:.1%}）；拒答精确率/召回率：{summary['refusal_precision']:.1%} / {summary['refusal_recall']:.1%}",
        ]
    pass_label = "可答用例通过" if meta["mode"] == "retrieval" else "用例通过"
    lines += [
        f"- 延迟 P50 / P95：{summary['latency_p50_ms']} / {summary['latency_p95_ms']} ms",
        f"- {pass_label}：{summary['passed']}/{summary['scorable']}（{summary['pass_rate']:.1%}{_ci_text(summary['pass_ci95'])}）{_md_stability_text(stability, 'pass_rate')}",
    ]
    if usage:
        s = usage.get("summary") or {}
        currency = (usage.get("pricing") or {}).get("currency") or ""
        lines.append(f"- LLM 调用：{s.get('calls', 0)} 次（失败 {s.get('failed', 0)}），tokens {s.get('total_tokens', 0)}，成本 {s.get('cost', 0)} {currency}")

    lines += [
        "",
        "## 评测口径与局限",
        "",
        f"- 评测类型：{SPLIT_LABELS.get(meta['split'], meta['split'])}；模式：{'仅检索' if meta['mode'] == 'retrieval' else '完整问答'}。",
        f"- 语料：{meta['fixtures_count']} 文件 / {meta['corpus_chunks']} chunks，top_k={meta['top_k']}，min_score={'disabled' if meta.get('min_score') == 0 else meta.get('min_score')}（请求覆盖）。",
        "- 配置快照来自评测机 env，服务端实际配置可能不同；本报告以请求里显式传入的 top_k/min_score 为准。",
    ]
    if meta.get("retrieval_ceiling"):
        lines.append("- 注意：语料 chunk 数 <= top_k，检索返回全库，Hit@K/MRR 不能作为检索质量结论。")
    lines += [
        "- 本报告只代表当前数据集、语料、模型和参数；benchmark 结果应与人工抽检和错误分析一起看。",
        "- 关键词/引用支持/拒答为 deterministic 近似指标，不等同于人工语义判断；需要时开启 `--judge` 并抽检 `judge-review.csv`。",
    ]
    if meta.get("judge"):
        lines.append(f"- LLM judge：{meta['judge'].get('model', '')}，prompt {meta['judge'].get('prompt_version', '')}；judge 分数本身不是 ground truth。")

    if stability:
        lines += [
            "",
            "## 多轮稳定性",
            "",
            "| 指标 | 均值 | 标准差 | 最小 | 最大 |",
            "|---|---|---|---|---|",
        ]
        for key, label in STABILITY_LABELS.items():
            if key not in stability:
                continue
            item = stability[key]
            if key in ("mrr", "latency_p50_ms", "latency_p95_ms"):
                lines.append(
                    f"| {label} | {item['mean']:.3f} | {item['std']:.3f} | {item['min']:.3f} | {item['max']:.3f} |"
                )
            else:
                lines.append(
                    f"| {label} | {item['mean']:.1%} | {item['std']:.1%} | {item['min']:.1%} | {item['max']:.1%} |"
                )

    failures = summary.get("failures") or {}
    if failures:
        lines += [
            "",
            "## 失败分类",
            "",
            "| 原因 | 数量 |",
            "|---|---|",
        ]
        for reason, count in sorted(failures.items(), key=lambda item: (-item[1], item[0])):
            lines.append(f"| {reason} | {count} |")

    if oracle and oracle.get("summary"):
        item = oracle["summary"]
        lines += [
            "",
            "## Oracle（语料/标签覆盖上界）",
            "",
            "完美 reranker 在该语料上能达到的上界；低于 100% 说明语料或标签有缺口。",
            "",
            f"- 可答用例：{item['cases']}",
            f"- File Hit@K / AllRequired：{item['file_hit_rate']:.1%} / {item['all_required_rate']:.1%}",
            f"- Span Recall@K / AllRequired：{item['span_recall_rate']:.1%} / {item['all_spans_rate']:.1%}",
        ]

    if baselines:
        lines += [
            "",
            "## 检索 baseline",
            "",
            "| baseline | File Hit@K | File MRR | Span Hit@K | Span Recall@K | Span AllRequired | nDCG@K | Span MRR |",
            "|---|---|---|---|---|---|---|---|",
        ]
        for name, item in baselines.items():
            lines.append(
                f"| {name} | {item['hit_rate']:.1%} | {item['mrr']:.3f} | {item['span_hit_rate']:.1%} | "
                f"{item['span_recall_rate']:.1%} | {item['all_spans_rate']:.1%} | "
                f"{item['ndcg']:.3f} | {item['span_mrr']:.3f} |"
            )

    if comparison:
        lines += [
            "",
            f"## RAG - BM25（配对 bootstrap 95% CI，n={comparison.get('n', 0)}）",
            "",
            "| 指标 | 均值差 | 95% CI | 赢/输/平 |",
            "|---|---|---|---|",
        ]
        for key, label in (
            ("file_hit", "File Hit@K"), ("all_required", "AllRequired"),
            ("span_hit", "Span Hit@K"), ("span_ndcg", "Span nDCG@K"),
        ):
            item = comparison.get(key)
            if not item:
                continue
            if key == "span_ndcg":
                diff = f"{item['mean_diff']:+.3f}"
                ci = f"[{item['ci95'][0]:+.3f}, {item['ci95'][1]:+.3f}]"
            else:
                diff = f"{item['mean_diff']:+.1%}"
                ci = f"[{item['ci95'][0]:+.1%}, {item['ci95'][1]:+.1%}]"
            lines.append(
                f"| {label} | {diff} | {ci} | "
                f"{item['wins']}/{item['losses']}/{item['ties']} |"
            )

    if k_sweep:
        lines += [
            "",
            "## K sweep",
            "",
            "| K | File Hit@K | File MRR | Span Hit@K | Span Recall@K | Span AllRequired | nDCG@K | Span MRR |",
            "|---|---|---|---|---|---|---|---|",
        ]
        for k, item in k_sweep.items():
            lines.append(
                f"| {k} | {item['hit_rate']:.1%} | {item['mrr']:.3f} | {item['span_hit_rate']:.1%} | "
                f"{item['span_recall_rate']:.1%} | {item['all_spans_rate']:.1%} | "
                f"{item['ndcg']:.3f} | {item['span_mrr']:.3f} |"
            )

    if judge_summary:
        lines += [
            "",
            "## LLM judge",
            "",
            f"- 评分条数：{judge_summary.get('scored', 0)}/{judge_summary.get('cases', 0)}（错误 {judge_summary.get('errors', 0)}）",
            f"- Pass：{judge_summary.get('pass', 0)}（{judge_summary.get('pass_rate', 0):.1%}）",
            f"- correctness / faithfulness / completeness / citation_support："
            f"{judge_summary.get('correctness', 0):.2f} / {judge_summary.get('faithfulness', 0):.2f} / "
            f"{judge_summary.get('completeness', 0):.2f} / {judge_summary.get('citation_support', 0):.2f}",
            f"- 模型：{judge_summary.get('model', '')}；prompt：{judge_summary.get('prompt_version', '')}",
        ]
        agreement = judge_summary.get("human_agreement")
        if agreement:
            lines.append(
                f"- judge/人工一致率：{agreement['agreement']:.1%}"
                f"（n={agreement['n']}，kappa={agreement['kappa']:.3f}）"
            )
        if judge_review_path:
            lines.append(f"- 人工抽检表：`{judge_review_path}`")

    tags = summary.get("by_tag") or {}
    if tags:
        lines += [
            "",
            "## 按标签",
            "",
            "| tag | cases | hit | pass |",
            "|---|---|---|---|",
        ]
        for tag, item in tags.items():
            lines.append(
                f"| {tag} | {item['cases']} | {item['hit_rate']:.1%} | {item['pass_rate']:.1%} |"
            )

    reasoning = summary.get("by_reasoning") or {}
    if reasoning:
        lines += [
            "",
            "## 按推理类型",
            "",
            "| 推理类型 | cases | Span Hit@K | pass |",
            "|---|---|---|---|",
        ]
        for rtype, item in reasoning.items():
            lines.append(
                f"| {rtype} | {item['cases']} | {item['span_hit_rate']:.1%} | {item['pass_rate']:.1%} |"
            )

    lines += [
        "",
        "## 明细",
        "",
        "| id | 通过 | 通过轮次 | hit | rank | recall | all_req | span | cite_sup | claim | 关键词 | 引用 | 引用命中 | 延迟(ms) | 问题 |",
        "|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|",
    ]
    for r in results:
        skipped = meta["mode"] == "retrieval" and not r.case.answerable
        keyword = "-" if meta["mode"] == "retrieval" else f"{r.keyword_hits[0]}/{r.keyword_hits[1]}"
        citation = "-" if meta["mode"] == "retrieval" else ("是" if r.citation_ok else "否")
        citation_hit = "-" if meta["mode"] == "retrieval" else ("是" if r.citation_hit else "否")
        passes = case_pass_counts.get(r.case.id, [])
        pass_runs = "-" if skipped else (f"{sum(passes)}/{len(passes)}" if passes else "-")
        passed_cell = "-" if skipped else ("impossible" if r.impossible else ("是" if r.passed else "否"))
        hit_cell = "-" if skipped else str(int(r.hit))
        rank_cell = "-" if skipped else str(r.rank)
        recall_cell = "-" if skipped else f"{r.recall:.2f}"
        all_req_cell = "-" if skipped else str(int(r.all_required))
        matches = r.span_matches
        span_cell = f"{sum(matches)}/{len(matches)}" if matches else "-"
        supported, total = r.citation_support
        cite_sup_cell = "-" if meta["mode"] == "retrieval" else f"{supported}/{total}"
        claim_supported, claim_total = r.claim_citation_support
        claim_cell = "-" if meta["mode"] == "retrieval" else f"{claim_supported}/{claim_total}"
        lines.append(
            f"| {r.case.id} | {passed_cell} | {pass_runs} | {hit_cell} | {rank_cell} | "
            f"{recall_cell} | {all_req_cell} | {span_cell} | {cite_sup_cell} | {claim_cell} | {keyword} | "
            f"{citation} | {citation_hit} | {r.latency_ms} | {r.case.question.replace('|', '/')} |"
        )
    lines.append("")
    md_path.write_text("\n".join(lines), encoding="utf-8")
    return md_path, json_path


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="WorkPilot offline RAG evaluation")
    parser.add_argument("--base-url", default="http://localhost:8080", help="Go server base URL")
    parser.add_argument("--dataset", type=Path, default=DEFAULT_DATASET)
    parser.add_argument("--fixtures", type=Path, default=DEFAULT_FIXTURES)
    parser.add_argument("--report-dir", type=Path, default=DEFAULT_REPORT_DIR)
    parser.add_argument("--group", default="", help="reuse an existing group instead of creating one")
    parser.add_argument("--user", default="eval")
    parser.add_argument("--top-k", type=int, default=6)
    parser.add_argument(
        "--min-score",
        type=float,
        default=0.0,
        help="override the server retrieval score threshold for /ask and /search; 0 disables it",
    )
    parser.add_argument("--ask-timeout", type=float, default=180.0)
    parser.add_argument("--index-timeout", type=float, default=300.0)
    parser.add_argument(
        "--split",
        choices=sorted(SPLIT_LABELS),
        default=None,
        help="report label; defaults to benchmark when the dataset name contains 'bench', otherwise smoke",
    )
    parser.add_argument("--tag", dest="run_label", default="", help="free-form label recorded in the report")
    parser.add_argument("--runs", type=int, default=1, help="repeat the case set N times for variance")
    parser.add_argument("--retrieval-only", action="store_true", help="skip the LLM and only run /search")
    parser.add_argument("--no-baseline", action="store_true", help="skip the BM25 baseline")
    parser.add_argument(
        "--baseline-report",
        type=Path,
        default=None,
        help="previous report JSON; prints paired bootstrap deltas for the same cases",
    )
    parser.add_argument("--judge", action="store_true", help="run optional LLM-as-judge on QA answers")
    parser.add_argument("--judge-limit", type=int, default=30, help="max answers to judge; 0 = all")
    parser.add_argument("--judge-labels", type=Path, default=None, help="CSV with human id,verdict labels for agreement")
    parser.add_argument("--judge-model", default=None, help="judge model; defaults to LLM_MODEL")
    parser.add_argument("--judge-base-url", default=None, help="judge OpenAI-compatible base URL")
    parser.add_argument("--judge-api-key", default=None, help="judge API key (never written to reports)")
    parser.add_argument(
        "--k-sweep",
        default="",
        help="comma-separated K values, e.g. 1,3,5; runs retrieval-only for each K",
    )
    parser.add_argument("--no-save", action="store_true", help="do not write report files")
    args = parser.parse_args(argv)
    if args.runs < 1:
        parser.error("--runs must be >= 1")
    if args.top_k < 1:
        parser.error("--top-k must be >= 1")
    if args.min_score < 0 or args.min_score > 1:
        parser.error("--min-score must be between 0 and 1")
    if args.judge_limit < 0:
        parser.error("--judge-limit must be >= 0")
    args.k_sweep_values = []
    if args.k_sweep:
        try:
            args.k_sweep_values = sorted({int(item) for item in args.k_sweep.split(",") if item.strip()})
        except ValueError:
            parser.error("--k-sweep must be a comma-separated list of integers")
        if any(value < 1 for value in args.k_sweep_values):
            parser.error("--k-sweep values must be >= 1")
    return args


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv)
    cases = load_dataset(args.dataset)
    validate_gold_spans(cases, args.fixtures)
    split = infer_split(args.dataset, args.split)
    started = datetime.now(timezone.utc)
    judge_rows: list[dict] = []
    judge_summary: dict | None = None
    judge_review_path: Path | None = None

    with httpx.Client(base_url=args.base_url, timeout=60.0) as client:
        if args.group:
            group_id = args.group
            ensure_group(client, group_id)
            print(f"复用群组 {group_id}")
        else:
            group_id = create_group(client, f"评测-{datetime.now().strftime('%Y-%m-%d %H:%M:%S')}")
            print(f"已创建评测群组 {group_id}")

        names = upload_fixtures(client, group_id, args.fixtures, args.user)
        validate_referenced_files(cases, names)
        print(f"已上传 {len(names)} 份语料，等待解析与索引…")
        corpus_chunks = wait_until_indexed(client, group_id, names, args.index_timeout)
        chunk_texts, chunk_records = fetch_chunks(client, group_id, names)
        print(
            f"语料已就绪（{len(names)} 文件 / {corpus_chunks} chunks / "
            f"{len(chunk_texts)} 个分块已加载），开始评测"
        )
        if not chunk_texts:
            print("警告：未能读取完整 chunk 内容，span/citation 指标会退化；请确认 /files/:id/chunks 可用")
        if corpus_chunks <= args.top_k:
            print(
                f"警告：语料仅 {corpus_chunks} 个 chunk，<= top_k={args.top_k}，"
                "检索会返回全库；Hit@K/MRR 只能证明链路可用，不能作为检索质量结论。"
            )
        impossible = [case.id for case in cases if case.answerable and len(case.required) > args.top_k]
        if impossible:
            print(
                f"警告：{len(impossible)} 个用例的 required_files 多于 top_k={args.top_k}，"
                f"将标记为 impossible 并从通过率中排除：{', '.join(impossible[:8])}"
            )
        oracle = corpus_oracle(cases, chunk_records)
        oracle_summary = oracle["summary"]
        if oracle_summary["all_required_rate"] < 1 or (
            oracle_summary["span_cases"] and oracle_summary["all_spans_rate"] < 1
        ):
            print(
                "警告：语料/标签覆盖上界不足 "
                f"（AllRequired={oracle_summary['all_required_rate']:.1%}，"
                f"SpanAllRequired={oracle_summary['all_spans_rate']:.1%}），请先检查语料与 gold span"
            )

        mode = "retrieval" if args.retrieval_only else "qa"
        run_results: list[list[CaseResult]] = []
        run_summaries: list[dict] = []
        case_pass_counts: dict[str, list[bool]] = {}
        for run_index in range(1, args.runs + 1):
            if args.runs > 1:
                print(f"\n---- 第 {run_index}/{args.runs} 轮 ----")
            results = run(
                client, group_id, args.user, cases, mode, args.top_k, args.ask_timeout,
                min_score=args.min_score, chunk_texts=chunk_texts,
            )
            run_results.append(results)
            run_summaries.append(summarize(results, mode))
            for result in results:
                case_pass_counts.setdefault(result.case.id, []).append(result.passed)

        results = run_results[-1]
        summary = run_summaries[-1]
        stability = aggregate_stability(run_summaries) if len(run_summaries) > 1 else None

        saved_comparison = None
        if args.baseline_report:
            baseline_cases, baseline_top_k = load_report_cases(args.baseline_report)
            saved_comparison = compare_saved_report(results, baseline_cases, baseline_top_k)
            if not saved_comparison:
                print(f"无法与历史报告配对：{args.baseline_report}")
            elif saved_comparison.get("baseline_top_k") and saved_comparison.get("baseline_top_k") != args.top_k:
                print(
                    "警告：历史报告 top_k="
                    f"{saved_comparison['baseline_top_k']} 与当前 top_k={args.top_k} 不一致，对比仅供诊断"
                )

        usage = None
        if mode == "qa":
            try:
                usage = fetch_usage(client, group_id, started, datetime.now(timezone.utc))
            except Exception as exc:  # usage is informational only
                print(f"读取用量失败：{exc}")

        baselines: dict[str, dict] = {}
        baseline_results: list[CaseResult] = []
        comparison: dict = {}
        if not args.no_baseline:
            baseline = bm25_baseline(cases, chunk_records, args.top_k)
            if baseline:
                baseline_summary, baseline_results = baseline
                baselines["bm25"] = baseline_summary
                comparison = compare_retrieval(results, baseline_results)

        k_sweep: dict[str, dict] = {}
        for k in args.k_sweep_values:
            print(f"\n---- K sweep: top_k={k} ----")
            sweep_results = run(
                client, group_id, args.user, cases, "retrieval", k, args.ask_timeout,
                min_score=args.min_score, chunk_texts=chunk_texts,
            )
            k_sweep[str(k)] = summarize(sweep_results, "retrieval")

        if args.judge:
            if mode != "qa":
                print("跳过 judge：仅完整问答模式可运行 LLM-as-judge")
            else:
                env = os.environ
                config = judge_mod.JudgeConfig(
                    base_url=args.judge_base_url or env.get("LLM_BASE_URL", "http://localhost:11434/v1"),
                    api_key=args.judge_api_key if args.judge_api_key is not None else env.get("LLM_API_KEY", "ollama"),
                    model=args.judge_model or env.get("LLM_MODEL", "qwen2.5:7b"),
                )
                print(f"\n---- LLM judge（{config.model}，最多 {args.judge_limit or '全部'} 条）----")
                judge_rows, judge_summary, judge_review_path = run_judge(
                    client, results, config, args.judge_limit, args.report_dir
                )
                if args.judge_labels and judge_rows:
                    labels = judge_mod.read_labels(args.judge_labels)
                    judge_summary["human_agreement"] = judge_mod.agreement(judge_rows, labels)
                print(
                    f"judge: {judge_summary.get('pass', 0)}/{judge_summary.get('scored', 0)} pass，"
                    f"correctness={judge_summary.get('correctness', 0):.2f}，"
                    f"faithfulness={judge_summary.get('faithfulness', 0):.2f}，"
                    f"错误 {judge_summary.get('errors', 0)}"
                )
                agreement = judge_summary.get("human_agreement")
                if agreement:
                    print(
                        f"judge/人工一致率 {agreement['agreement']:.1%}（n={agreement['n']}，"
                        f"kappa={agreement['kappa']:.3f}）"
                    )
                print(f"人工抽检表：{judge_review_path}")

    print_report(
        summary, results, usage, mode, group_id, args.dataset, split, args.run_label,
        run_summaries, stability, corpus_chunks, corpus_chunks <= args.top_k,
        top_k=args.top_k, min_score=args.min_score, oracle=oracle, comparison=comparison,
        baselines=baselines, k_sweep=k_sweep, judge_summary=judge_summary,
    )

    if saved_comparison:
        print(f"\n与历史报告配对（n={saved_comparison['n']}，bootstrap 95% CI）：")
        for key, label in (
            ("file_hit", "FileHit"),
            ("all_required", "AllRequired"),
            ("span_hit", "SpanHit"),
            ("span_recall", "SpanRecall"),
            ("span_ndcg", "Span nDCG"),
        ):
            item = saved_comparison.get(key)
            if not item:
                continue
            unit = "" if key == "span_ndcg" else "%"
            scale = 1 if key == "span_ndcg" else 100
            print(
                f"  {label:<12} {item['mean_diff'] * scale:+.3f}{unit} "
                f"[{item['ci95'][0] * scale:+.3f}, {item['ci95'][1] * scale:+.3f}] "
                f"(赢/输/平 {item['wins']}/{item['losses']}/{item['ties']})"
            )

    if not args.no_save:
        meta = {
            "schema_version": SCHEMA_VERSION,
            "eval_version": EVAL_VERSION,
            "split": split,
            "run_label": args.run_label,
            "runs": args.runs,
            "group_id": group_id,
            "base_url": args.base_url,
            "dataset": str(args.dataset),
            "dataset_sha256": hash_file(args.dataset),
            "fixtures": str(args.fixtures),
            "fixtures_sha256": hash_dir(args.fixtures),
            "fixtures_count": len(names),
            "corpus_chunks": corpus_chunks,
            "chunks_fetched": len(chunk_texts),
            "retrieval_ceiling": corpus_chunks <= args.top_k,
            "mode": mode,
            "top_k": args.top_k,
            "min_score": args.min_score,
            "impossible_cases": summary.get("impossible_cases", 0),
            "impossible_ids": summary.get("impossible_ids", []),
            "k_sweep": args.k_sweep_values,
            "judge": judge_summary,
            "baseline_report": str(args.baseline_report) if args.baseline_report else "",
            "baseline_comparison": saved_comparison,
            "ask_timeout": args.ask_timeout,
            "index_timeout": args.index_timeout,
            "config": config_snapshot(),
            "git": git_info(),
            "started_at": started.isoformat(),
            "finished_at": datetime.now(timezone.utc).isoformat(),
        }
        md_path, json_path = write_reports(
            args.report_dir, summary, results, usage, meta, run_summaries, stability,
            case_pass_counts, baselines, k_sweep, judge_rows, judge_summary, judge_review_path,
            oracle=oracle, comparison=comparison,
        )
        print(f"\n报告已写入：\n  {md_path}\n  {json_path}")

    return 1 if any(item["errors"] for item in run_summaries) else 0


if __name__ == "__main__":
    raise SystemExit(main())
