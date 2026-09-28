# WorkPilot

面向小型研发团队的“项目进度智能助手”：团队在群聊中讨论需求、上传文档（PRD、设计稿、会议纪要），AI 读取这些资料，梳理当前进度、识别风险、生成下一阶段任务建议，并支持自定义 AI 任务（如“每周五自动汇总本周进展并生成周报”）。

北极星：**任何群内资料，30 秒内给出带出处的答案，并能一键把结论变成任务。**

## 技术栈

| 层 | 选型 |
|---|---|
| 群聊 / WebSocket / 文件空间 | Go + Gin + gorilla/websocket |
| RAG 管道 | Eino + pgvector（Postgres） |
| Agent 与工作流 | Eino ADK + compose.Graph |
| 缓存 / 队列 | Redis（asynq：定时 AI 任务与周报、GitHub 活动同步） |
| GitHub 集成 | GitHub REST API v3（PAT，asynq 定时轮询 PR/Issue/Commit） |
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
 ├─ internal/risks     风险识别（任务看板快照 + 已索引资料 → 带引用的风险建议）
 ├─ internal/aitasks   自定义 AI 任务与定时周报（asynq 扫描/执行 + cron + 报告落库）
 ├─ internal/llmtrack  LLM 用量采集（装饰 ChatModel，记录 tokens/延迟/失败 → llm_usage）
 ├─ internal/gitsync   GitHub 活动同步（REST 客户端 + asynq 轮询 → repos/repo_items）
 ├─ internal/logging   slog 初始化（LOG_LEVEL / LOG_FORMAT）
 ├─ internal/agent     Eino Agent / 工作流（待实现）
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
internal/risks/        风险识别（未完成任务 + 已索引资料 → 建议风险 + 引用 + 关联任务）
internal/aitasks/       定时 AI 任务（cron 解析、素材聚合、报告生成、asynq 调度）
internal/llmtrack/      LLM 用量采集（Eino ChatModel 装饰器 + 来源/群组打标）
internal/gitsync/       GitHub 活动同步（REST 客户端、asynq 定时扫描与执行）
internal/logging/       结构化日志初始化（slog）
webui/                 内嵌演示页面（/ui/；静态单页 + go:embed，仅复用既有接口）
sidecar/               Python 解析服务 + 离线评测（不持有业务状态）
sidecar/eval/          评测语料（fixtures）、数据集（datasets）与报告输出（reports，已忽略）
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

演示页面（可选，用于快速查看整体效果）：服务启动后打开 `http://localhost:8080/ui/`（访问 `/` 会重定向过去）。页面覆盖聊天（WS 实时）、文件（上传/解析与索引状态/下载/内容/分块/重试解析/重建索引/删除/单文件抽取）、SSE 流式问答（引用可点开原分块）、任务看板（抽取/创建/确认/指派/流转/删除）、风险看板（识别/创建/确认/流转/删除，关联任务可追溯）、报告（定时 AI 任务/周报模板/立即生成/启停/报告查看）、GitHub（绑定仓库/手动同步/活动列表）和用量（LLM 调用次数/token/预估成本），解析、索引、任务、风险、报告与仓库变更经 WS 实时刷新；支持 `?group=<群组ID>` 与 `#files` / `#ask` / `#board` / `#risks` / `#reports` / `#git` / `#usage` 深链。仅为本地演示与手工验收，不是产品前端；静态资源经 `go:embed` 打包，无构建步骤。

WebSocket 冒烟测试（任意 WS 客户端，如 wscat）：

```text
ws://localhost:8080/ws?group_id=00000000-0000-0000-0000-000000000001&user=alice
发送: {"type":"message","content":"你好"}
```

文件工作空间（上传/列表/下载/删除，上传与删除会广播 WS `file_uploaded` / `file_deleted` 事件）：

```powershell
curl.exe -F "file=@.\docs\PRD.md" -F "user=alice" http://localhost:8080/api/groups/00000000-0000-0000-0000-000000000001/files
curl.exe http://localhost:8080/api/groups/00000000-0000-0000-0000-000000000001/files
curl.exe -OJ http://localhost:8080/api/groups/00000000-0000-0000-0000-000000000001/files/1/download
curl.exe -X DELETE http://localhost:8080/api/groups/00000000-0000-0000-0000-000000000001/files/1
```

上传后自动异步解析为 Markdown：`parse_status` 依次为 `pending` → `parsing` → `parsed`（文本类 `txt/md/csv` 直读，PDF/DOCX 等经 markitdown；不支持的扩展名为 `unsupported`，sidecar 未启动或解析异常为 `failed` 并附 `parse_error`），完成时广播 WS `file_parsed` / `file_parse_failed`；失败可手动重试（`parsing` 中返回 409）：

