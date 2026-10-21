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
 ├─ internal/qa        带引用问答（OpenAI 兼容 ChatModel 流式 + 落库）
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
internal/qa/           带引用问答服务（检索 → prompt → 流式回答 → citations 落库）
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

配置加载顺序：进程环境变量 > 根目录 `.env`（Go 端 godotenv、sidecar python-dotenv、docker compose 端口插值都会读取）。

验证：

```powershell
curl.exe http://localhost:8080/healthz
curl.exe http://localhost:8080/api/groups
```

演示页面（可选，用于快速查看整体效果）：服务启动后打开 `http://localhost:8080/ui/`（访问 `/` 会重定向过去）。页面覆盖聊天（WS 实时）、文件（上传/解析与索引状态/下载/内容/分块/重试解析/重建索引/删除/单文件抽取）、SSE 流式问答（引用可点开原分块）、任务看板（抽取/创建/确认/指派/流转/删除）、风险看板（识别/创建/确认/流转/删除，关联任务可追溯）、报告（定时 AI 任务/周报模板/立即生成/启停/报告查看）、工作流（新建/步骤编辑/手动运行/启停/删除/运行详情含节点状态与 tokens）、GitHub（绑定仓库/手动同步/活动列表）和用量（LLM 调用次数/token/预估成本），解析、索引、任务、风险、报告、仓库变更与工作流运行经 WS 实时刷新；支持 `?group=<群组ID>` 与 `#files` / `#ask` / `#board` / `#risks` / `#reports` / `#workflows` / `#git` / `#usage` 深链。仅为本地演示与手工验收，不是产品前端；静态资源经 `go:embed` 打包，无构建步骤。

## 离线评测

评测分两层：

- `smoke`：保留 5 篇小语料 / 50 题，快速验证上传、解析、索引、检索/问答链路。语料 chunk 数小于 TopK，**Hit@K 不能作为检索质量结论**。
- `benchmark`：`sidecar/eval/corpus/bench/` 下 27 份文档（约 45 chunks，含 PDF/DOCX、干扰文档、旧版冲突计划），`bench-dev.jsonl` / `bench-test.jsonl` / `bench-all.jsonl` 共 220 题（dev/test 各 110），覆盖单跳、多跳、聚合、数值、时序、改写、范围否定、近领域不可答、版本冲突和跨文档；另有 `bench-reasoning.jsonl` 30 题，专门覆盖推导型时间、算术、计数、排序、否定、条件和 3 跳链。

评测方法：

- 检索：用 `/files/:id/chunks` 拉取完整 chunk，按 gold span 计算 `Span Hit@K`、`Span Recall@K`、`AllRequired`、`nDCG@K`、`Span MRR`；同时跑字符 bigram BM25 baseline 和 `--k-sweep 1,3,5`。
- 问答：在检索指标之上检查答案关键词、引用格式、引用命中和引用支持；拒答拆成 `not_in_corpus` 与 `out_of_scope`，分别统计处理率、误拒答和拒答精确率。
- 可复现：报告记录 git commit、数据集/语料 hash、LLM/Embedding/chunk/top_k/min_score 配置快照、语料 chunk 数、95% Wilson 置信区间、按 tag 汇总；`--runs N` 输出多轮均值/标准差；可选 `--judge` LLM-as-judge 并输出人工抽检 CSV。

```powershell
cd sidecar
.\.venv\Scripts\pip install -r requirements.txt

# smoke：完整问答 / 仅检索
.\.venv\Scripts\python -m app.eval --base-url http://localhost:8080
.\.venv\Scripts\python -m app.eval --retrieval-only

# benchmark 检索：BM25 baseline + K=1/3/5 sweep
.\.venv\Scripts\python -m app.eval --dataset eval\datasets\bench-test.jsonl --fixtures eval\corpus\bench --split benchmark --retrieval-only --top-k 5 --k-sweep 1,3,5 --tag bench-retrieval

# benchmark 完整问答（可选 judge，抽检前 20 条）
.\.venv\Scripts\python -m app.eval --dataset eval\datasets\bench-test.jsonl --fixtures eval\corpus\bench --split benchmark --judge --judge-limit 20 --tag bench-qa

# 只跑推理类 30 题
.\.venv\Scripts\python -m app.eval --dataset eval\datasets\bench-reasoning.jsonl --fixtures eval\corpus\bench --split benchmark --tag reasoning
```

### 核心 95 题实测成绩（2026-10-20，推理题扩展前）

环境：Postgres 5433、Redis 6380、sidecar 8000、Go 服务 8080、Ollama `bge-m3`、DeepSeek `deepseek-chat`。检索用 `bench-test` 95 题（89 可答），`top_k=5`。

| 指标 | 向量检索 | BM25 |
|---|---:|---:|
| Span Hit@K | **96.6%** | 92.1% |
| Span Recall@K | **95.3%** | 90.3% |
| Span AllRequired | **94.4%** | 88.8% |
| nDCG@K | **0.802** | 0.777 |
| Span MRR | **0.746** | 0.715 |

| K | Span Hit@K | Span Recall@K | Span AllRequired | Span MRR |
|---|---:|---:|---:|---:|
| 1 | 59.6% | 59.0% | 58.4% | 0.600 |
| 3 | 91.0% | 89.3% | 87.6% | 0.730 |
| 5 | 96.6% | 95.3% | 94.4% | 0.750 |

注：nDCG 已按修正后的 capped 口径重算，其余检索指标来自 2026-10-20 实跑报告。

完整问答同样使用 `bench-test` 95 题，`top_k=6`：

| 指标 | 结果 |
|---|---:|
| 用例通过 | **92/95（96.8%）** |
| Span Hit@K | 98.9% |
| 关键词命中 | 98.7% |
| 引用格式有效 | 100% |
| 引用命中期望文件 | 97.8% |
| 引用支持率（答案级） | 98.9% |
| 不可答处理 | 6/6（100%） |
| 误拒答 | 1/89（1.1%） |
| 延迟 P50 / P95 | 1305 / 2204 ms |
| LLM 调用 / tokens | 95 次 / 270,017 |

LLM judge 抽检前 20 条可答用例：18/20 pass，correctness 4.75、faithfulness 4.90、completeness 4.75。

推理类 30 题已加入 `bench-reasoning.jsonl`，覆盖期限推导、事件顺序、延期判断、日期差、预算求和/差值、计数、排序、否定排除、条件和 3 跳链。本地 BM25 top5 自检：29/30 召回 gold span、26/30 全部证据召回；1 条被 `shared-ops-runbook.md` 干扰文档抢走。真实向量检索和完整问答待服务启动后单独跑。

结论：检索不是“TopK 覆盖全库”的假满分，K=1 只有 59.6%，K=3 到 5 仍有明显提升；BM25 已经很强，向量检索 Span Hit 比 BM25 高约 4.5 个点。问答主要剩余问题是跨文档/读表类检索漏召和少量模型幻觉，失败集中在 `b-030`、`b-158`、`b-180`。benchmark 题目仍偏模板化，后续应补充真实群聊问题或独立出题，避免再次被词面匹配推高。

报告写入 `sidecar/eval/reports/`（已 git 忽略），不会进入版本库。

更多规划见 `docs/PRD.md`；协作与开发约定见 `AGENTS.md`。
