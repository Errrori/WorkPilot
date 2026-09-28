# WorkPilot PRD（v0.1 草稿）

## 一句话

面向小型研发团队的“项目进度智能助手”：群聊 + 文件工作空间 + 带引用的 RAG 问答 + 任务与风险梳理。

## 北极星

任何群内资料，30 秒内给出带出处的答案，并能一键把结论变成任务。

## 目标用户与场景

- 3~10 人的小型研发团队，没有专职 PM，进度靠人肉同步
- 群内讨论需求、上传 PRD/会议纪要/设计稿，信息散落、无人汇总
- 每周要写周报、对齐进度、识别风险，成本高且质量不稳定

## 核心闭环

```text
群聊/文档 → AI 提取任务与状态 → 人工确认 → 轻量看板 → 风险识别 + 周报
```

关键设计：AI 的结论必须可确认、可修改、带引用；确认后的结构化数据支撑后续周报与风险分析。

## MVP 范围（M1~M5）

- 单群聊：发消息、传文件
- 文件空间：PDF / DOCX / Markdown / 会议纪要文本
- 带引用的 RAG 问答（回答可点回原文）
- AI 建议任务 → 人工确认 → 内置轻量看板

## 非目标（明确不做）

- 图片/设计稿理解（v1 只存文件和元信息）
- 甘特图、工时、OKR、多项目、复杂权限、移动端
- 企业级认证（SSO / 审计合规）
- 完整 IM 功能（语音、视频、已读回执等）

## 路线图

| 模块 | 内容 | 状态 |
|---|---|---|
| M0 | 基础设施与骨架（服务、迁移、群聊 REST + WS、健康检查） | 已完成 |
| M1 | 文件工作空间（上传/下载/列表/存储） | 已完成 |
| M2 | 文档解析接入（Go ↔ sidecar，文件转 Markdown 入库） | 已完成 |
| M3 | RAG 索引管道（分块、Embedding、pgvector 适配器） | 已完成 |
| M4 | RAG 问答（检索 + 带引用回答，流式） | 已完成 |
| M5 | 任务抽取与轻量看板（AI 建议 → 人工确认 → 状态管理） | 已完成 |
| U0 | 演示页面（内嵌静态单页：聊天/文件/带引用问答/看板，复用既有 REST + WS，非产品前端） | 已完成 |
| M6 | 风险识别 | 已完成 |
| M7 | 自定义 AI 任务与定时周报（asynq） | 已完成 |
| M8 | 离线评测与可观测性（评测集、token 成本、日志） | 已完成 |

后续可选：M9 对接 GitHub/GitLab（PR/Issue/commit 作为进度信号）。

## 决策记录

