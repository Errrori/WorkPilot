"""Optional LLM-as-judge for answer correctness and citation support.

The judge uses the same OpenAI-compatible endpoint as the product. It is off by
default because it costs tokens and is non-deterministic; deterministic metrics
remain the primary signal.
"""

from __future__ import annotations

import csv
import json
import re
from dataclasses import dataclass
from pathlib import Path

import httpx

JUDGE_PROMPT_VERSION = "2026-10-20"

SYSTEM_PROMPT = """你是 RAG 问答评测员。只根据给定资料和参考答案评分，不要补充资料外的知识。
输出严格 JSON，不要 Markdown 代码块，不要额外解释：
{
  "correctness": 1-5,
  "faithfulness": 1-5,
  "completeness": 1-5,
  "citation_support": 1-5,
  "verdict": "pass" or "fail",
  "reason": "不超过 80 字的中文理由"
}
评分标准：
- correctness：结论是否正确、数值/日期/负责人是否准确。
- faithfulness：回答是否只使用资料中的信息，是否出现编造。
- completeness：是否覆盖问题要求的要点。
- citation_support：引用编号是否真的支撑对应结论。
只要存在编造或关键事实错误，verdict 必须是 fail。"""


@dataclass
class JudgeConfig:
    base_url: str
    api_key: str
    model: str
    timeout: float = 60.0


def config_from_env(env) -> JudgeConfig:
    return JudgeConfig(
        base_url=env.get("LLM_BASE_URL", "http://localhost:11434/v1"),
        api_key=env.get("LLM_API_KEY", "ollama"),
        model=env.get("LLM_MODEL", "qwen2.5:7b"),
    )


def _parse_json(content: str) -> dict:
    content = content.strip()
    if content.startswith("```"):
        content = re.sub(r"^```(?:json)?\s*", "", content)
        content = re.sub(r"\s*```$", "", content)
    try:
        return json.loads(content)
    except json.JSONDecodeError:
        match = re.search(r"\{.*\}", content, re.DOTALL)
        if not match:
            raise
        return json.loads(match.group(0))


def judge_answer(
    client: httpx.Client,
    config: JudgeConfig,
    question: str,
    reference: str,
    context: str,
    answer: str,
) -> dict:
    user = (
        f"问题：{question}\n\n"
        f"参考答案：{reference or '无参考答案，请根据资料判断'}\n\n"
        f"资料：\n{context}\n\n"
        f"回答：\n{answer}"
    )
    resp = client.post(
        f"{config.base_url.rstrip('/')}/chat/completions",
        headers={"Authorization": f"Bearer {config.api_key}"},
        json={
            "model": config.model,
            "temperature": 0,
            "max_tokens": 600,
            "messages": [
                {"role": "system", "content": SYSTEM_PROMPT},
                {"role": "user", "content": user},
            ],
        },
        timeout=config.timeout,
    )
    resp.raise_for_status()
    payload = resp.json()
    content = payload["choices"][0]["message"]["content"]
    result = _parse_json(content)
    result["prompt_version"] = JUDGE_PROMPT_VERSION
    result["model"] = config.model
    return result


def summarize(rows: list[dict]) -> dict:
    scored = [row for row in rows if not row.get("error")]
    if not scored:
        return {"cases": len(rows), "scored": 0, "errors": len(rows)}
    keys = ("correctness", "faithfulness", "completeness", "citation_support")
    summary = {
        "cases": len(rows),
        "scored": len(scored),
        "errors": len(rows) - len(scored),
        "pass": sum(1 for row in scored if row.get("verdict") == "pass"),
        "model": scored[0].get("model", ""),
        "prompt_version": scored[0].get("prompt_version", ""),
    }
    for key in keys:
        values = [float(row[key]) for row in scored if isinstance(row.get(key), (int, float))]
        summary[key] = (sum(values) / len(values)) if values else 0.0
    summary["pass_rate"] = summary["pass"] / len(scored) if scored else 0.0
    return summary


def read_labels(path: Path) -> dict[str, str]:
    labels: dict[str, str] = {}
    with path.open("r", encoding="utf-8-sig", newline="") as fh:
        for row in csv.DictReader(fh):
            case_id = (row.get("id") or "").strip()
            verdict = (row.get("verdict") or "").strip().lower()
            if case_id and verdict in {"pass", "fail"}:
                labels[case_id] = verdict
    return labels


def agreement(rows: list[dict], labels: dict[str, str]) -> dict:
    pairs = [
        (str(row.get("verdict", "")).lower(), labels[row["id"]])
        for row in rows
        if row.get("id") in labels and row.get("verdict") in {"pass", "fail"}
    ]
    if not pairs:
        return {"n": 0, "agreement": 0.0, "kappa": 0.0}
    total = len(pairs)
    observed = sum(1 for judge_verdict, human_verdict in pairs if judge_verdict == human_verdict) / total
    expected = 0.0
    for label in ("pass", "fail"):
        judge_rate = sum(1 for judge_verdict, _ in pairs if judge_verdict == label) / total
        human_rate = sum(1 for _, human_verdict in pairs if human_verdict == label) / total
        expected += judge_rate * human_rate
    kappa = (observed - expected) / (1 - expected) if expected != 1 else 0.0
    return {"n": total, "agreement": observed, "kappa": kappa}


def write_review(path: Path, rows: list[dict]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    columns = [
        "id", "question", "verdict", "correctness", "faithfulness",
        "completeness", "citation_support", "reason", "answer", "error",
    ]
    with path.open("w", encoding="utf-8-sig", newline="") as fh:
        writer = csv.DictWriter(fh, fieldnames=columns)
        writer.writeheader()
        for row in rows:
            writer.writerow({column: row.get(column, "") for column in columns})
