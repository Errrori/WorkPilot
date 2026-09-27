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
| M5 | 任务抽取与轻量看板（AI 建议 → 人工确认 → 状态管理） | 待开始 |
| M6 | 风险识别 | 待开始 |
| M7 | 自定义 AI 任务与定时周报（asynq） | 待开始 |
| M8 | 离线评测与可观测性（评测集、token 成本、日志） | 待开始 |

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

## 开放问题

- Embedding 换模型：M3 定为 Ollama `bge-m3`（1024 维）；换不同维度模型需新迁移并全量重建索引
- 问答体验增强：多轮记忆、rerank、引用高亮定位、WS 群广播（M4 未做）
- 前端选型（服务端已提供 REST + WebSocket）
- 鉴权方案（当前用 `?user=` 临时标识，后续替换）
