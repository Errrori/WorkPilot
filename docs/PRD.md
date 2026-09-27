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
| M2 | 文档解析接入（Go ↔ sidecar，文件转 Markdown 入库） | 已完成（待验收） |
| M3 | RAG 索引管道（分块、Embedding、pgvector 适配器） | 待开始 |
| M4 | RAG 问答（检索 + 带引用回答，流式） | 待开始 |
| M5 | 任务抽取与轻量看板（AI 建议 → 人工确认 → 状态管理） | 待开始 |
| M6 | 风险识别 | 待开始 |
| M7 | 自定义 AI 任务与定时周报（asynq） | 待开始 |
| M8 | 离线评测与可观测性（评测集、token 成本、日志） | 待开始 |

后续可选：M9 对接 GitHub/GitLab（PR/Issue/commit 作为进度信号）。

## 决策记录

- 2026-09：聚焦“小型研发团队项目进度助手”场景，规模不做大，深度押在 RAG 溯源与自定义 AI 任务。
- 技术栈：Go + Gin（群聊/WS/文件）、Eino + pgvector（RAG）、Eino ADK + Graph（Agent/工作流）、Redis（缓存/队列）；文档解析与离线评测放 Python sidecar。
- 进度数据：自建轻量看板，暂不对接 GitHub Issues；M9 再评估集成。
- 开发流程：一次只开发一个模块，实现 → 审查 → 验收 → 提交 GitHub 后才开下一个模块（详见 AGENTS.md）。
- 无试点团队：先自用 + 种子/演示数据（seed 命令待做），再考虑找团队试用。
- 配置一律走环境变量，见 `.env.example`。
- 2026-09-26：M0 验收通过（build/vet/test/gofmt、healthz、群组/消息 REST、WS 收发与落库）；Go module 路径定为 `github.com/Errrori/workpilot`。
- 2026-09-26：M1 方案定稿：文件存本地磁盘（`FILE_STORAGE_DIR`，默认 `./data/files`），单文件上限 50MB（`MAX_UPLOAD_MB`），接口含上传/列表/下载/删除，并广播 WS 文件事件。
- 2026-09-26：M1 实现细节：`files` 表 + `GET/POST /api/groups/:id/files`、`GET /api/groups/:id/files/:fileID/download`、`DELETE /api/groups/:id/files/:fileID`；上传者取 `user`（query 或表单），磁盘文件名随机化并限制在存储根目录内；超限返回 413；WS 事件 `file_uploaded` / `file_deleted`；compose 端口参数化为 `POSTGRES_PORT` / `REDIS_PORT`（默认 5432/6379）。
- 2026-09-27：M1 验收通过（build/vet/test/gofmt、compose config 校验、httpapi/storage 单元测试；本机 Docker 未运行，手工接口验证以 `internal/httpapi` 测试覆盖为准）。
- 2026-09-27：M2 方案定稿：上传后自动异步解析（内存 worker，默认 2 并发）+ `POST /api/groups/:id/files/:fileID/parse` 手动重试；失败不自动重试，服务启动时把残留 `parsing` 重置并重入队；解析结果存 Postgres `file_contents` 表；新增 `GET /api/groups/:id/files/:fileID/content`；WS 事件 `file_parsed` / `file_parse_failed`；sidecar 以 compose profile `sidecar` 可选启动；解析状态 pending/parsing/parsed/failed/unsupported；415 视为 unsupported，超时/422/5xx 视为 failed。
- 2026-09-27：M2 实现完成（待验收）：迁移 `0003_file_parse.sql`（files 增 parse_status/parse_error/parsed_at + 状态约束，新表 file_contents）；`internal/parser`（client 错误映射 + worker 默认 2 并发、队列 128、错误截断 500 字）；重试接口 parsing 中返回 409、未解析内容返回 404；`PARSER_TIMEOUT_SECONDS`（默认 120）。手工验证通过（本机 5432/6379 被本地服务占用，Docker 以 `POSTGRES_PORT=5433` / `REDIS_PORT=6380` 运行）：md/txt 上传后 pending→parsed 且 content 可取回；png→unsupported；停掉 sidecar 上传→failed 含错误信息，重启 sidecar 后 POST parse→parsed；WS 依次收到 `file_uploaded`/`file_parsed`；服务重启自动重入队残留 parsing 任务并解析成功。

## 开放问题

- LLM / Embedding 提供方（eino-ext 支持 dashscope、ark、ollama、openai 等）
- 前端选型（服务端已提供 REST + WebSocket）
- 鉴权方案（当前用 `?user=` 临时标识，后续替换）
