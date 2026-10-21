import unittest

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
        self.assertEqual(summary["passed"], 3)
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


if __name__ == "__main__":
    unittest.main()