```powershell
curl.exe http://localhost:8080/api/groups/00000000-0000-0000-0000-000000000001/files/1/content
curl.exe -X POST http://localhost:8080/api/groups/00000000-0000-0000-0000-000000000001/files/1/parse
```

解析完成后自动进入 RAG 索引（Markdown 分块约 800 字符/块、100 重叠 → Ollama `bge-m3` Embedding → pgvector `doc_chunks`），`index_status` 依次为 `pending` → `indexing` → `indexed`（解析失败/不支持的文件为 `skipped`），完成时广播 WS `file_indexed` / `file_index_failed`；失败可手动重建，服务重启会自动恢复中断的索引并回填未索引文件：

```powershell
curl.exe http://localhost:8080/api/groups/00000000-0000-0000-0000-000000000001/files/1/chunks
curl.exe -X POST http://localhost:8080/api/groups/00000000-0000-0000-0000-000000000001/files/1/index
```

RAG 问答（检索本群已索引分块，LLM 走 OpenAI 兼容接口，默认本机 Ollama `/v1`，也可在 `.env` 配 DeepSeek 等云端 API）。`POST /api/groups/:id/ask` 以 SSE 流式返回：`sources`（引用：file_id/file_name/chunk_index/snippet/score，可点回原文件）→ `delta`（增量文本）→ `done`（落库消息，含 citations）；无可用资料时不调 LLM，直接返回提示；问题和最终回答写入群聊历史：

```powershell
curl.exe -N -X POST http://localhost:8080/api/groups/00000000-0000-0000-0000-000000000001/ask -H "Content-Type: application/json" -d '{"user":"alice","question":"当前进度和风险分别是什么？"}'
```

任务抽取与轻量看板（AI 从群内已索引资料抽取任务建议 → 人工确认 → 状态管理，全链路带引用；建议/建/改/删会广播 WS `task_suggested` / `task_created` / `task_updated` / `task_deleted`）。`POST .../tasks/extract` 默认汇总全群已索引资料，也可用 `file_id` 限定单个文件；同组标题相同且未被忽略的任务不会重复建议，群内没有已索引资料返回 409。状态流转：`suggested`（待确认）→ `todo` / `doing` / `done`，或 `rejected`（忽略）；离开 `suggested` 时记录 `confirmed_by` / `confirmed_at`：

```powershell
# 抽取任务建议（全群或指定文件）
curl.exe -X POST http://localhost:8080/api/groups/00000000-0000-0000-0000-000000000001/tasks/extract -H "Content-Type: application/json" -d '{"user":"alice"}'
curl.exe -X POST http://localhost:8080/api/groups/00000000-0000-0000-0000-000000000001/tasks/extract -H "Content-Type: application/json" -d '{"user":"alice","file_id":1}'
# 看板列表（可按状态过滤）与手工建任务（直接 todo）
curl.exe "http://localhost:8080/api/groups/00000000-0000-0000-0000-000000000001/tasks?status=suggested"
curl.exe -X POST http://localhost:8080/api/groups/00000000-0000-0000-0000-000000000001/tasks -H "Content-Type: application/json" -d '{"user":"alice","title":"准备上线检查清单","priority":"high"}'
# 确认/编辑/忽略/删除
curl.exe -X PATCH http://localhost:8080/api/groups/00000000-0000-0000-0000-000000000001/tasks/1 -H "Content-Type: application/json" -d '{"user":"alice","status":"todo","assignee":"bob"}'
curl.exe -X DELETE http://localhost:8080/api/groups/00000000-0000-0000-0000-000000000001/tasks/1
```

风险识别（AI 依据未完成任务看板 + 群内已索引资料识别风险，输出带引用的建议，并关联相关任务；建议/建/改/删会广播 WS `risk_suggested` / `risk_created` / `risk_updated` / `risk_deleted`）。`POST .../risks/extract` 默认汇总全群，也可用 `file_id` 限定单个文件；没有未完成任务且没有已索引资料时返回 409。严重级 `low` / `medium` / `high`；状态流转：`suggested`（待确认）→ `open`（确认）→ `mitigating`（处理中）→ `resolved`（已解决），或 `dismissed`（忽略）；离开 `suggested` 时记录 `confirmed_by` / `confirmed_at`：

