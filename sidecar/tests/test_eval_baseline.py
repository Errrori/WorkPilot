import unittest

from app import eval as runner
from app import eval_baseline
from app import judge


class BM25Test(unittest.TestCase):
    def setUp(self):
        self.docs = [
            ("a", "苹果香蕉是水果"),
            ("b", "测试环境数据库容量不足"),
            ("c", "短信通道失败后改用邮件通知"),
        ]
        self.index = eval_baseline.BM25Index(self.docs)

    def test_exact_terms_rank_first(self):
        self.assertEqual(self.index.search("数据库容量", 1), ["b"])
        self.assertEqual(self.index.search("邮件通知", 1), ["c"])

    def test_empty_query(self):
        self.assertEqual(self.index.search("", 3), [])
        self.assertEqual(self.index.search("不存在", 3), [])


class SpanAndCitationTest(unittest.TestCase):
    def setUp(self):
        self.case = runner.Case(
            id="s1",
            question="灰度是哪天？",
            expected_files=["release.md"],
            required_files=["release.md"],
            gold_spans=["10 月 23 日：灰度发布"],
            answer_keywords=["10 月 23 日"],
        )

    def test_span_metrics_use_full_content(self):
        result = runner.CaseResult(
            case=self.case,
            sources=[{"file_name": "release.md", "content": "10 月 23 日：灰度发布，先开放。"}],
            answer="10 月 23 日灰度发布 [1]。",
            latency_ms=1,
            mode="qa",
        )
        self.assertTrue(result.span_hit)
        self.assertTrue(result.all_spans)
        self.assertEqual(result.span_rank, 1)
        self.assertEqual(result.span_ndcg, 1.0)

    def test_citation_support_checks_the_cited_source(self):
        good = runner.CaseResult(
            case=self.case,
            sources=[{"file_name": "release.md", "content": "10 月 23 日：灰度发布，先开放。"}],
            answer="10 月 23 日灰度发布 [1]。",
            latency_ms=1,
            mode="qa",
        )
        bad = runner.CaseResult(
            case=self.case,
            sources=[{"file_name": "other.md", "content": "无关内容"}],
            answer="10 月 23 日灰度发布 [1]。",
            latency_ms=1,
            mode="qa",
        )
        self.assertEqual(good.citation_support, (1, 1))
        self.assertTrue(good.citation_supported)
        self.assertEqual(bad.citation_support, (0, 1))
        self.assertFalse(bad.citation_supported)


class RefusalTest(unittest.TestCase):
    def test_out_of_scope_is_handled_not_refused(self):
        case = runner.Case(id="r1", question="移动端什么时候上线？", answerable=False, refusal_type="out_of_scope")
        result = runner.CaseResult(
            case=case,
            sources=[{"file_name": "release.md", "content": "本期不包含移动端。"}],
            answer="本期不包含移动端 [1]。",
            latency_ms=1,
            mode="qa",
        )
        self.assertTrue(result.scope_handled)
        self.assertTrue(result.handled)
        self.assertTrue(result.passed)

    def test_clean_refusal_rejects_fabricated_number(self):
        case = runner.Case(id="r2", question="Kubernetes 部署？", answerable=False, refusal_type="not_in_corpus")
        clean = runner.CaseResult(case=case, sources=[], answer="资料未提及 Kubernetes 部署。", latency_ms=1, mode="qa")
        dirty = runner.CaseResult(case=case, sources=[], answer="资料未提及，但据经验 10 月上线。", latency_ms=1, mode="qa")
        self.assertTrue(clean.refused_clean)
        self.assertFalse(dirty.refused_clean)


class JudgeParseTest(unittest.TestCase):
    def test_parse_fenced_json(self):
        result = judge._parse_json('```json\n{"correctness": 5, "verdict": "pass"}\n```')
        self.assertEqual(result["correctness"], 5)
        self.assertEqual(result["verdict"], "pass")

    def test_human_agreement(self):
        rows = [{"id": "a", "verdict": "pass"}, {"id": "b", "verdict": "fail"}]
        labels = {"a": "pass", "b": "pass"}
        result = judge.agreement(rows, labels)
        self.assertEqual(result["n"], 2)
        self.assertEqual(result["agreement"], 0.5)
        self.assertLess(result["kappa"], 0.1)


if __name__ == "__main__":
    unittest.main()
