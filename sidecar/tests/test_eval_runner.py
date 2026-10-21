import json
import unittest

import httpx

from app import eval as runner


def make_cases():
    return [
        runner.Case(
            id="q1",
            question="Q1",
            expected_files=["a.md"],
            required_files=["a.md"],
            answer_keywords=["10 月"],
            tags=["time"],
        ),
        runner.Case(
            id="q2",
            question="Q2",
            expected_files=["a.md", "b.md"],
            required_files=["a.md", "b.md"],
            answer_keywords=["x"],
            tags=["multi"],
        ),
        runner.Case(
            id="q3",
            question="Q3",
            answerable=False,
            refusal_type="not_in_corpus",
            tags=["refusal"],
        ),
    ]


def make_results():
    cases = make_cases()
    return [
        runner.CaseResult(
            case=cases[0],
            sources=[{"file_name": "a.md", "snippet": "计划在 10 月交付。"}],
            answer="计划在 10 月交付 [1]。",
            latency_ms=10,
            mode="qa",
        ),
        runner.CaseResult(
            case=cases[1],
            sources=[{"file_name": "a.md", "snippet": "x。"}],
            answer="x [1]。",
            latency_ms=20,
            mode="qa",
        ),
        runner.CaseResult(
            case=cases[2],
            sources=[],
            answer="资料未提及，无法回答。",
            latency_ms=30,
            mode="qa",
        ),
    ]


class SummarizeTest(unittest.TestCase):
    def test_qa_summary_separates_hit_and_all_required(self):
        summary = runner.summarize(make_results(), "qa")
        self.assertEqual(summary["cases"], 3)
        self.assertEqual(summary["hits"], 2)
        self.assertEqual(summary["all_required_ok"], 1)
        self.assertAlmostEqual(summary["recall_rate"], 0.75)
        self.assertEqual(summary["keyword_hits"], 2)
        self.assertEqual(summary["keyword_total"], 2)
        self.assertEqual(summary["citation_ok"], 2)
        self.assertEqual(summary["citation_hit_ok"], 2)
        self.assertEqual(summary["refusal_ok"], 1)
        self.assertEqual(summary["false_refusal"], 0)
        self.assertEqual(summary["passed"], 2)
        self.assertEqual(summary["retrieval_passed"], 1)
        self.assertEqual(summary["answer_passed"], 2)
        self.assertEqual(summary["citation_passed"], 2)
        self.assertIn("time", summary["by_tag"])
        self.assertIn("multi", summary["by_tag"])

    def test_retrieval_summary_excludes_answer_metrics(self):
        results = make_results()
        retrieval = [
            runner.CaseResult(case=results[0].case, sources=results[0].sources, answer="", latency_ms=10, mode="retrieval"),
            runner.CaseResult(case=results[2].case, sources=[], answer="", latency_ms=10, mode="retrieval"),
        ]
        summary = runner.summarize(retrieval, "retrieval")
        self.assertEqual(summary["scorable"], 1)
        self.assertEqual(summary["passed"], 1)
        self.assertNotIn("keyword_rate", summary)
        self.assertNotIn("citation_rate", summary)

    def test_stability_reports_mean_and_spread(self):
        first = runner.summarize(make_results(), "qa")
        second = runner.summarize(make_results(), "qa")
        second["hit_rate"] = 0.5
        stability = runner.aggregate_stability([first, second])
        self.assertEqual(stability["hit_rate"]["mean"], 0.75)
        self.assertEqual(stability["hit_rate"]["min"], 0.5)
        self.assertEqual(stability["hit_rate"]["max"], 1.0)

    def test_reasoning_breakdown(self):
        case = runner.Case(
            id="r1",
            question="Q",
            expected_files=["a.md"],
            required_files=["a.md"],
            gold_spans=["A"],
            answer_keywords=["1"],
            reasoning_type="sum",
            tags=["reasoning"],
        )
        result = runner.CaseResult(
            case=case,
            sources=[{"file_name": "a.md", "content": "A"}],
            answer="1 [1]",
            latency_ms=1,
            mode="qa",
        )
        summary = runner.summarize([result], "qa")
        self.assertIn("sum", summary["by_reasoning"])
        self.assertEqual(summary["by_reasoning"]["sum"]["cases"], 1)

    def test_impossible_cases_are_excluded_from_pass_rate(self):
        case = runner.Case(
            id="i1",
            question="Q",
            expected_files=["a.md", "b.md"],
            required_files=["a.md", "b.md"],
            answer_keywords=["x"],
        )
        result = runner.CaseResult(
            case=case,
            sources=[{"file_name": "a.md", "content": "x"}],
            answer="x [1]",
            latency_ms=1,
            mode="retrieval",
            top_k=1,
        )
        summary = runner.summarize([result], "retrieval")
        self.assertEqual(summary["impossible_cases"], 1)
        self.assertEqual(summary["scorable"], 0)
        self.assertFalse(result.passed)
        self.assertEqual(result.failure_reason, "impossible_k_lt_required")

    def test_failure_reason_distinguishes_wrong_project(self):
        case = runner.Case(
            id="w1",
            question="星河项目的目标？",
            expected_files=["xinghe-brief.md"],
            required_files=["xinghe-brief.md"],
        )
        result = runner.CaseResult(
            case=case,
            sources=[{"file_name": "tianqiong-brief.md", "content": "别的项目"}],
            answer="",
            latency_ms=1,
            mode="retrieval",
            top_k=3,
        )
        self.assertEqual(result.failure_reason, "retrieval_wrong_project")

    def test_failure_reason_uses_wrong_doc_for_different_sections(self):
        case = runner.Case(
            id="w2",
            question="发布计划？",
            expected_files=["release-plan.md"],
            required_files=["release-plan.md"],
        )
        result = runner.CaseResult(
            case=case,
            sources=[{"file_name": "project-brief.md", "content": "别的章节"}],
            answer="",
            latency_ms=1,
            mode="retrieval",
            top_k=1,
        )
        self.assertEqual(result.failure_reason, "retrieval_wrong_doc")

    def test_ambiguous_label_when_evidence_lives_in_another_file(self):
        case = runner.Case(
            id="a1",
            question="周会风险？",
            expected_files=["xinghe-weekly.md"],
            required_files=["xinghe-weekly.md"],
            gold_spans=["短信通道审批未完成"],
        )
        result = runner.CaseResult(
            case=case,
            sources=[
                {"file_name": "xinghe-risks.md", "content": "第三方短信通道审批未完成"},
                {"file_name": "xinghe-brief.md", "content": "无关"},
            ],
            answer="",
            latency_ms=1,
            mode="retrieval",
            top_k=3,
        )
        self.assertEqual(result.failure_reason, "retrieval_ambiguous_label")