```powershell
# 识别风险（全群或指定文件）
curl.exe -X POST http://localhost:8080/api/groups/00000000-0000-0000-0000-000000000001/risks/extract -H "Content-Type: application/json" -d '{"user":"alice"}'
curl.exe -X POST http://localhost:8080/api/groups/00000000-0000-0000-0000-000000000001/risks/extract -H "Content-Type: application/json" -d '{"user":"alice","file_id":1}'
# 风险列表（可按状态过滤）与手工新建（直接 open）
curl.exe "http://localhost:8080/api/groups/00000000-0000-0000-0000-000000000001/risks?status=suggested"
curl.exe -X POST http://localhost:8080/api/groups/00000000-0000-0000-0000-000000000001/risks -H "Content-Type: application/json" -d '{"user":"alice","title":"测试环境就绪时间未确认","severity":"high","owner":"bob"}'
# 确认/流转/编辑/删除
curl.exe -X PATCH http://localhost:8080/api/groups/00000000-0000-0000-0000-000000000001/risks/1 -H "Content-Type: application/json" -d '{"user":"alice","status":"open","owner":"bob"}'
curl.exe -X DELETE http://localhost:8080/api/groups/00000000-0000-0000-0000-000000000001/risks/1
```

自定义 AI 任务与定时周报（asynq 每分钟扫描到期任务，执行后生成报告并同时以 `WorkPilot AI` 身份发到群聊；周报只是预置模板，本质是自定义任务）。素材取自时间窗 `[上次运行或 now-lookback_days 天, now)`：可选消息/任务/风险/文件清单/GitHub 活动（窗口内同步到的 PR/Issue/Commit，报告 metrics 同时给出问题/PR 状态计数与提交数），`prompt` 作为附加要求；失败会生成 failed 报告并记录 `last_error`，不影响后续调度。建议/建/改/删广播 WS `ai_task_created` / `ai_task_updated` / `ai_task_deleted`，报告生成广播 `report_created`。cron 为 5 段（分 时 日 月 周），按任务 `timezone`（默认 `AI_TASK_TIMEZONE`）解释：

```powershell
# 创建定时任务（cron 字段：分 时 日 月 周；这里每周五 18:00）
curl.exe -X POST http://localhost:8080/api/groups/00000000-0000-0000-0000-000000000001/ai-tasks -H "Content-Type: application/json" -d '{"user":"alice","name":"每周进展周报","schedule":"0 18 * * 5","prompt":"汇总本周期进展、任务变化与风险阻塞，并给出下周计划建议","lookback_days":7}'
# 任务列表 / 编辑（改 cron 会重算下次运行）/ 启停 / 删除
curl.exe http://localhost:8080/api/groups/00000000-0000-0000-0000-000000000001/ai-tasks
curl.exe -X PATCH http://localhost:8080/api/groups/00000000-0000-0000-0000-000000000001/ai-tasks/1 -H "Content-Type: application/json" -d '{"user":"alice","enabled":false}'
curl.exe -X DELETE http://localhost:8080/api/groups/00000000-0000-0000-0000-000000000001/ai-tasks/1
# 立即生成（异步入队，完成后 WS 推送报告与群聊消息）
curl.exe -X POST http://localhost:8080/api/groups/00000000-0000-0000-0000-000000000001/ai-tasks/1/run -H "Content-Type: application/json" -d '{"user":"alice"}'
# 报告列表（可按 ai_task_id 过滤）/ 详情 / 删除
curl.exe "http://localhost:8080/api/groups/00000000-0000-0000-0000-000000000001/reports?limit=20"
curl.exe http://localhost:8080/api/groups/00000000-0000-0000-0000-000000000001/reports/1
curl.exe -X DELETE http://localhost:8080/api/groups/00000000-0000-0000-0000-000000000001/reports/1
```

GitHub 集成（M9：PR/Issue/Commit 作为进度信号）。按群绑定仓库（`owner/name`，仅 GitHub），asynq 每分钟扫描到期仓库并同步最近 `GIT_SYNC_LOOKBACK_DAYS` 天的 PR、Issue 与 commit（`GIT_SYNC_INTERVAL_MINUTES` 控制间隔，`GITHUB_TOKEN` 可选，未配置则走匿名接口限流 60 次/小时；GitHub Enterprise 可改 `GITHUB_BASE_URL`）。同步为 upsert，PR 状态含 `open` / `merged` / `closed`，Issue 混入的 PR 会被过滤；建/改/删/同步广播 WS `repo_created` / `repo_updated` / `repo_deleted` / `repo_synced`。同步到窗口内的活动会作为定时 AI 任务的 `git` 素材：