- 2026-09：聚焦“小型研发团队项目进度助手”场景，规模不做大，深度押在 RAG 溯源与自定义 AI 任务。
- 技术栈：Go + Gin（群聊/WS/文件）、Eino + pgvector（RAG）、Eino ADK + Graph（Agent/工作流）、Redis（缓存/队列）；文档解析与离线评测放 Python sidecar。
- 进度数据：自建轻量看板，暂不对接 GitHub Issues；M9 再评估集成。
- 开发流程：一次只开发一个模块，实现 → 自测（构建通过、服务可运行、功能符合预期）→ 提交 GitHub 后才开下一个模块（详见 AGENTS.md）。
- 无试点团队：先自用 + 种子/演示数据（seed 命令待做），再考虑找团队试用。
- 配置一律走环境变量，见 `.env.example`。
- 2026-09-26：M0 验收通过（build/vet/test/gofmt、healthz、群组/消息 REST、WS 收发与落库）；Go module 路径定为 `github.com/Errrori/workpilot`。
- 2026-09-26：M1 方案定稿：文件存本地磁盘（`FILE_STORAGE_DIR`，默认 `./data/files`），单文件上限 50MB（`MAX_UPLOAD_MB`），接口含上传/列表/下载/删除，并广播 WS 文件事件。
- 2026-09-26：M1 实现细节：`files` 表 + `GET/POST /api/groups/:id/files`、`GET /api/groups/:id/files/:fileID/download`、`DELETE /api/groups/:id/files/:fileID`；上传者取 `user`（query 或表单），磁盘文件名随机化并限制在存储根目录内；超限返回 413；WS 事件 `file_uploaded` / `file_deleted`；compose 端口参数化为 `POSTGRES_PORT` / `REDIS_PORT`（默认 5432/6379）。
- 2026-09-27：M1 验收通过（build/vet/test/gofmt、compose config 校验、httpapi/storage 单元测试；本机 Docker 未运行，手工接口验证以 `internal/httpapi` 测试覆盖为准）。
- 2026-09-27：M2 方案定稿：上传后自动异步解析（内存 worker，默认 2 并发）+ `POST /api/groups/:id/files/:fileID/parse` 手动重试；失败不自动重试，服务启动时把残留 `parsing` 重置并重入队；解析结果存 Postgres `file_contents` 表；新增 `GET /api/groups/:id/files/:fileID/content`；WS 事件 `file_parsed` / `file_parse_failed`；sidecar 以 compose profile `sidecar` 可选启动；解析状态 pending/parsing/parsed/failed/unsupported；415 视为 unsupported，超时/422/5xx 视为 failed。
- 2026-09-27：M2 实现完成：迁移 `0003_file_parse.sql`（files 增 parse_status/parse_error/parsed_at + 状态约束，新表 file_contents）；`internal/parser`（client 错误映射 + worker 默认 2 并发、队列 128、错误截断 500 字）；重试接口 parsing 中返回 409、未解析内容返回 404；`PARSER_TIMEOUT_SECONDS`（默认 120）。手工验证通过（本机 5432/6379 被本地服务占用，Docker 以 `POSTGRES_PORT=5433` / `REDIS_PORT=6380` 运行）：md/txt 上传后 pending→parsed 且 content 可取回；png→unsupported；停掉 sidecar 上传→failed 含错误信息，重启 sidecar 后 POST parse→parsed；WS 依次收到 `file_uploaded`/`file_parsed`；服务重启自动重入队残留 parsing 任务并解析成功。
- 2026-09-27：开发流程调整：取消单独验收环节，`go build/vet/test` 通过、服务可构建并运行、功能表现符合预期即视为模块完成并提交 GitHub；M2 据此标记已完成。
- 2026-09-27：M3 方案定稿：解析完成的 Markdown 自动分块（Markdown 标题/段落聚合，默认 800 字符/块、100 重叠，按 rune 计数）→ Embedding（Ollama 本地 `bge-m3`，1024 维，走 Eino `embedding.Embedder` + eino-ext ollama 组件，16 条/批）→ 自写 pgvector `Indexer` 适配器入库 `doc_chunks`；迁移 `0004_rag_chunks.sql`（files 增 index_status/index_error/indexed_at/chunk_count，doc_chunks unique(file_id,chunk_index) + HNSW cosine）；索引 worker 仿 parser（内存队列、启动时 indexing→pending 恢复并回填历史 parsed 文件、失败不自动重试）；解析成功后自动入队，`POST /api/groups/:id/files/:fileID/index` 手动重建，`GET /api/groups/:id/files/:fileID/chunks` 便于自测，WS 事件 `file_indexed`/`file_index_failed`；解析失败/不支持的文件 index_status=skipped；M3 仅索引文件内容，群聊消息索引暂不做。配置新增 `EMBEDDING_PROVIDER/MODEL/BASE_URL/API_KEY`、`EMBEDDING_DIM`（默认 1024，须与迁移维度一致）、`CHUNK_SIZE/CHUNK_OVERLAP`、`INDEX_WORKERS`。
- 2026-09-27：M3 实现完成：依赖 `eino v0.9.21` + `eino-ext/components/embedding/ollama`；`internal/rag` 含 chunker（fenced code 不拆、标题为块边界）、Ollama embedder（provider 目前仅支持 ollama，未引入 API_KEY 配置）、自写 `PgVectorIndexer`（按 file_id 分组、单事务替换 chunk、文档需预置 DenseVector）、索引 worker（默认 1 并发、16 条/批、维度校验、错误截断 500 字）；parser worker 增 `SetOnParsed` 回调实现解析成功后自动入队；迁移后历史 parsed 文件由启动回填补齐。手工验证通过（Postgres 5433 / Redis 6380 / 本机 Ollama）：旧文件自动回填 indexed、unsupported→skipped、上传→parsed→indexed 且 `doc_chunks` 向量 1024 维、超长文档分 2 块且块间重叠、WS 收到 `file_indexed`、停 Ollama 重建→failed 附连接错误且旧块保留、重启 Ollama 手动 `/index`→indexed、重启服务自动恢复中断索引并重入队；`go build/vet/test`、`gofmt -l` 通过。
- 2026-09-27：M4 方案定稿：LLM 定 OpenAI 兼容接口（`LLM_PROVIDER/MODEL/BASE_URL/API_KEY/TIMEOUT_SECONDS`，eino-ext openai ChatModel，默认指向本机 Ollama `/v1`）；`POST /api/groups/:id/ask` 以 SSE 流式返回（`sources` → `delta` → `done`，错误发 `error`）；检索用自写 Eino `PgVectorRetriever`（同 bge-m3 embedder、按 `group_id` 过滤、`RETRIEVAL_TOP_K` 默认 6、score = 1 − cosine 距离，group 经 context 传递）；问答落库（迁移 0005 `messages.citations jsonb`，问题与最终回答写入 messages，回答带 citations：file_id/file_name/chunk_index/snippet/score）；无检索结果不调 LLM 直接回固定提示；失败/断连时问题保留、回答不落库。多轮记忆、群聊消息参与检索、rerank、引用高亮、WS 群广播留待后续。
- 2026-09-27：M4 实现完成：依赖 `eino-ext/components/model/openai v0.1.13`；`internal/rag/retriever.go`（group 上下文 + 文档元数据 + WithScore）、`internal/qa`（system prompt 只据资料回答并标注 `[n]`、OpenAI 兼容 ChatModel 流式、引用组装与 snippet 截断 200 字、落库）、`internal/httpapi/ask.go`（JSON 校验、question ≤ 2000 字、SSE 事件序）；手工验证通过（Postgres 5433 / Redis 6380 / Ollama bge-m3 / DeepSeek `deepseek-chat`）：上传并索引的文档问答返回 `sources` 6 条（按分数降序）→ 逐 token `delta` → `done` 落库消息含 citations，回答正确标注 `[1]`；空群提问 sources 为空且不调 LLM，回答固定提示并落库；错误 API key 发 `error` 事件且问题保留、回答不落库；组间隔离生效；历史接口可回看带引用回答；`go build/vet/test`、`gofmt -l` 通过。
- 2026-09-27：配置加载调整（配合 M4 密钥管理）：根目录 `.env`（已 git 忽略）作为本地配置与密钥入口，Go 端 godotenv（`config.Load` 时加载，进程环境变量优先）、sidecar python-dotenv 加载，docker compose 复用同一文件做端口插值；模板仍为 `.env.example`。
- 2026-09-27：M5 方案定稿：任务模型 `tasks`（status suggested/todo/doing/done/rejected、priority low/medium/high、assignee、source manual/extracted、citations jsonb、created_by/confirmed_by/confirmed_at）；`POST /api/groups/:id/tasks/extract` 从群内已索引 chunks 抽取（可 `file_id` 限定单文件，字符预算默认 12000、单次建议上限 20），LLM 非流式输出 JSON 数组，解析校验后落库为 suggested，并按标题去重（忽略大小写/空白，rejected 不参与去重）；REST 另含列表（按状态过滤）、手工创建（直接 todo）、PATCH（离开 suggested 记录 confirmed_by/at）、删除；WS 事件 `task_suggested`/`task_created`/`task_updated`/`task_deleted`；配置 `TASK_EXTRACT_MAX`、`TASK_EXTRACT_BUDGET`；本期纯后端 API + WS，看板前端与“从 AI 回答一键转任务”留待后续。
- 2026-09-27：M5 实现完成：迁移 `0006_tasks.sql`；`internal/store/tasks.go`（任务 CRUD + 抽取素材查询 + 去重标题）；`internal/tasks`（prompt、JSON 数组解析容忍 code fence/前后缀与字符串编号来源、字段截断、citations 编号映射）；`internal/httpapi/tasks.go`；`internal/ws` 任务事件。手工验证通过（Postgres 5433 / Redis 6380 / DeepSeek `deepseek-chat` / sidecar）：上传周会纪要 → 解析索引 → extract 得到 11 条带引用建议（已完成事项未抽取，引用指向 m5-demo.md 分块）→ WS 收到 8 条 `task_suggested`；确认/指派（status=suggested→todo 且 confirmed_by/at 落库）→ `task_updated`；手工建任务（source=manual、直接 todo）、忽略（rejected）、删除与相应 WS 事件；空群 extract 返回 409 且不调 LLM；`file_id` 限定抽取仅引用该文件；非法状态/未知任务/不存在的 file_id/缺 user 等返回 400/404/409 符合预期；修复 LLM 偶发把 `sources` 输出为字符串数组导致的解析 500；`go build/vet/test`、`gofmt -l` 通过。
- 2026-09-27：U0 方案定稿：新增独立小模块 `webui/`（`go:embed` 打包静态单页，无构建、无新依赖、无数据库改动），只在 `cmd/server/main.go` 挂载 `/ui/` 并把 `/` 重定向过去，与业务路由隔离；页面覆盖聊天（WS 实时 + 历史）、文件（上传/状态/下载/内容/分块/重试解析/重建索引/删除/单文件抽取）、SSE 流式问答（引用可点开原分块）、任务看板（抽取/手工创建/确认/指派/流转/删除），全部复用既有 REST + WS + SSE 接口，`file_*`、`task_*` 事件实时刷新；前端原生 HTML/CSS/JS，零 npm；支持 `?group=<id>` 与 `#tab` 深链便于演示。
- 2026-09-27：U0 实现完成：`webui/webui.go` + `webui/static/{index.html,style.css,app.js}`，`cmd/server/main.go` 挂载；验证：`go build/vet/test`、`gofmt -l` 通过；服务运行后 `/` 302→`/ui/`、三个静态资源 200；无头浏览器截图确认聊天/文件/看板页签真实数据渲染、WS 连接指示正常；Node `--check` 通过 JS 语法；`/ws` 冒烟收到广播，`/ask` SSE 事件格式（`sources`/`delta`/`done`）与页面解析一致。仅为本地演示与手工验收，产品级前端选型仍待定。
- 2026-09-27：M6 方案定稿：风险模型 `risks`（severity low/medium/high、status suggested/open/mitigating/resolved/dismissed、owner、source manual/extracted、citations jsonb、related_task_ids bigint[]、created_by/confirmed_by/confirmed_at）；`POST /api/groups/:id/risks/extract` 素材为「未完成任务快照（suggested/todo/doing，含负责人/优先级/更新时间）+ 群内已索引 chunks」（可 `file_id` 限定单文件），字符预算默认 12000（任务最多占一半，保证资料有位置），LLM 非流式输出 JSON 数组 `{title, description, severity, owner, task_ids, sources}`，解析校验后落库为 suggested，并按标题去重（dismissed 不参与），任务与资料皆空返回 409；REST 另含列表（按状态过滤）、手工创建（直接 open）、PATCH（离开 suggested 记录 confirmed_by/at）、删除；WS 事件 `risk_suggested`/`risk_created`/`risk_updated`/`risk_deleted`；配置 `RISK_EXTRACT_MAX`（默认 10）、`RISK_EXTRACT_BUDGET`（默认 12000）；含 webui 风险页签（`#risks` 深链）；时间规则型风险（如 doing 超期）与定时自动识别留待后续。
- 2026-09-27：M6 实现完成：迁移 `0007_risks.sql`；`internal/store/risks.go`（风险 CRUD + 未完成任务快照 + 去重标题）；`internal/risks`（prompt、JSON 数组解析容忍 code fence/前后缀/字符串编号、severity 归一、citations 与 related_task_ids 编号映射、字段截断）；`internal/httpapi/risks.go`；`internal/ws` 风险事件；webui 风险页签。手工验证通过（Postgres 5433 / Redis 6380 / Ollama bge-m3 / DeepSeek `deepseek-chat`）：42 条未完成任务 + 5 块资料 extract 得到 10 条建议（severity 分布 high/medium/low、citations 指向文件分块、related_task_ids 命中相关任务）；WS 依次收到 `risk_suggested`（另一群仅任务快照无资料时抽出 4 条）、手工创建 `risk_created`、确认 open 且 confirmed_by/at 落库、mitigating→resolved 流转 `risk_updated`、删除 `risk_deleted`；`file_id` 限定抽取 5 条建议引用全部只含该文件；跨群 file_id 返回 404、无素材群 409、非法状态/severity 400、未知风险 404；`go build/vet/test`、`gofmt -l`、Node `--check` 通过。

