"""Offline evaluation runner for RAG QA.

Uploads the fixture corpus into a fresh group, waits for parse + index, then
runs every dataset question through `/search` (retrieval only) or `/ask`
(full QA) and writes a Markdown/JSON report.

Run from sidecar/ with the venv active:

    .venv/Scripts/python -m app.eval --base-url http://localhost:8080
"""

from __future__ import annotations

import argparse
import json
import time
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path

import httpx

from app import eval_metrics as metrics

EVAL_DIR = Path(__file__).resolve().parents[1] / "eval"
DEFAULT_DATASET = EVAL_DIR / "datasets" / "demo.jsonl"
DEFAULT_FIXTURES = EVAL_DIR / "fixtures"
DEFAULT_REPORT_DIR = EVAL_DIR / "reports"


@dataclass
class Case:
    id: str
    question: str
    expected_files: list[str] = field(default_factory=list)
    answer_keywords: list[str] = field(default_factory=list)
    answerable: bool = True
    tags: list[str] = field(default_factory=list)


@dataclass
class CaseResult:
    case: Case
    sources: list[dict]
    answer: str
    latency_ms: int
    error: str = ""

    @property
    def hit(self) -> bool:
        return metrics.hit_at_k(self.sources, self.case.expected_files)

    @property
    def rank(self) -> int:
        return metrics.first_hit_rank(self.sources, self.case.expected_files)

    @property
    def keyword_hits(self) -> tuple[int, int]:
        return metrics.keyword_hits(self.answer, self.case.answer_keywords)

    @property
    def keyword_ok(self) -> bool:
        hits, total = self.keyword_hits
        return total == 0 or hits == total

    @property
    def citation_ok(self) -> bool:
        return metrics.citation_valid(self.answer, len(self.sources))

    @property
    def refused(self) -> bool:
        return metrics.refused(self.answer, len(self.sources))

    @property
    def passed(self) -> bool:
        if self.error:
            return False
        if not self.case.answerable:
            return self.refused
        return self.hit and self.keyword_ok and self.citation_ok


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
        cases.append(Case(
            id=str(raw.get("id") or f"case-{line_no}"),
            question=question,
            expected_files=[str(x) for x in raw.get("expected_files") or []],
            answer_keywords=[str(x) for x in raw.get("answer_keywords") or []],
            answerable=bool(raw.get("answerable", True)),
            tags=[str(x) for x in raw.get("tags") or []],
        ))
    if not cases:
        raise SystemExit(f"dataset {path} is empty")
    return cases


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
        with path.open("rb") as fh:
            resp = client.post(
                f"/api/groups/{group_id}/files",
                params={"user": user},
                files={"file": (path.name, fh, "text/markdown")},
            )
        if resp.status_code != 201:
            raise SystemExit(f"upload {path.name} failed: HTTP {resp.status_code}: {resp.text[:300]}")
        names.append(path.name)
    if not names:
        raise SystemExit(f"no fixtures found in {fixtures_dir}")
    return names


def wait_until_indexed(client: httpx.Client, group_id: str, names: list[str], timeout_s: float) -> None:
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
            return
        if time.monotonic() > deadline:
            raise SystemExit("timed out waiting for indexing: " + ", ".join(pending))
        time.sleep(2)


def search(client: httpx.Client, group_id: str, question: str, top_k: int) -> list[dict]:
    resp = client.post(f"/api/groups/{group_id}/search", json={"query": question, "top_k": top_k})
    resp.raise_for_status()
    return resp.json().get("sources") or []