```powershell
# 绑定仓库（立即排队首次同步）与列表
curl.exe -X POST http://localhost:8080/api/groups/00000000-0000-0000-0000-000000000001/repos -H "Content-Type: application/json" -d '{"user":"alice","repo":"gin-gonic/gin"}'
curl.exe http://localhost:8080/api/groups/00000000-0000-0000-0000-000000000001/repos
# 手动同步（异步入队，完成后 WS repo_synced）/ 启停 / 删除
curl.exe -X POST http://localhost:8080/api/groups/00000000-0000-0000-0000-000000000001/repos/1/sync -H "Content-Type: application/json" -d '{"user":"alice"}'
curl.exe -X PATCH http://localhost:8080/api/groups/00000000-0000-0000-0000-000000000001/repos/1 -H "Content-Type: application/json" -d '{"user":"alice","enabled":false}'
curl.exe -X DELETE http://localhost:8080/api/groups/00000000-0000-0000-0000-000000000001/repos/1
# 活动列表（可按 kind=pull_request|issue|commit 过滤，limit ≤ 500）
curl.exe "http://localhost:8080/api/groups/00000000-0000-0000-0000-000000000001/repos/1/items?kind=pull_request&limit=50"
```

LLM 用量与成本（所有 Chat 调用经 `internal/llmtrack` 装饰后落库 `llm_usage`，含流式问答；记录来源 qa/task_extract/risk_extract/ai_task、tokens、延迟与失败；provider 不回 usage 时 token 记 0）。`GET /api/groups/:id/usage` 支持 `from` / `to`（RFC3339 或 `YYYY-MM-DD`，默认最近 7 天，最大 90 天）与 `limit`，返回 summary、按来源汇总与最近调用；成本按 `LLM_PRICE_INPUT_PER_MTOK` / `LLM_PRICE_OUTPUT_PER_MTOK`（每百万 token，默认 0 表示不估算）读取时计算。演示页「用量」页签（`#usage`）可视化，也可建群：

```powershell
# 新建群组（返回 group.id）
curl.exe -X POST http://localhost:8080/api/groups -H "Content-Type: application/json" -d '{"name":"新项目群"}'
# 用量汇总（默认最近 7 天）
curl.exe "http://localhost:8080/api/groups/00000000-0000-0000-0000-000000000001/usage?from=2026-09-01&to=2026-09-30&limit=20"
# 仅检索（不调 LLM，返回与问答相同的引用结构，供评测与调试）
curl.exe -X POST http://localhost:8080/api/groups/00000000-0000-0000-0000-000000000001/search -H "Content-Type: application/json" -d '{"query":"当前进度和风险？","top_k":6}'
```

日志：进程统一走 `slog`（`LOG_FORMAT=text|json`、`LOG_LEVEL=debug|info|warn|error`），每个 HTTP 请求输出一行结构化访问日志（`request_id`（透传或生成 `X-Request-Id`）、method、path、status、`latency_ms`、client_ip、group_id），健康检查不记录。

Python sidecar（解析必需，可手动启动或走 compose profile）：

```powershell
cd sidecar
python -m venv .venv
.\.venv\Scripts\pip install -r requirements.txt
.\.venv\Scripts\uvicorn app.main:app --port 8000

# 或：docker compose --profile sidecar up -d --build
```

离线评测（M8，sidecar CLI + 评测集，仅评 RAG 问答）：预置语料 `sidecar/eval/fixtures/*.md` 与数据集 `sidecar/eval/datasets/demo.jsonl`（问题 + 期望文件 + 答案关键词，含不可答题）。运行时会自建隔离群组、上传语料并等待解析索引完成，然后逐题走 `/ask`（SSE，完整问答）或 `--retrieval-only` 走 `/search`（不调 LLM），输出 Hit@K、MRR、关键词命中、引用有效性、不可答拒答、延迟 P50/P95 与窗口内 token/成本，报告写入 `sidecar/eval/reports/`（已 git 忽略）。评测需要 Postgres/Redis、sidecar 与 Ollama（完整模式还需可用 LLM）都已启动：

```powershell
cd sidecar
# 完整问答评测（自动建群 + 上传 fixtures）
.\.venv\Scripts\python -m app.eval --base-url http://localhost:8080
# 仅检索快速回归（只需要 Embedding，不消耗 LLM token）
.\.venv\Scripts\python -m app.eval --retrieval-only
# 复用/指定群组、指定数据集
.\.venv\Scripts\python -m app.eval --group 00000000-0000-0000-0000-000000000001 --dataset eval\datasets\demo.jsonl
# 指标逻辑单元测试
.\.venv\Scripts\python -m unittest discover -s tests
```