- 2026-09-27：M7 方案定稿：统一「定时 AI 任务 → 报告」机制（周报为预置模板，不单开分支）；迁移 `0008_ai_tasks.sql` 新增 `ai_tasks`（group_id、name、prompt、schedule 5 段 cron、timezone、sources（messages/tasks/risks/files）、lookback_days、enabled、created_by、last_run_at/last_status/last_error、next_run_at）与 `reports`（group_id、ai_task_id 可空、title、content Markdown、status pending/running/succeeded/failed、error、period_start/end、trigger schedule/manual、metrics jsonb 确定性统计、related_task_ids/related_risk_ids bigint[]、created_by、created_at/finished_at）；调度用 asynq（复用 `REDIS_ADDR`，`cmd/server` 内嵌 Server + Scheduler），Scheduler 仅注册每分钟扫描任务，扫 `enabled and next_run_at <= now()` 入队 `ai:run`（TaskID 按任务+计划时间去重），DB 为准、重启自动恢复、停机期间到期任务重启后补跑一次；cron 用 robfig/cron/v3 按 `AI_TASK_TIMEZONE`（默认 Asia/Shanghai）计算；素材时间窗 `[last_run_at 或 now-lookback_days, now)` = 窗口内消息 + 未完成任务/窗口内变更 + open/mitigating 风险 + 文件清单，字符预算 `AI_TASK_BUDGET`（默认 12000），metrics 确定性统计 + LLM 非流式叙述 Markdown（只依据素材）；报告落库后同时以 AI 身份写入群聊消息（sender 用固定 AI 名）；REST 含 `GET/POST /api/groups/:id/ai-tasks`、`PATCH/DELETE .../:taskID`、`POST .../:taskID/run`（立即生成）、`GET .../reports`（可按 ai_task_id 过滤）/`GET|DELETE .../reports/:reportID`；WS 事件 `ai_task_created/updated/deleted`、`report_created`；webui 增 `#reports` 页签（任务管理 + 周报模板一键填参 + 报告查看/关联跳转）；新增配置 `AI_TASK_WORKERS`（默认 1）、`AI_TASK_TIMEZONE`、`AI_TASK_BUDGET`。明确不做（留后续）：报告外发（邮件/IM）、报告编辑与版本、文档向量检索纳入素材、token 成本统计（M8）。
- 2026-09-27：M7 实现完成：依赖 `hibiken/asynq v0.26.0` + `robfig/cron/v3`；迁移 `0008_ai_tasks.sql`（`ai_tasks` + `reports`，last_status 仅 succeeded/failed、reports 亦仅 succeeded/failed——生成在 worker 中同步完成后一次落库，不落 pending/running 中间态）；`internal/store/ai_tasks.go`（任务 CRUD + 到期扫描 + 报告 + 素材查询）；`internal/aitasks`（5 段 cron 解析/NextRun、素材聚合与字符预算分配（消息 50%/任务 25%/风险 20%/文件 5%）、确定性 metrics、prompt、失败也落 failed 报告并记录 last_error、成功后以 `store.SenderAI`（`WorkPilot AI`，与 M4 问答共用）发群聊消息）；`internal/aitasks/scheduler.go`（asynq Server+Scheduler 每分钟扫描 + 启动即刻扫描补跑，TaskID 去重，MaxRetry(0)，每次运行后广播 `ai_task_updated`）；`internal/httpapi/ai_tasks.go`（任务 CRUD/立即生成 202/报告列表详情删除，cron/时区/素材/回看校验）；`internal/ws` AI 任务与报告事件（`BroadcastMessage` 支持 AI 播报）；webui `#reports` 页签（任务列表/周报模板/立即生成/启停/报告查看）。手工验证通过（Postgres 5433 / Redis 6380 / DeepSeek `deepseek-chat`）：创建 `*/1 * * * *` 任务后手动 run 立即生成报告（WS 依次 `report_created`/`message`/`ai_task_updated`，报告含 metrics 与 related ids、群聊出现 `WorkPilot AI` 播报）；定时扫描在 17:28:51、17:29:51 各生成一次 schedule 报告且标题周期随上次运行收窄；停用后不再触发（3 次报告后数量不变）；空素材群走固定提示且不调 LLM、metrics 全 0；改 cron 重算 next_run_at、非法 cron/时区/素材/回看 400、未知任务/报告 404、跨群隔离生效；`go build/vet/test`、`gofmt -l`、Node `--check` 通过。
- 2026-09-27：M8 方案定稿：分三块——① LLM 用量与成本：迁移 `0009_llm_usage.sql`（group_id/source/provider/model/tokens/latency/status/error），新增 `internal/llmtrack` 装饰 Eino `model.BaseChatModel`（Generate 取 `ResponseMeta.Usage`、Stream 用 `schema.Pipe` 透传并从末块取 usage，EOF 记 succeeded、错误记 failed、取消/提前关闭也记，provider 不回 usage 时 token 记 0），ctx 经 `WithCall(source, groupID)` 在 qa/tasks/risks/aitasks 四处打标；`GET /api/groups/:id/usage` 返回 summary + 分来源 + 最近调用，成本按 `LLM_PRICE_INPUT/OUTPUT_PER_MTOK` 读取时计算（默认 0）；webui 加 `#usage` 页签。② 日志：新增 `internal/logging`（slog，`LOG_LEVEL`/`LOG_FORMAT=text|json`），`httpapi` 访问日志中间件替换 `gin.Logger`（`X-Request-Id` 透传/生成 + method/path/status/latency/client_ip/group_id），既有 `log.Printf` 全量迁移 slog。③ 离线评测（仅 RAG 问答）：Go 新增 `POST /api/groups`（评测自建隔离群，也补齐建群能力）与 `POST /api/groups/:id/search`（仅检索不调 LLM，复用 PgVectorRetriever + WithTopK）；sidecar 新增 `app/eval.py` CLI（httpx）：上传 `eval/fixtures/*.md` → 轮询 parse/index → `/search` 或 `/ask`（SSE）→ 指标 Hit@K、MRR、答案关键词命中、引用有效性、不可答拒答、延迟 p50/p95、tokens/成本（/usage 窗口差值）→ 控制台 + `eval/reports/*.md|json`；数据集 `eval/datasets/demo.jsonl` 约 12 题（含不可答题）。评测扩展抽取质量、报告外发、CI 自动化留后续。
- 2026-09-28：M8 实现完成：迁移 `0009_llm_usage.sql`；`internal/llmtrack`（装饰 `model.BaseChatModel`，Generate/Stream 双路径采 usage，流式用 `schema.Pipe` 透传、末块取 usage，失败/取消/消费者提前关闭也落 failed，ctx `WithCall` 在 qa/tasks/risks/aitasks 四处打标，记录用 `WithoutCancel` + 3s 超时保证取消时仍落库，错误截断 500 字）；`internal/store/usage.go`（插入 + summary/按来源/最近调用查询）；`internal/httpapi/usage.go`（from/to 支持 RFC3339 与 `YYYY-MM-DD`，to 按次日零点含全天，默认近 7 天、上限 90 天、limit ≤ 100，成本按单价四舍五入 4 位；httpapi 观测测试覆盖窗口与成本）；`internal/logging` + `accessLog` 中间件（`X-Request-Id` 透传或生成并回写响应头，healthz 不记录，全量 `log.Printf` → slog）；`POST /api/groups`（名称去空白、≤100 字符，201）；`POST /api/groups/:id/search`（query 校验同问答、top_k ≤ 20、无服务 503）；sidecar `app/eval.py` + `app/eval_metrics.py` + `eval/fixtures/{project-brief,release-plan,weekly-meeting}.md` + `eval/datasets/demo.jsonl`（14 题，含 2 道不可答题）+ `tests/test_eval_metrics.py`（8 用例，覆盖 Hit@K/MRR/百分位/关键词/引用有效性/拒答）；`eval/reports/` 已 git 忽略。验证：`go build/vet/test`、`gofmt -l`、sidecar `unittest`（8/8）通过；compose（`POSTGRES_PORT=5433`/`REDIS_PORT=6380`）+ `go run ./cmd/migrate` 应用 0009；服务与 sidecar 启动后 `POST /api/groups` 201 且响应含 `X-Request-Id`，访问日志含 request_id/method/path/status/latency/group_id；上传 `project-brief.md` 自动 parsed→indexed（1 块）；`/search` 返回带分数引用；两次 SSE `/ask`（DeepSeek）后 `/usage` 汇总 qa 2 次调用、699 tokens（流式 usage 采集生效）；`python -m app.eval --retrieval-only` 自建群上传 3 份语料，14 题 Hit@K 12/12、MRR 0.917、报告写入 `eval/reports/`。