def ask(client: httpx.Client, group_id: str, user: str, question: str, timeout_s: float) -> tuple[list[dict], str]:
    sources: list[dict] = []
    chunks: list[str] = []
    error: str | None = None
    with client.stream(
        "POST",
        f"/api/groups/{group_id}/ask",
        json={"user": user, "question": question},
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


def run(client: httpx.Client, group_id: str, user: str, cases: list[Case], mode: str, top_k: int, ask_timeout: float) -> list[CaseResult]:
    results: list[CaseResult] = []
    for index, case in enumerate(cases, start=1):
        print(f"[{index}/{len(cases)}] {case.id} {case.question[:32]}…", flush=True)
        start = time.perf_counter()
        try:
            if mode == "retrieval":
                sources, answer = search(client, group_id, case.question, top_k), ""
            else:
                sources, answer = ask(client, group_id, user, case.question, ask_timeout)
            latency_ms = int((time.perf_counter() - start) * 1000)
            results.append(CaseResult(case=case, sources=sources, answer=answer, latency_ms=latency_ms))
        except Exception as exc:  # keep going, the report shows per-case failures
            latency_ms = int((time.perf_counter() - start) * 1000)
            results.append(CaseResult(case=case, sources=[], answer="", latency_ms=latency_ms, error=str(exc)))
    return results


def summarize(results: list[CaseResult], mode: str) -> dict:
    answerable = [r for r in results if r.case.answerable]
    unanswerable = [r for r in results if not r.case.answerable]
    latencies = [r.latency_ms for r in results]

    hits = sum(1 for r in answerable if r.hit)
    ranks = [r.rank for r in answerable if r.rank > 0]
    keyword_hits = sum(r.keyword_hits[0] for r in answerable)
    keyword_total = sum(r.keyword_hits[1] for r in answerable)
    citations = sum(1 for r in answerable if r.citation_ok)
    refusals = sum(1 for r in unanswerable if r.refused)

    summary = {
        "cases": len(results),
        "answerable": len(answerable),
        "unanswerable": len(unanswerable),
        "errors": sum(1 for r in results if r.error),
        "hit_rate": (hits / len(answerable)) if answerable else 0.0,
        "hits": hits,
        "mrr": (sum(1 / r for r in ranks) / len(answerable)) if answerable else 0.0,
        "keyword_rate": (keyword_hits / keyword_total) if keyword_total else 0.0,
        "keyword_hits": keyword_hits,
        "keyword_total": keyword_total,
        "citation_rate": (citations / len(answerable)) if answerable else 0.0,
        "citation_ok": citations,
        "refusal_rate": (refusals / len(unanswerable)) if unanswerable else 0.0,
        "refusal_ok": refusals,
        "passed": sum(1 for r in results if r.passed),
        "latency_p50_ms": int(metrics.percentile(latencies, 0.5)),
        "latency_p95_ms": int(metrics.percentile(latencies, 0.95)),
    }
    if mode == "retrieval":
        for key in ("keyword_rate", "keyword_hits", "keyword_total", "citation_rate", "citation_ok", "refusal_rate", "refusal_ok"):
            summary.pop(key, None)
    return summary


def print_report(summary: dict, results: list[CaseResult], usage: dict | None, mode: str, group_id: str, dataset: Path) -> None:
    print("\n==== WorkPilot 离线评测 ====")
    print(f"群组    : {group_id}")
    print(f"数据集  : {dataset}")
    print(f"模式    : {'仅检索' if mode == 'retrieval' else '完整问答'}")
    print(f"用例    : {summary['cases']}（可答 {summary['answerable']} / 不可答 {summary['unanswerable']}）")
    print("\n指标：")
    print(f"  Hit@K 命中率      : {summary['hits']}/{summary['answerable']} ({summary['hit_rate']:.1%})")
    print(f"  MRR               : {summary['mrr']:.3f}")
    if mode != "retrieval":
        print(f"  关键词命中率      : {summary['keyword_hits']}/{summary['keyword_total']} ({summary['keyword_rate']:.1%})")
        print(f"  引用有效性        : {summary['citation_ok']}/{summary['answerable']} ({summary['citation_rate']:.1%})")
        print(f"  不可答拒答正确率  : {summary['refusal_ok']}/{summary['unanswerable']} ({summary['refusal_rate']:.1%})")
    print(f"  延迟 P50 / P95    : {summary['latency_p50_ms']} / {summary['latency_p95_ms']} ms")
    print(f"  用例通过          : {summary['passed']}/{summary['cases']}")
    if usage:
        s = usage.get("summary") or {}
        currency = (usage.get("pricing") or {}).get("currency") or ""
        print(f"  LLM 调用          : {s.get('calls', 0)} 次（失败 {s.get('failed', 0)}），"
              f"tokens {s.get('total_tokens', 0)}，成本 {s.get('cost', 0)} {currency}")

    print("\n明细：")
    for r in results:
        if r.error:
            status = f"调用失败：{r.error[:60]}"
        elif r.case.answerable:
            status = "通过" if r.passed else "未通过"
        else:
            status = "通过（拒答）" if r.passed else "未通过"
        keywords = f"关键词 {r.keyword_hits[0]}/{r.keyword_hits[1]}" if mode != "retrieval" else ""
        print(f"  {r.case.id:<4} {status:<12} hit={int(r.hit)} rank={r.rank} {keywords:<16} {r.latency_ms:>5} ms  {r.case.question[:28]}")


def write_reports(report_dir: Path, summary: dict, results: list[CaseResult], usage: dict | None, meta: dict) -> tuple[Path, Path]:
    report_dir.mkdir(parents=True, exist_ok=True)
    stamp = datetime.now().strftime("%Y%m%d-%H%M%S")
    json_path = report_dir / f"eval-{stamp}.json"
    md_path = report_dir / f"eval-{stamp}.md"

    payload = {
        "meta": meta,
        "summary": summary,
        "usage": usage,
        "cases": [
            {
                "id": r.case.id,
                "question": r.case.question,
                "tags": r.case.tags,
                "answerable": r.case.answerable,
                "expected_files": r.case.expected_files,
                "answer_keywords": r.case.answer_keywords,
                "sources": [{"file_name": s.get("file_name"), "chunk_index": s.get("chunk_index"), "score": s.get("score")} for s in r.sources],
                "answer": r.answer,
                "latency_ms": r.latency_ms,
                "hit": r.hit,
                "rank": r.rank,
                "keyword_hits": list(r.keyword_hits),
                "citation_ok": r.citation_ok,
                "refused": r.refused,
                "passed": r.passed,
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
        f"- 群组：`{meta['group_id']}`",
        f"- 数据集：`{meta['dataset']}`",
        f"- 模式：{'仅检索' if meta['mode'] == 'retrieval' else '完整问答'}",
        f"- 用例：{summary['cases']}（可答 {summary['answerable']} / 不可答 {summary['unanswerable']}）",
        "",
        "## 指标",
        "",
        f"- Hit@K 命中率：{summary['hits']}/{summary['answerable']}（{summary['hit_rate']:.1%}）",
        f"- MRR：{summary['mrr']:.3f}",
    ]
    if meta["mode"] != "retrieval":
        lines += [
            f"- 关键词命中率：{summary['keyword_hits']}/{summary['keyword_total']}（{summary['keyword_rate']:.1%}）",
            f"- 引用有效性：{summary['citation_ok']}/{summary['answerable']}（{summary['citation_rate']:.1%}）",
            f"- 不可答拒答正确率：{summary['refusal_ok']}/{summary['unanswerable']}（{summary['refusal_rate']:.1%}）",
        ]
    lines += [
        f"- 延迟 P50 / P95：{summary['latency_p50_ms']} / {summary['latency_p95_ms']} ms",
        f"- 用例通过：{summary['passed']}/{summary['cases']}",
    ]
    if usage:
        s = usage.get("summary") or {}
        currency = (usage.get("pricing") or {}).get("currency") or ""
        lines.append(f"- LLM 调用：{s.get('calls', 0)} 次（失败 {s.get('failed', 0)}），tokens {s.get('total_tokens', 0)}，成本 {s.get('cost', 0)} {currency}")
    lines += [
        "",
        "## 明细",
        "",
        "| id | 通过 | hit | rank | 关键词 | 引用 | 延迟(ms) | 问题 |",
        "|---|---|---|---|---|---|---|---|",
    ]
    for r in results:
        keyword = "-" if meta["mode"] == "retrieval" else f"{r.keyword_hits[0]}/{r.keyword_hits[1]}"
        citation = "-" if meta["mode"] == "retrieval" else ("是" if r.citation_ok else "否")
        lines.append(
            f"| {r.case.id} | {'是' if r.passed else '否'} | {int(r.hit)} | {r.rank} | {keyword} | {citation} | {r.latency_ms} | {r.case.question.replace('|', '/')} |"
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
    parser.add_argument("--ask-timeout", type=float, default=180.0)
    parser.add_argument("--index-timeout", type=float, default=300.0)
    parser.add_argument("--retrieval-only", action="store_true", help="skip the LLM and only run /search")
    parser.add_argument("--no-save", action="store_true", help="do not write report files")
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv)
    cases = load_dataset(args.dataset)
    started = datetime.now(timezone.utc)

    with httpx.Client(base_url=args.base_url, timeout=60.0) as client:
        if args.group:
            group_id = args.group
            ensure_group(client, group_id)
            print(f"复用群组 {group_id}")
        else:
            group_id = create_group(client, f"评测-{datetime.now().strftime('%Y-%m-%d %H:%M:%S')}")
            print(f"已创建评测群组 {group_id}")

        names = upload_fixtures(client, group_id, args.fixtures, args.user)
        print(f"已上传 {len(names)} 份语料，等待解析与索引…")
        wait_until_indexed(client, group_id, names, args.index_timeout)
        print("语料已就绪，开始评测")

        mode = "retrieval" if args.retrieval_only else "qa"
        results = run(client, group_id, args.user, cases, mode, args.top_k, args.ask_timeout)
        summary = summarize(results, mode)

        usage = None
        if mode == "qa":
            try:
                usage = fetch_usage(client, group_id, started, datetime.now(timezone.utc))
            except Exception as exc:  # usage is informational only
                print(f"读取用量失败：{exc}")

    print_report(summary, results, usage, mode, group_id, args.dataset)

    if not args.no_save:
        meta = {
            "group_id": group_id,
            "dataset": str(args.dataset),
            "mode": mode,
            "top_k": args.top_k,
            "started_at": started.isoformat(),
            "finished_at": datetime.now(timezone.utc).isoformat(),
        }
        md_path, json_path = write_reports(args.report_dir, summary, results, usage, meta)
        print(f"\n报告已写入：\n  {md_path}\n  {json_path}")

    return 1 if summary["errors"] else 0


if __name__ == "__main__":
    raise SystemExit(main())