## 常用命令

| 命令 | 说明 |
|---|---|
| `docker compose up -d` | 启动 Postgres 与 Redis |
| `docker compose config -q` | 校验 compose 配置 |
| `go run ./cmd/migrate` | 应用迁移（forward-only） |
| `go run ./cmd/server` | 启动 REST / WebSocket 服务 |
| `go build ./...` | 构建 |
| `go vet ./...` | 静态检查 |
| `go test ./...` | 单元测试 |
| `gofmt -l .` | 格式检查 |
| `.\.venv\Scripts\python -m app.eval` | 离线评测（在 `sidecar/` 下运行） |
| `.\.venv\Scripts\python -m unittest discover -s tests` | sidecar 评测指标单元测试 |

配置通过环境变量注入，参考 `.env.example`（`APP_PORT`、`DATABASE_URL`、`REDIS_ADDR`、`REDIS_PASSWORD`、`SIDECAR_URL`、`FILE_STORAGE_DIR`、`MAX_UPLOAD_MB`、`PARSER_TIMEOUT_SECONDS`、`EMBEDDING_PROVIDER`、`EMBEDDING_MODEL`、`EMBEDDING_BASE_URL`、`EMBEDDING_DIM`、`EMBEDDING_TIMEOUT_SECONDS`、`CHUNK_SIZE`、`CHUNK_OVERLAP`、`INDEX_WORKERS`、`LLM_PROVIDER`、`LLM_MODEL`、`LLM_BASE_URL`、`LLM_API_KEY`、`LLM_TIMEOUT_SECONDS`、`RETRIEVAL_TOP_K`、`LLM_PRICE_INPUT_PER_MTOK`、`LLM_PRICE_OUTPUT_PER_MTOK`、`LLM_PRICE_CURRENCY`、`LOG_LEVEL`、`LOG_FORMAT`、`TASK_EXTRACT_MAX`、`TASK_EXTRACT_BUDGET`、`RISK_EXTRACT_MAX`、`RISK_EXTRACT_BUDGET`、`AI_TASK_WORKERS`、`AI_TASK_TIMEZONE`、`AI_TASK_BUDGET`、`GITHUB_BASE_URL`、`GITHUB_TOKEN`、`GIT_SYNC_INTERVAL_MINUTES`、`GIT_SYNC_LOOKBACK_DAYS`、`GIT_SYNC_WORKERS`）；`POSTGRES_PORT` / `REDIS_PORT` / `SIDECAR_PORT` 仅控制 compose 的宿主机端口映射，默认 `5432` / `6379` / `8000`。`EMBEDDING_DIM` 默认 `1024`，必须与迁移中的 `vector(1024)` 维度一致，换维度模型需新增迁移并全量重建索引。`AI_TASK_*` 控制定时 AI 任务：worker 并发（默认 1）、默认时区（默认 `Asia/Shanghai`）、素材字符预算（默认 12000）。`GITHUB_*` / `GIT_SYNC_*` 控制 GitHub 集成：API 地址（默认 `https://api.github.com`）、可选 PAT、同步间隔分钟（默认 10）、回看天数（默认 7）、同步并发（默认 1）。

## 当前状态

- [x] 服务骨架、健康检查、群组/消息 REST、WebSocket 聊天（持久化 + 广播）
- [x] 文件工作空间：上传/列表/下载/删除（本地磁盘、50MB 上限、WS 文件事件）
- [x] 文档解析接入（Go ↔ sidecar，文件转 Markdown 入库，异步 + 失败重试）
- [x] RAG 索引管道（分块 → Ollama Embedding → pgvector 入库，自动索引 + 手动重建）
- [x] RAG 问答（SSE 流式、带引用可点回原文、问答落库）
- [x] 任务抽取 → 人工确认 → 轻量看板（AI 建议带引用、状态流转、WS 事件）
- [x] 演示页面（`/ui/`：聊天/文件/问答/看板/风险，复用既有 REST + WS，无构建）
- [x] 风险识别（任务看板快照 + 已索引资料 → 带引用建议 → 确认/流转，关联任务）
- [x] 自定义 AI 任务与定时周报（asynq 定时执行 → 报告落库 + 群聊播报 + WS 事件）
- [x] 离线评测与可观测性（llm_usage 用量/成本 + slog 访问日志 + sidecar 评测 runner）
- [x] GitHub 集成（群内绑定仓库 → 定时同步 PR/Issue/Commit → 页签查看 + 进入周报素材）

更多规划见 `docs/PRD.md`；协作与开发约定见 `AGENTS.md`。
