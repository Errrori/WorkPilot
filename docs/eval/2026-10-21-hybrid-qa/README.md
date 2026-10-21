# 混合召回问答 A/B 评测（2026-10-21）

本目录保存 M15 混合召回的离线问答评测证据。结论：检索层指标稳定提升；严格问答通过率受 claim 级引用支持影响下降，不能据此宣称端到端问答提升。

注意：本轮使用语料去重后的 31 chunks（`9ca870e`），与 README 中 45 chunks 的历史检索评测不可直接横比。

## 评测条件

- 代码：`9ca870e`（含语料去重与标签修复），工作区干净
- 数据集：`sidecar/eval/datasets/bench-test.jsonl`，SHA-256 `59e742d7ed184279df4e3ce7f2dfe6252f459ed390f5eb0f9f64a2db62c6515e`
- 语料：`sidecar/eval/corpus/bench/`，27 文件 / 31 chunks，SHA-256 `08395bfc0ced5c450c650903e56c7fbbaaf184002e8e8db0eb2d0076e912643b`
- 题量：110 题（104 可答，6 不可答）
- 参数：`top_k=6`、`min_score=0.42`、`RETRIEVAL_CANDIDATES=30`、`RRF k=60`、dense/sparse 权重 1
- 模型：Embedding `bge-m3`（Ollama），LLM `deepseek-chat`
- 运行方式：同一数据集与语料，只切换 `RETRIEVAL_MODE=dense|hybrid`，各跑 1 轮；hybrid 额外跑 30 条 LLM judge
- 统计：paired bootstrap，2000 轮，seed `20261021`

```powershell
# 服务端 RETRIEVAL_MODE=hybrid
.\.venv\Scripts\python -m app.eval `
  --dataset eval\datasets\bench-test.jsonl --fixtures eval\corpus\bench `
  --split benchmark --top-k 6 --min-score 0.42 `
  --judge --judge-limit 30 --tag hybrid-qa-ms042

# 服务端 RETRIEVAL_MODE=dense
.\.venv\Scripts\python -m app.eval `
  --dataset eval\datasets\bench-test.jsonl --fixtures eval\corpus\bench `
  --split benchmark --top-k 6 --min-score 0.42 --tag dense-qa-ms042
```

## 检索指标

| 指标 | dense | hybrid | 差值 | 95% CI |
|---|---:|---:|---:|---:|
| File Hit@6 | 98.1% | **100.0%** | +1.9pp | [0, +4.8] |
| File Recall@6 | 95.4% | **97.8%** | +2.4pp | [+0.5, +5.0] |
| MRR | 0.756 | **0.824** | +0.068 | - |
| AllRequired@6 | 93.3% | **96.2%** | +2.9pp | [0, +6.7] |
| Span Hit@6 | 98.1% | **100.0%** | +1.9pp | [0, +4.8] |
| Span Recall@6 | 95.3% | **97.8%** | +2.5pp | [+0.5, +5.1] |
| Span nDCG@6 | 0.813 | **0.871** | +0.058 | [+0.026, +0.091] |

## 问答与引用指标

| 指标 | dense | hybrid | 差值 | 95% CI |
|---|---:|---:|---:|---:|
| 严格通过（110 题） | **85.5%（94/110）** | 78.2%（86/110） | -7.3pp | [-14.5, 0] |
| 检索通过 | 93.3% | **96.2%** | +2.7pp | [0, +6.4] |
| 答案通过 | 94.2% | 94.2% | 0 | [-3.6, +3.6] |
| 引用通过 | **88.5%** | 81.7% | -6.4pp | [-14.5, +0.9] |
| 引用命中期望文件 | 97.1% | **99.0%** | +1.9pp | [0, +4.8] |
| 引用支持率 | 97.1% | **99.0%** | +1.9pp | - |
| 引用精确率 | **69.5%** | 68.4% | -1.1pp | - |
| Claim 支持率 | **92.7%（290/313）** | 90.3%（288/319） | -2.4pp | - |
| 全部 claim 有据 | **84.6%（88/104）** | 76.9%（80/104） | -7.3pp | [-15.5, +0.9] |
| 关键词命中 | **96.0%** | 94.4% | -1.6pp | - |
| 精确答案 | 14/19（73.7%） | **15/19（78.9%）** | +5.3pp | - |
| 引用格式有效 | 100% | 100% | 0 | - |
| 不可答处理 | 6/6 | 6/6 | 0 | - |
| 误拒答 | 4/104（3.8%） | 4/104（3.8%） | 0 | - |
| 延迟 P50 / P95 | 1191 / 2098 ms | 1300 / 1979 ms | +109 / -119 ms | - |
| LLM tokens | 205,703 | 218,104 | +12,401 | - |

hybrid 的 LLM judge：29/30 pass，correctness 4.87、faithfulness 4.97、completeness 4.83、citation support 4.90。dense 未跑 judge，不能直接比较 judge 通过率。

## 解读

- 混合召回提升了检索覆盖与排序：File Recall +2.4pp、AllRequired +2.9pp、Span nDCG +0.058，其中 Recall 和 nDCG 的配对 bootstrap CI 不含 0。
- 检索提升没有传导到严格问答通过率：strict pass 从 85.5% 降到 78.2%。主要失败项是 `citation_claim`（dense 6 次，hybrid 17 次）；hybrid 回答产生的 claim 更多（319 vs 313），claim 级支持率略低，全部 claim 有据从 84.6% 降到 76.9%。
- 因此不能写“问答准确率提升”。下一步应优先优化回答长度与 claim 级引用约束，或做 rerank/父子块，而不是继续堆检索召回。
- 这是单轮结果，LLM 生成有随机性；strict pass 的 CI 下界虽然为 0 附近，但赢/输为 5/13，需多轮复跑后再下结论。检索指标是确定性的，可作为本模块的主要收益证据。

## 文件

- `dense-qa-ms042.md`：dense 基线完整报告（逐题明细）
- `hybrid-qa-ms042.md`：hybrid 完整报告（逐题明细，含 judge 摘要）
- `judge-review.csv`：hybrid 的 30 条 LLM judge 人工抽检表
- `paired-bootstrap.md`：配对 bootstrap 明细与原始数值
