"""Dataset linter for the offline RAG evaluation.

The linter turns the P1 data contract into actionable errors: duplicate
paragraphs, template reuse, answer leakage, ambiguous gold spans, impossible
required-file counts, missing refusal coverage and document coverage gaps.

Run from sidecar/:

    .venv/Scripts/python -m app.eval_lint
    .venv/Scripts/python -m app.eval_lint --json eval/reports/lint.json
    .venv/Scripts/python -m app.eval_lint --dataset eval/datasets/smoke.jsonl \
        --fixtures eval/fixtures --top-k 6
"""

from __future__ import annotations

import argparse
import json
import re
from collections import Counter, defaultdict
from pathlib import Path

from app import eval as runner
from app import eval_metrics as metrics

DEFAULT_TOP_K = 3
DEFAULT_MAX_TEMPLATE = 2
DEFAULT_MIN_UNANSWERABLE = 20
DEFAULT_MIN_REFUSAL_TYPES = 5
DEFAULT_MAX_DUOSHAO_RATIO = 0.2
DEFAULT_SHARED_FILES = "shared-ops-runbook.md"
DEFAULT_ALIASES = "星河=xinghe,天穹=tianqiong,潮汐=chaoxi,Phoenix=phoenix"
MIN_PARAGRAPH_CHARS = 30
BOOLEAN_ANSWERS = {
    "是", "否", "超过", "不超过", "未超过", "需要", "不需要", "可以", "不可以",
}


def parse_aliases(value: str) -> dict[str, str]:
    aliases: dict[str, str] = {}
    for part in value.split(","):
        item = part.strip()
        if not item:
            continue
        if "=" in item:
            alias, project = item.split("=", 1)
            alias, project = alias.strip(), project.strip()
        else:
            alias = project = item
        if alias and project:
            aliases[alias] = project
            aliases[project] = project
    return aliases


def template_of(question: str, aliases: dict[str, str]) -> str:
    normalized = question.strip()
    for alias in sorted(aliases, key=len, reverse=True):
        normalized = re.sub(re.escape(alias), "<P>", normalized, flags=re.IGNORECASE)
    return normalized


def paragraphs(text: str) -> list[str]:
    out: list[str] = []
    for block in re.split(r"\n\s*\n", text):
        normalized = metrics.normalize(block)
        if len(normalized) >= MIN_PARAGRAPH_CHARS:
            out.append(normalized)
    return out


def _add(items: list[dict], code: str, message: str, case_id: str = "") -> None:
    items.append({"code": code, "case_id": case_id, "message": message})


