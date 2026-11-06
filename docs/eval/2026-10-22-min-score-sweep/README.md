# min_score 阈值扫描与问答 A/B（2026-10-22）


## 评测条件

- 代码：`9aa245b`
- 数据集：`sidecar/eval/datasets/bench-test.jsonl`，SHA-256 `59e742d7ed184279df4e3ce7f2dfe6252f459ed390f5eb0f9f64a2db62c6515e`
- 语料：`sidecar/eval/corpus/bench/`，27 文件 / 31 chunks，SHA-256 `08395bfc0ced5c450c650903e56c7fbbaaf184002e8e8db0eb2d0076e912643b`
- 题量：110 题（104 可答，6 不可答）
- 检索参数：`top_k=6`、`RETRIEVAL_CANDIDATES=30`、`HYBRID_RRF_K=60`、dense/sparse 权重 1、`RETRIEVAL_MODE=hybrid`
- 模型：Embedding `bge-m3`（Ollama）；LLM `deepseek-chat`；judge `deepseek-chat`（prompt_version `2026-10-20`）
- 环境：Postgres 5433 / Redis 6380 / Ollama 11434
- 运行：retrieval-only 七档阈值在同一群组内执行；问答两臂在同一群组内执行，各 1 轮，避免重复索引
- 请求级覆盖：`/search`、`/ask` 传 `min_score`；`min_score=0` 表示关闭过滤

## 1. retrieval-only 扫描

| min_score | File Hit@6 | File Recall@6 | MRR | AllRequired@6 | Span Hit@6 | Span Recall@6 | nDCG@6 | 可答被拦 | 不可答仍有召回 | P50 / P95 |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| 0.00 | 100.0% | 98.1% | 0.824 | 97.1% | 100.0% | 98.1% | 0.870 | 0/104 | 6/6 | 114 / 162 ms |
| 0.30 | 100.0% | 98.1% | 0.824 | 97.1% | 100.0% | 98.1% | 0.870 | 0/104 | 6/6 | 119 / 160 ms |
| 0.36 | 100.0% | 98.1% | 0.824 | 97.1% | 100.0% | 98.1% | 0.870 | 0/104 | 6/6 | 116 / 162 ms |
| **0.42** | 100.0% | 97.8% | 0.824 | 96.2% | 100.0% | 97.8% | 0.871 | 0/104 | 6/6 | 114 / 159 ms |
| 0.48 | 96.2% | 95.5% | 0.803 | 95.2% | 96.2% | 95.5% | 0.844 | 1/104 | 6/6 | 116 / 158 ms |
| 0.54 | 92.3% | 91.3% | 0.787 | 90.4% | 92.3% | 91.2% | 0.823 | 2/104 | 4/6 | 116 / 165 ms |
| 0.60 | 71.2% | 70.2% | 0.620 | 69.2% | 71.2% | 70.2% | 0.644 | 19/104 | 4/6 | 113 / 161 ms |


## 2. 问答 A/B（0.36 vs 0.42）

| 指标 | 0.36 | 0.42 |
|---|---:|---:|
| 严格通过（110 题） | 88/110（80.0%） | 88/110（80.0%） |
| 检索通过 | 97.1% | 96.2% |
| 答案通过 | 95.2% | 94.2% |
| 引用通过 | 81.7% | **83.7%** |
| Claim 支持率 | 91.0% | **92.7%** |
| 全部 claim 有据 | 77.9% | **80.8%** |
| 不可答处理 | 6/6 | 6/6 |
| Judge 通过 | 29/30（96.7%） | **30/30（100%）** |
| 延迟 P50 / P95 | 1226 / 1813 ms | 1218 / 1826 ms |

## 3. 结论

- 0.42 保留为默认值：处于召回平台区，引用层不劣
- 阈值收益体现在上下文噪声与引用质量，不体现在通过率或拒答；重定阈值应多轮复跑，并按 query 类型或分数分位数自适应。

## 复现

```powershell
cd sidecar

# retrieval-only：每档阈值单独执行（新建群组并重新索引）
.\.venv\Scripts\python -m app.eval --dataset eval\datasets\bench-test.jsonl --fixtures eval\corpus\bench --split benchmark --retrieval-only --top-k 6 --min-score 0.36 --no-baseline --tag ms036
# 0.00 / 0.30 / 0.42 / 0.48 / 0.54 / 0.60 同理

# 问答 A/B：两臂各 1 轮，judge 抽检 30 条
.\.venv\Scripts\python -m app.eval --dataset eval\datasets\bench-test.jsonl --fixtures eval\corpus\bench --split benchmark --top-k 6 --min-score 0.36 --judge --judge-limit 30 --tag qa-ms036
.\.venv\Scripts\python -m app.eval --dataset eval\datasets\bench-test.jsonl --fixtures eval\corpus\bench --split benchmark --top-k 6 --min-score 0.42 --judge --judge-limit 30 --tag qa-ms042
```

本次扫描在同一群组内复用 `app.eval` 的 `run()` / `summarize()` 逐档执行；检索为确定性，独立建群复跑指标应一致。

## 文件

- `retrieval-sweep.json`：七档阈值的检索指标原始值（命中、召回、MRR、AllRequired、nDCG、被拦、不可答召回、延迟）
- `qa-ab-ms036-ms042.json`：两臂问答汇总与 judge 汇总
- `judge-review-ms042.csv`：0.42 臂 30 条 judge 抽检明细（0.36 臂仅保留汇总）
