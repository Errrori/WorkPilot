# WorkPilot

面向小型研发团队的“项目进度智能助手”：团队在群聊中讨论需求、上传文档（PRD、设计稿、会议纪要），AI 读取这些资料，梳理当前进度、抽取成为任务建议，并支持自定义 AI 任务（如“每周五自动汇总本周进展并生成周报”）。


## 技术栈

| 层 | 选型 |
|---|---|
| 群聊 / WebSocket / 文件空间 | Go + Gin + gorilla/websocket |
| RAG 管道 | Eino + pgvector（Postgres） |
| AI 工作流编排 | 自研拓扑序执行器（线性 DAG；Eino ChatModel / pgvector Retriever 仅作为节点组件） |
| 缓存 / 队列 | Redis（asynq：定时 AI 任务与周报） |
| 文档解析 / 离线评测 | Python sidecar（FastAPI + markitdown） |

产品范围与选型理由见 `docs/PRD.md`。

## 架构

```text
Gin（REST + WebSocket）
 ├─ internal/httpapi   REST 路由与 healthz
 ├─ internal/ws        WebSocket Hub（单实例内存广播）
 ├─ internal/store     Postgres（pgx）+ SQL 迁移
 ├─ internal/storage   上传文件本地磁盘存储
 ├─ internal/parser    文档解析（sidecar 客户端 + 异步 worker）
 ├─ internal/rag       RAG 管道（Markdown 分块 + Ollama Embedding + 自写 pgvector Indexer/Retriever）
 ├─ internal/qa        带引用问答（个人会话 + 多轮历史/检索改写 + OpenAI 兼容 ChatModel 流式 + citations 落库）
 ├─ internal/tasks     AI 任务抽取（素材聚合 → JSON 解析 → 建议落库/去重）
 ├─ internal/aitasks   自定义 AI 任务与定时周报（asynq 扫描/执行 + cron + 报告落库）
 ├─ internal/llmtrack  LLM 用量采集（装饰 ChatModel，记录 tokens/延迟/失败 → llm_usage）
  ├─ internal/workflow   AI 工作流编排（线性 DAG：检索/问答/抽取/报告节点 + asynq 调度 + 运行记录）
  ├─ internal/logging   slog 初始化（LOG_LEVEL / LOG_FORMAT）
  ├─ webui/             内嵌演示页面（/ui/，go:embed 静态页，复用 REST + WS）
 └─ sidecar/           Python：文档解析 + 离线评测
```

## 目录结构

```text
cmd/server/            HTTP/WS 服务入口
cmd/migrate/           数据库迁移命令
internal/config/       环境变量配置
internal/httpapi/      Gin 路由与 handlers
internal/ws/           WebSocket Hub 与消息持久化
internal/store/        Postgres 访问层 + migrations/*.sql（embed）
internal/storage/      上传文件本地磁盘存储（随机名、限长、防路径穿越）
internal/parser/       sidecar 解析客户端 + 异步 worker（默认 2 并发）
internal/rag/          Markdown 分块、Ollama Embedding、pgvector Indexer/Retriever 适配器与 worker
internal/qa/           带引用问答服务（个人会话/多轮历史 → 按需改写检索 → prompt → 流式回答 → citations 落库）
internal/tasks/        AI 任务抽取（群内已索引资料 → 建议任务 + 引用 → 人工确认）
internal/aitasks/       定时 AI 任务（cron 解析、素材聚合、报告生成、asynq 调度）
internal/llmtrack/      LLM 用量采集（Eino ChatModel 装饰器 + 来源/群组打标）
internal/gitsync/       GitHub 活动同步（REST 客户端、asynq 定时扫描与执行）
internal/workflow/      AI 工作流编排（definition 校验、检索/问答/抽取/报告节点、执行引擎、asynq 调度）
internal/logging/       结构化日志初始化（slog）
webui/                 内嵌演示页面（/ui/；静态单页 + go:embed，仅复用既有接口）
sidecar/               Python 解析服务 + 离线评测（不持有业务状态）
sidecar/eval/          评测语料（fixtures/corpus）、数据集（datasets）与报告输出（reports，已忽略）
docs/                  产品与设计文档
```