def lint_dataset(
    cases: list[runner.Case],
    fixture_names: list[str],
    texts: dict[str, str],
    top_k: int = DEFAULT_TOP_K,
    max_template: int = DEFAULT_MAX_TEMPLATE,
    min_unanswerable: int = DEFAULT_MIN_UNANSWERABLE,
    min_refusal_types: int = DEFAULT_MIN_REFUSAL_TYPES,
    max_duoshao_ratio: float = DEFAULT_MAX_DUOSHAO_RATIO,
    shared_files: set[str] | None = None,
    aliases: dict[str, str] | None = None,
) -> dict:
    """Run every dataset check and return a JSON-serializable report."""
    shared = shared_files or {DEFAULT_SHARED_FILES}
    aliases = aliases or parse_aliases(DEFAULT_ALIASES)
    errors: list[dict] = []
    warnings: list[dict] = []
    known = set(fixture_names)

    for case in cases:
        for name in case.evidence_files:
            if name not in known:
                _add(errors, "unknown_file", f"referenced file not found: {name}", case.id)
        if case.answerable and len(case.required) > top_k:
            _add(
                errors,
                "required_gt_top_k",
                f"required_files={len(case.required)} > top_k={top_k}",
                case.id,
            )
        if case.acceptable_files and set(case.acceptable_files) & set(case.required):
            _add(
                warnings,
                "acceptable_also_required",
                "acceptable_files overlaps required_files",
                case.id,
            )

    # 1. Paragraph duplication across files except the shared runbook.
    owners: dict[str, set[str]] = defaultdict(set)
    for name, text in texts.items():
        for paragraph in paragraphs(text):
            owners[paragraph].add(name)
    duplicate_paragraphs = 0
    for paragraph, files in owners.items():
        if len(files) <= 1 or files <= shared:
            continue
        duplicate_paragraphs += 1
        _add(
            errors,
            "duplicate_paragraph",
            f"{', '.join(sorted(files))}: {paragraph[:60]}",
        )

    # 2. Template reuse within a split and dev/test overlap.
    by_split: dict[str, Counter[str]] = defaultdict(Counter)
    exact_questions: dict[str, list[str]] = defaultdict(list)
    for case in cases:
        split = case.split or "unknown"
        template = template_of(case.question, aliases)
        by_split[split][template] += 1
        exact_questions[metrics.normalize(case.question)].append(case.id)
    template_over_limit = 0
    for split, counts in by_split.items():
        for template, count in counts.items():
            if count > max_template:
                template_over_limit += 1
                _add(
                    errors,
                    "template_reused",
                    f"split={split} count={count}: {template[:80]}",
                )
    dev_templates = set(by_split.get("dev", {}))
    test_templates = set(by_split.get("test", {}))
    if not dev_templates or not test_templates:
        _add(
            warnings,
            "split_missing",
            f"splits present: {sorted(by_split)}; dev/test overlap check skipped",
        )
    overlap = sorted(dev_templates & test_templates)
    for template in overlap:
        _add(errors, "dev_test_template_overlap", template[:100])
    for normalized, ids in exact_questions.items():
        if len(ids) > 1:
            _add(errors, "duplicate_question", f"{', '.join(ids)}: {normalized[:80]}")

    # 3. Answer leakage into the question.
    leakage = 0
    for case in cases:
        question = metrics.normalize(case.question)
        for answer in case.expected_answers:
            normalized = metrics.normalize(answer)
            if not normalized or normalized in BOOLEAN_ANSWERS:
                continue
            if normalized in question:
                leakage += 1
                _add(errors, "answer_leakage", f"expected_answer={answer!r}", case.id)
        for keyword in case.answer_keywords:
            if any(ch.isdigit() for ch in keyword) and metrics.keyword_present(question, keyword):
                leakage += 1
                _add(errors, "answer_leakage", f"numeric keyword={keyword!r}", case.id)

    # 4. Gold spans: must exist, must not exist outside evidence files, and each
    # required text file must carry at least one span.
    span_missing = 0
    span_ambiguous = 0
    required_without_span = 0
    for case in cases:
        if not case.gold_spans:
            continue
        evidence = set(case.evidence_files)
        text_evidence = {name for name in evidence if name in texts}
        binary_evidence = evidence - text_evidence
        if not text_evidence:
            _add(
                warnings,
                "span_check_skipped",
                "all evidence files are binary or missing from text corpus",
                case.id,
            )
            continue
        for span in case.gold_spans:
            normalized = metrics.normalize(span)
            present = [
                name for name, text in texts.items()
                if normalized in metrics.normalize(text)
            ]
            if not present:
                if binary_evidence:
                    _add(
                        warnings,
                        "span_check_partial",
                        f"span not in text files; binary evidence unchecked: {span[:48]}",
                        case.id,
                    )
                else:
                    span_missing += 1
                    _add(errors, "span_missing", span[:80], case.id)
                continue
            outside = [name for name in present if name not in evidence]
            if outside:
                span_ambiguous += 1
                _add(
                    errors,
                    "span_ambiguous",
                    f"span appears outside evidence files {sorted(outside)}: {span[:48]}",
                    case.id,
                )
        for name in case.required:
            if name not in texts:
                continue
            normalized_text = metrics.normalize(texts[name])
            if not any(metrics.normalize(span) in normalized_text for span in case.gold_spans):
                required_without_span += 1
                _add(
                    errors,
                    "required_file_without_span",
                    f"required file has no gold span: {name}",
                    case.id,
                )

    # 5. Refusal coverage.
    unanswerable = [case for case in cases if not case.answerable]
    refusal_types = Counter(case.refusal_type or "none" for case in unanswerable)
    refusal_subtypes = Counter(
        case.refusal_subtype or case.refusal_type or "none" for case in unanswerable
    )
    if len(unanswerable) < min_unanswerable:
        _add(
            errors,
            "unanswerable_too_few",
            f"{len(unanswerable)} < {min_unanswerable}",
        )
    if len(refusal_types) < min_refusal_types:
        _add(
            errors,
            "refusal_types_too_few",
            f"{len(refusal_types)} < {min_refusal_types}: {dict(refusal_types)}",
        )

    # 6. Document coverage: every fixture should be evidence for at least one case.
    referenced = {name for case in cases for name in case.evidence_files}
    unreferenced = sorted(known - referenced)
    for name in unreferenced:
        _add(warnings, "document_unreferenced", name)

    # 7. Naturalness metrics (reported, not all hard failures).
    question_count = len(cases)
    duoshao = sum(1 for case in cases if "是多少" in case.question)
    project_named = sum(
        1 for case in cases
        if any(alias.lower() in case.question.lower() for alias in aliases)
    )
    single_file = sum(1 for case in cases if len(case.required) <= 1)
    average_runes = (
        sum(len(case.question) for case in cases) / question_count
    ) if question_count else 0.0
    duoshao_ratio = duoshao / question_count if question_count else 0.0
    if duoshao_ratio > max_duoshao_ratio:
        _add(
            warnings,
            "duoshao_ratio_high",
            f"{duoshao}/{question_count} = {duoshao_ratio:.1%} > {max_duoshao_ratio:.1%}",
        )

    unique_templates = len({
        template_of(case.question, aliases) for case in cases
    })
    metrics_out = {
        "cases": question_count,
        "answerable": sum(1 for case in cases if case.answerable),
        "unanswerable": len(unanswerable),
        "splits": {split: sum(counts.values()) for split, counts in by_split.items()},
        "refusal_types": dict(refusal_types),
        "refusal_subtypes": dict(refusal_subtypes),
        "templates": unique_templates,
        "effective_n": unique_templates,
        "duplicate_paragraphs": duplicate_paragraphs,
        "template_over_limit": template_over_limit,
        "dev_test_template_overlap": len(overlap),
        "answer_leakage": leakage,
        "span_missing": span_missing,
        "span_ambiguous": span_ambiguous,
        "required_file_without_span": required_without_span,
        "unreferenced_documents": len(unreferenced),
        "single_file_ratio": (single_file / question_count) if question_count else 0.0,
        "duoshao_ratio": duoshao_ratio,
        "project_named_ratio": (project_named / question_count) if question_count else 0.0,
        "average_question_runes": round(average_runes, 1),
    }
    return {
        "errors": errors,
        "warnings": warnings,
        "error_counts": dict(Counter(item["code"] for item in errors)),
        "warning_counts": dict(Counter(item["code"] for item in warnings)),
        "metrics": metrics_out,
        "examples": {
            "duplicate_paragraphs": [
                item["message"] for item in errors if item["code"] == "duplicate_paragraph"
            ][:8],
            "dev_test_template_overlap": [
                item["message"] for item in errors
                if item["code"] == "dev_test_template_overlap"
            ][:8],
            "span_ambiguous": [
                item["message"] for item in errors if item["code"] == "span_ambiguous"
            ][:8],
        },
    }


