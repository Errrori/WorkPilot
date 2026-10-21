import unittest

from app import eval as runner
from app import eval_lint

ALIASES = eval_lint.parse_aliases("星河=xinghe,天穹=tianqiong")
LONG = "这是一个足够长的段落，用来检测跨文件重复内容，不应该出现在其他项目文档里。"


def case(**kwargs):
    base = dict(
        id="c1",
        question="星河项目的目标是什么？",
        expected_files=["xinghe-brief.md"],
        required_files=["xinghe-brief.md"],
        split="dev",
    )
    base.update(kwargs)
    return runner.Case(**base)


def lint(cases, texts, **kwargs):
    options = dict(
        max_template=10,
        min_unanswerable=0,
        min_refusal_types=0,
        aliases=ALIASES,
    )
    options.update(kwargs)
    return eval_lint.lint_dataset(
        cases,
        list(texts),
        texts,
        **options,
    )


class ParagraphLintTest(unittest.TestCase):
    def test_duplicate_paragraph_across_files(self):
        result = lint(
            [case()],
            {
                "xinghe-brief.md": LONG,
                "tianqiong-brief.md": LONG,
            },
        )
        self.assertEqual(result["metrics"]["duplicate_paragraphs"], 1)
        self.assertIn("duplicate_paragraph", result["error_counts"])

    def test_short_repeated_lines_are_ignored(self):
        result = lint(
            [case()],
            {
                "xinghe-brief.md": "标题\n\n短内容",
                "tianqiong-brief.md": "标题\n\n短内容",
            },
        )
        self.assertNotIn("duplicate_paragraph", result["error_counts"])


class TemplateLintTest(unittest.TestCase):
    def test_dev_test_template_overlap(self):
        result = lint(
            [
                case(id="d1", question="星河项目的目标是什么？", split="dev"),
                case(
                    id="t1",
                    question="天穹项目的目标是什么？",
                    expected_files=["tianqiong-brief.md"],
                    required_files=["tianqiong-brief.md"],
                    split="test",
                ),
            ],
            {"xinghe-brief.md": "A", "tianqiong-brief.md": "B"},
        )
        self.assertIn("dev_test_template_overlap", result["error_counts"])
        self.assertEqual(result["metrics"]["templates"], 1)

    def test_template_reuse_limit(self):
        cases = [
            case(id=f"d{i}", question="星河项目的目标是什么？", split="dev")
            for i in range(3)
        ]
        result = lint(cases, {"xinghe-brief.md": "A"}, max_template=2)
        self.assertIn("template_reused", result["error_counts"])

    def test_duplicate_question(self):
        cases = [case(id="a"), case(id="b")]
        result = lint(cases, {"xinghe-brief.md": "A"})
        self.assertIn("duplicate_question", result["error_counts"])


class LabelLintTest(unittest.TestCase):
    def test_answer_leakage_in_question(self):
        result = lint(
            [
                case(
                    id="leak",
                    question="星河项目上线前完成 30 条回归是多少？",
                    expected_answers=["30 条"],
                    answer_keywords=["30"],
                )
            ],
            {"xinghe-brief.md": "A"},
        )
        self.assertIn("answer_leakage", result["error_counts"])

    def test_required_file_over_top_k(self):
        result = lint(
            [
                case(
                    required_files=["a.md", "b.md"],
                    expected_files=["a.md", "b.md"],
                )
            ],
            {"a.md": "A", "b.md": "B"},
            top_k=1,
        )
        self.assertIn("required_gt_top_k", result["error_counts"])

    def test_answerable_does_not_require_refusal_coverage(self):
        result = lint([case()], {"xinghe-brief.md": "A"})
        self.assertNotIn("unanswerable_too_few", result["error_counts"])


class SpanLintTest(unittest.TestCase):
    def test_ambiguous_span_outside_evidence(self):
        result = lint(
            [
                case(
                    gold_spans=["10 月 23 日灰度发布"],
                    expected_files=["a.md"],
                    required_files=["a.md"],
                )
            ],
            {
                "a.md": "10 月 23 日灰度发布",
                "b.md": "10 月 23 日灰度发布",
            },
        )
        self.assertIn("span_ambiguous", result["error_counts"])

    def test_acceptable_file_resolves_ambiguity(self):
        result = lint(
            [
                case(
                    gold_spans=["10 月 23 日灰度发布"],
                    expected_files=["a.md"],
                    required_files=["a.md"],
                    acceptable_files=["b.md"],
                )
            ],
            {
                "a.md": "10 月 23 日灰度发布",
                "b.md": "10 月 23 日灰度发布",
            },
        )
        self.assertNotIn("span_ambiguous", result["error_counts"])

    def test_missing_span(self):
        result = lint(
            [case(gold_spans=["不存在的句子"])],
            {"xinghe-brief.md": "别的资料"},
        )
        self.assertIn("span_missing", result["error_counts"])

    def test_required_file_without_span(self):
        result = lint(
            [
                case(
                    gold_spans=["10 月 23 日灰度发布"],
                    expected_files=["a.md", "b.md"],
                    required_files=["a.md", "b.md"],
                )
            ],
            {
                "a.md": "10 月 23 日灰度发布",
                "b.md": "无关内容",
            },
        )
        self.assertIn("required_file_without_span", result["error_counts"])


class RefusalLintTest(unittest.TestCase):
    def test_refusal_coverage_gate(self):
        result = eval_lint.lint_dataset(
            [case(answerable=False, refusal_type="not_in_corpus")],
            ["xinghe-brief.md"],
            {"xinghe-brief.md": "A"},
            max_template=10,
            min_unanswerable=2,
            min_refusal_types=2,
            aliases=ALIASES,
        )
        self.assertIn("unanswerable_too_few", result["error_counts"])
        self.assertIn("refusal_types_too_few", result["error_counts"])


if __name__ == "__main__":
    unittest.main()