## 快速开始

依赖：Go 1.25+（本地版本更低时 Go 会自动下载工具链）、Docker（Compose v2）、Python 3.11+（仅 sidecar 需要）、[Ollama](https://ollama.com)（RAG 索引需要，先执行 `ollama pull bge-m3`）。

```powershell
# 1. 复制配置模板，按需修改（.env 已被 git 忽略；RAG 问答需填 LLM_*，见下）
Copy-Item .env.example .env

# 2. 启动 Postgres(pgvector) 与 Redis
docker compose up -d

# 3. 执行数据库迁移
go run ./cmd/migrate

# 4. 启动服务（默认 :8080）
go run ./cmd/server
```

配置加载顺序：进程环境变量 > 根目录 `.env`（Go 端 godotenv、sidecar python-dotenv、docker compose 端口插值都会读取）。多轮追问相关配置见 `.env.example` 的 `QA_CONTEXT_*` / `QA_REWRITE_*`（默认 `heuristic`，可切 `off`/`always`）。

验证：

```powershell
curl.exe http://localhost:8080/healthz
curl.exe http://localhost:8080/api/groups
```

演示页面（可选，用于快速查看整体效果）：服务启动后打开 `http://localhost:8080/ui/`（访问 `/` 会重定向过去）。页面覆盖聊天（WS 实时）、文件（上传/解析与索引状态/下载/内容/分块/重试解析/重建索引/删除/单文件抽取）、个人问答会话（我的会话列表/历史回看/新建会话/多轮追问，SSE 流式回答，引用可点开原分块）、任务看板（抽取/创建/确认/指派/流转/删除）、风险看板（识别/创建/确认/流转/删除，关联任务可追溯）、报告（定时 AI 任务/周报模板/立即生成/启停/报告查看）、工作流（新建/步骤编辑/手动运行/启停/删除/运行详情含节点状态与 tokens）、GitHub（绑定仓库/手动同步/活动列表）和用量（LLM 调用次数/token/预估成本），解析、索引、任务、风险、报告、仓库变更与工作流运行经 WS 实时刷新；支持 `?group=<群组ID>` 与 `#files` / `#ask` / `#board` / `#risks` / `#reports` / `#workflows` / `#git` / `#usage` 深链。仅为本地演示与手工验收，不是产品前端；静态资源经 `go:embed` 打包，无构建步骤。

## 离线评测

评测分两层：

- `smoke`：保留 5 篇小语料 / 50 题，快速验证上传、解析、索引、检索/问答链路。语料 chunk 数小于 TopK，**Hit@K 不能作为检索质量结论**。
- `benchmark`：`sidecar/eval/corpus/bench/` 下 27 份文档（约 45 chunks，含 PDF/DOCX、干扰文档、旧版冲突计划），`bench-dev.jsonl` / `bench-test.jsonl` / `bench-all.jsonl` 共 220 题（dev/test 各 110），覆盖单跳、多跳、聚合、数值、时序、改写、范围否定、近领域不可答、版本冲突和跨文档；另有 `bench-reasoning.jsonl` 30 题，专门覆盖推导型时间、算术、计数、排序、否定、条件和 3 跳链。

评测方法：

- 检索：用 `/files/:id/chunks` 拉取完整 chunk，按 gold span 计算 `Span Hit@K`、`Span Recall@K`、`AllRequired`、`nDCG@K`、`Span MRR`；span 仍要求来自 `expected_files`，但引用支持改为检查被引 chunk 是否包含证据，不再限制必须来自 expected 文件，避免把合法的跨文件引用判错；同时跑字符 bigram BM25 baseline 和 `--k-sweep 1,3,5`。
- 参数一致性：`/ask` 与 `/search` 都支持请求级 `top_k` / `min_score` 覆盖，评测默认显式传 `top_k` 且 `min_score=0`；BM25 使用同一 `top_k` 且不过滤分数，报告记录请求实际参数，并在 JSON 中保存 `meta.config` 作为服务端环境参考。
- 问答：在检索指标之上检查答案关键词（辅助）、推理题 `expected_answers` 精确答案、引用格式、引用命中和引用支持；严格通过要求 `AllRequired` + 答案正确 + 引用命中/支持 + claim 支持率 ≥80%，并同时报告检索/答案/引用三个分项通过率。多文档题在 `required_files > top_k` 时标记 `impossible`，从通过率中排除。
- 可复现：报告记录 git commit、数据集/语料 hash、LLM/Embedding/chunk/top_k/min_score、语料 chunk 数、95% Wilson 置信区间、按 tag 汇总、oracle 语料覆盖上界、RAG 与 BM25 的配对 bootstrap 增益；`--runs N` 输出多轮均值/标准差；可选 `--judge` LLM-as-judge 并输出人工抽检 CSV。
- 失败分类：区分 `retrieval_wrong_project`、`retrieval_wrong_doc`、`retrieval_missing_required`、`retrieval_missing_project`、`retrieval_below_min_score`、`retrieval_ambiguous_label`、`impossible_k_lt_required` 等，便于定位数据或检索问题。

### 评测数据规范（P1）

- `expected_files`：期望命中的文件；`required_files`：多文档题必须全部命中的文件；`acceptable_files`：同一事实的等价来源，任一命中算 hit，也允许作为 span 证据，但不放宽 `AllRequired`。
- `refusal_subtype`：不可答子类型（`not_in_corpus` / `out_of_scope` / `false_premise` / `entity_confusion` / `stale_version` / `missing_aggregate`）。
- 数据校验：`python -m app.eval_lint`（默认 `bench-all`），检查引用文件、`required > top_k`、跨文件重复段落、模板复用/dev-test 重叠、题干泄漏、span 缺失/歧义、required 无 span、不可答覆盖、文档覆盖和自然度；`--json` 输出完整报告，`--warn-only` 不因错误退出。
- v1 现状（2026-10-21）：220 题、128 模板、42 段跨文件重复、46 个 dev/test 模板重叠、15 处 span 歧义、7 处答案泄漏、2 个 required 文件无 span、12 个不可答（2 类）；报告 `eval/reports/lint-20261021-v1-all.json`（本地，不入库）。

```powershell
cd sidecar
.\.venv\Scripts\pip install -r requirements.txt

# smoke：完整问答 / 仅检索
.\.venv\Scripts\python -m app.eval --base-url http://localhost:8080
.\.venv\Scripts\python -m app.eval --retrieval-only

# benchmark 检索：BM25 baseline + K=1/3/5 sweep
.\.venv\Scripts\python -m app.eval --dataset eval\datasets\bench-test.jsonl --fixtures eval\corpus\bench --split benchmark --retrieval-only --top-k 3 --min-score 0 --k-sweep 1,3,5 --tag bench-retrieval

# benchmark 完整问答（可选 judge，抽检 30 条）
.\.venv\Scripts\python -m app.eval --dataset eval\datasets\bench-test.jsonl --fixtures eval\corpus\bench --split benchmark --top-k 3 --min-score 0 --judge --judge-limit 30 --tag bench-qa

# 只跑推理类 30 题
.\.venv\Scripts\python -m app.eval --dataset eval\datasets\bench-reasoning.jsonl --fixtures eval\corpus\bench --split benchmark --tag reasoning

# 数据 lint（默认 bench-all，输出 JSON 便于后续处理）
.\.venv\Scripts\python -m app.eval_lint --json eval\reports\lint.json
```

### benchmark test 110 题实测结果（P0 新口径，2026-10-21）

环境：Postgres 5433、Redis 6380、sidecar 8000、Go 服务 8080、Ollama `bge-m3`、DeepSeek `deepseek-chat`。语料 27 文件 / 45 chunks；`bench-test` 110 题（104 可答），**top_k=3、min_score=0（显式请求覆盖）**，BM25 使用同一 K。命令：

```powershell
python -m app.eval --dataset eval\datasets\bench-test.jsonl --fixtures eval\corpus\bench --split benchmark --top-k 3 --min-score 0 --k-sweep 1,3,5 --judge --judge-limit 30 --tag p0-tight-qa
```

本地报告（未入库）：`sidecar/eval/reports/p0-20261021-150424/`，含报告 MD/JSON、judge 抽检 CSV 和 `manifest.json`。

| 指标 | 向量检索 | BM25 |
|---|---:|---:|
| File Hit@K | **89.4%** | 83.7% |
| File MRR | **0.728** | 0.704 |
| Span Hit@K | **89.4%** | 78.8% |
| Span Recall@K | **86.4%** | 77.7% |
| AllRequired | **83.7%** | 76.9% |
| nDCG@K | **0.768** | 0.707 |
| Span MRR | **0.723** | 0.676 |

RAG - BM25 配对 bootstrap（n=104，95% CI）：

| 指标 | 均值差 | 95% CI | 赢/输/平 |
|---|---:|---|---|
| File Hit@K | +5.8pp | [-1.9, +13.5] | 12/6/86 |
| AllRequired | +4.8pp | [-2.9, +12.5] | 12/7/85 |
| Span Hit@K | +10.6pp | [+1.9, +19.2] | 16/5/83 |
| Span nDCG@K | +0.061 | [-0.010, +0.131] | 28/17/59 |

K sweep（retrieval-only）：

| K | File Hit@K | Span Hit@K | Span Recall@K | Span AllRequired | nDCG@K | Span MRR |
|---:|---:|---:|---:|---:|---:|---:|
| 1 | 61.8% | 60.7% | 60.7% | 60.7% | 0.607 | 0.607 |
| 3 | 89.4% | 89.4% | 86.4% | 83.7% | 0.768 | 0.723 |
| 5 | 94.2% | 94.2% | 91.7% | 89.4% | 0.789 | 0.734 |

完整问答同样使用 `bench-test` 110 题，`top_k=3`、`min_score=0`；严格通过要求 `AllRequired` + 答案正确 + 引用命中/支持 + claim 支持率 ≥80%：

| 指标 | 结果 |
|---|---:|
| 用例通过（严格） | 84/110（76.4%） |
| 分项：检索 / 答案 / 引用 | 83.7% / 93.3% / 80.8% |
| 关键词命中（辅助） | 95.5% |
| 推理题精确答案 | 11/15（73.3%） |
| 引用格式有效 | 100% |
| 引用命中期望文件 | 89.4% |
| 引用支持率 / 精确率 | 89.4% / 71.2% |
| Claim 引用支持 | 273/305（89.5%） |
| 全部 claim 有据 | 84/104（80.8%） |
| not_in_corpus / out_of_scope 处理 | 5/5 / 1/1 |
| 不可答处理 | 6/6（100%） |
| 误拒答 / 拒答精确率 / 召回率 | 5/104（4.8%） / 54.5% / 100% |
| 延迟 P50 / P95 | 1337 / 2071 ms |
| LLM 调用 / tokens | 108 次 / 159,419 |
| LLM judge | 26/30 pass；correctness 4.63 / faithfulness 4.90 / completeness 4.50 / citation_support 4.83 |

失败分类：`citation_claim` 8、`retrieval_wrong_doc` 8、`retrieval_missing_required` 6、`retrieval_missing_project` 3、`answer_expected` 1。Oracle 语料/标签覆盖上界为 100%，说明当前失败不是 gold span 缺失或语料未索引导致。

与 v3.0 旧口径不可直接比较：旧口径通过 98/110（89.1%），但当时 QA 实际 top_k 由服务端配置决定、引用支持限制在 `expected_files`，且不看 `AllRequired` / claim 阈值。新口径下严格通过 84/110（76.4%），claim 支持 89.5%（旧 77.0%）、引用精确率 71.2%（旧 67.8%）；旧数据保留在 git 历史和本地历史报告中。

结论：同 K 下向量检索的 Span Hit 显著高于 BM25（CI 不含 0），File Hit 的提升仍不显著；剩余失败集中在跨文档/多文档和 claim 级引用，符合 P1 需要优先处理“语料同构、多文档标签和引用证据”的判断。

测试的下一步改进目标：继续扩语料、加真实用户问法和更强干扰；先按 P0 新口径重跑基线，再决定检索参数与数据改造优先级。上述数据相关测试报告不进入版本库，存储在本地。

更多规划见 `docs/PRD.md`；协作与开发约定见 `AGENTS.md`。