## 开放问题

- Embedding 换模型：M3 定为 Ollama `bge-m3`（1024 维）；换不同维度模型需新迁移并全量重建索引
- 问答体验增强：多轮记忆、rerank、引用高亮定位、WS 群广播（M4 未做）
- 前端选型（服务端已提供 REST + WebSocket）；M5 看板当前仅 API + WS，无页面；U0 仅内嵌演示页（`/ui/`），产品前端仍待选型
- 从 AI 回答一键转任务（按 `message_id` 抽取，M4 回答已带 citations）
- 任务抽取的自动/定时触发，以及近重复检测（当前仅精确标题去重）
- 风险识别的自动/定时触发与时间规则（如任务 doing 超期、suggested 长期未确认自动标记）未做，当前仅手动 extract
- 风险识别与任务的自动/定时触发与时间规则（如任务 doing 超期、suggested 长期未确认自动标记）未做；M7 定时任务只产出报告，不自动抽取任务/风险
- 报告体验增强：webui 未把 metrics/related ids 渲染成任务/风险跳转，报告暂不外发（邮件/IM），无编辑与版本
- 定时 AI 任务素材暂不含文档向量检索（文件只以清单出现），按主题检索文档段留待后续
- 离线评测范围为 RAG 问答（检索质量/答案关键词/引用/拒答/延迟/token），任务与风险抽取质量、报告外发效果、CI 自动化留后续
- 鉴权方案（当前用 `?user=` 临时标识，后续替换）
