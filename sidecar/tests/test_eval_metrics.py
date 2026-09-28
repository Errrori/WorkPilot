import unittest

from app import eval_metrics as m


class NormalizeTest(unittest.TestCase):
    def test_strips_whitespace_and_case(self):
        self.assertEqual(m.normalize("10 月 23 日 PDF"), "10月23日pdf")


class RetrievalMetricsTest(unittest.TestCase):
    def setUp(self):
        self.sources = [
            {"file_name": "release-plan.md", "chunk_index": 0, "score": 0.9},
            {"file_name": "weekly-meeting.md", "chunk_index": 2, "score": 0.8},
        ]

    def test_hit_at_k(self):
        self.assertTrue(m.hit_at_k(self.sources, ["weekly-meeting.md"]))
        self.assertFalse(m.hit_at_k(self.sources, ["project-brief.md"]))
        self.assertFalse(m.hit_at_k(self.sources, []))

    def test_hit_at_k_is_case_insensitive(self):
        self.assertTrue(m.hit_at_k([{"file_name": "Weekly-Meeting.md"}], ["weekly-meeting.md"]))

    def test_first_hit_rank(self):
        self.assertEqual(m.first_hit_rank(self.sources, ["weekly-meeting.md"]), 2)
        self.assertEqual(m.first_hit_rank(self.sources, ["project-brief.md"]), 0)
        self.assertEqual(m.first_hit_rank(self.sources, []), 0)


class AnswerMetricsTest(unittest.TestCase):
    def test_keyword_hits(self):
        answer = "根据 [1]，计划在 10 月 23 日灰度发布。"
        self.assertEqual(m.keyword_hits(answer, ["10月23日", "灰度"]), (2, 2))
        self.assertEqual(m.keyword_hits(answer, ["10月23日", "回滚"]), (1, 2))
        self.assertEqual(m.keyword_hits(answer, []), (0, 0))

    def test_citation_valid(self):
        self.assertTrue(m.citation_valid("结论来自 [1] 和 [2]。", 2))
        self.assertFalse(m.citation_valid("结论来自 [3]。", 2))
        self.assertFalse(m.citation_valid("没有任何引用", 2))
        self.assertFalse(m.citation_valid("[1]", 0))

    def test_refused(self):
        self.assertTrue(m.refused("群内暂时没有可检索的已索引资料，无法回答这个问题。", 0))
        self.assertFalse(m.refused("资料显示计划在十月上线 [1]。", 1))
        self.assertFalse(m.refused("", 0))


class PercentileTest(unittest.TestCase):
    def test_percentile(self):
        values = [10, 20, 30, 40]
        self.assertEqual(m.percentile(values, 0.5), 25)
        self.assertEqual(m.percentile(values, 0.95), 38.5)
        self.assertEqual(m.percentile([7], 0.95), 7)
        self.assertEqual(m.percentile([], 0.5), 0.0)


if __name__ == "__main__":
    unittest.main()