def print_report(result: dict, limit: int = 20) -> None:
    metrics_out = result["metrics"]
    print("\n==== WorkPilot 评测数据 lint ====")
    print(f"用例：{metrics_out['cases']}（可答 {metrics_out['answerable']} / "
          f"不可答 {metrics_out['unanswerable']}）；split={metrics_out['splits']}")
    print(f"错误：{len(result['errors'])}，警告：{len(result['warnings'])}")
    print(
        f"模板：{metrics_out['templates']} 个；有效 n={metrics_out['effective_n']}；"
        f"重复段落={metrics_out['duplicate_paragraphs']}；"
        f"dev/test 模板重叠={metrics_out['dev_test_template_overlap']}"
    )
    print(
        f"泄漏={metrics_out['answer_leakage']}；span 缺失={metrics_out['span_missing']}；"
        f"span 歧义={metrics_out['span_ambiguous']}；"
        f"required 无 span={metrics_out['required_file_without_span']}"
    )
    print(
        f"单文件题={metrics_out['single_file_ratio']:.1%}；"
        f"“是多少”={metrics_out['duoshao_ratio']:.1%}；"
        f"题干含项目名={metrics_out['project_named_ratio']:.1%}；"
        f"平均长度={metrics_out['average_question_runes']} 字"
    )
    print(f"不可答类型：{metrics_out['refusal_types']}")
    print(f"不可答子类型：{metrics_out['refusal_subtypes']}")
    if result["error_counts"]:
        print("\n错误分类：")
        for code, count in sorted(result["error_counts"].items(), key=lambda item: (-item[1], item[0])):
            print(f"  {code:<32} {count}")
    if result["warnings"]:
        print("\n警告分类：")
        for code, count in sorted(result["warning_counts"].items(), key=lambda item: (-item[1], item[0])):
            print(f"  {code:<32} {count}")
    if result["errors"]:
        print(f"\n错误样例（最多 {limit} 条）：")
        for item in result["errors"][:limit]:
            prefix = f"{item['case_id']} " if item["case_id"] else ""
            print(f"  [{item['code']}] {prefix}{item['message']}")


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Lint an offline RAG evaluation dataset")
    parser.add_argument("--dataset", type=Path, default=runner.EVAL_DIR / "datasets" / "bench-all.jsonl")
    parser.add_argument("--fixtures", type=Path, default=runner.EVAL_DIR / "corpus" / "bench")
    parser.add_argument("--top-k", type=int, default=DEFAULT_TOP_K)
    parser.add_argument("--max-template", type=int, default=DEFAULT_MAX_TEMPLATE)
    parser.add_argument("--min-unanswerable", type=int, default=DEFAULT_MIN_UNANSWERABLE)
    parser.add_argument("--min-refusal-types", type=int, default=DEFAULT_MIN_REFUSAL_TYPES)
    parser.add_argument("--max-duoshao-ratio", type=float, default=DEFAULT_MAX_DUOSHAO_RATIO)
    parser.add_argument("--shared-files", default=DEFAULT_SHARED_FILES)
    parser.add_argument("--project-aliases", default=DEFAULT_ALIASES)
    parser.add_argument("--json", type=Path, default=None, help="write the full report as JSON")
    parser.add_argument("--warn-only", action="store_true", help="exit 0 even when errors exist")
    parser.add_argument("--limit", type=int, default=20)
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv)
    cases = runner.load_dataset(args.dataset)
    fixture_names = sorted(p.name for p in args.fixtures.glob("*") if p.is_file())
    if not fixture_names:
        raise SystemExit(f"no fixtures found in {args.fixtures}")
    result = lint_dataset(
        cases,
        fixture_names,
        runner.fixture_texts(args.fixtures),
        top_k=args.top_k,
        max_template=args.max_template,
        min_unanswerable=args.min_unanswerable,
        min_refusal_types=args.min_refusal_types,
        max_duoshao_ratio=args.max_duoshao_ratio,
        shared_files={item.strip() for item in args.shared_files.split(",") if item.strip()},
        aliases=parse_aliases(args.project_aliases),
    )
    result["dataset"] = str(args.dataset)
    result["fixtures"] = str(args.fixtures)
    result["top_k"] = args.top_k
    print_report(result, args.limit)
    if args.json:
        args.json.parent.mkdir(parents=True, exist_ok=True)
        args.json.write_text(json.dumps(result, ensure_ascii=False, indent=2), encoding="utf-8")
        print(f"\nJSON 报告：{args.json}")
    if result["errors"] and not args.warn_only:
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