class BaselineComparisonTest(unittest.TestCase):
    def test_paired_bootstrap_is_deterministic(self):
        first = runner.paired_bootstrap([1, 1, 0], [0, 1, 0])
        second = runner.paired_bootstrap([1, 1, 0], [0, 1, 0])
        self.assertEqual(first, second)
        self.assertAlmostEqual(first["mean_diff"], 1 / 3)
        self.assertEqual(first["wins"], 1)
        self.assertEqual(first["ties"], 2)

    def test_compare_retrieval_uses_same_cases(self):
        case = runner.Case(
            id="c1", question="Q", expected_files=["a.md"], required_files=["a.md"],
            gold_spans=["A"],
        )
        rag = runner.CaseResult(
            case=case, sources=[{"file_name": "a.md", "content": "A"}],
            answer="", latency_ms=1, mode="retrieval", top_k=1,
        )
        baseline = runner.CaseResult(
            case=case, sources=[{"file_name": "b.md", "content": "B"}],
            answer="", latency_ms=0, mode="retrieval", top_k=1,
        )
        comparison = runner.compare_retrieval([rag], [baseline])
        self.assertEqual(comparison["n"], 1)
        self.assertEqual(comparison["file_hit"]["mean_diff"], 1.0)
        self.assertEqual(comparison["span_hit"]["mean_diff"], 1.0)


class HttpPayloadTest(unittest.TestCase):
    def test_search_sends_explicit_overrides(self):
        seen = {}

        def handler(request: httpx.Request) -> httpx.Response:
            seen.update(json.loads(request.content))
            return httpx.Response(200, json={"sources": []})

        with httpx.Client(transport=httpx.MockTransport(handler), base_url="http://test") as client:
            runner.search(client, "group-1", "q", 4, 0.0)
        self.assertEqual(seen, {"query": "q", "top_k": 4, "min_score": 0.0})

    def test_ask_sends_explicit_overrides(self):
        seen = {}
        body = (
            'event:sources\ndata:{"sources":[]}\n\n'
            'event:delta\ndata:{"text":"ok"}\n\n'
            'event:done\ndata:{}\n\n'
        )

        def handler(request: httpx.Request) -> httpx.Response:
            seen.update(json.loads(request.content))
            return httpx.Response(200, content=body.encode(), headers={"content-type": "text/event-stream"})

        with httpx.Client(transport=httpx.MockTransport(handler), base_url="http://test") as client:
            sources, answer = runner.ask(client, "group-1", "alice", "q", 10, 3, 0.0)
        self.assertEqual(seen, {"user": "alice", "question": "q", "top_k": 3, "min_score": 0.0})
        self.assertEqual(sources, [])
        self.assertEqual(answer, "ok")


class OracleTest(unittest.TestCase):
    def test_oracle_detects_missing_span(self):
        case = runner.Case(
            id="o1", question="Q", expected_files=["a.md"], required_files=["a.md"],
            gold_spans=["不存在的句子"],
        )
        records = [{"file_name": "a.md", "chunk_index": 0, "content": "A"}]
        oracle = runner.corpus_oracle([case], records)
        self.assertEqual(oracle["summary"]["all_required_rate"], 1.0)
        self.assertEqual(oracle["summary"]["span_recall_rate"], 0.0)
        self.assertFalse(oracle["by_id"]["o1"]["all_spans"])


if __name__ == "__main__":
    unittest.main()
