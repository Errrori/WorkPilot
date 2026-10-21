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

演示页面（可选，用于快速查看整体效果）：服务启动后打开 `http://localhost:8080/ui/`（访问 `/` 会重定向过去）。页面覆盖聊天（WS 实时）、文件（上传/解析与索引状态/下载/内容/分块/重试解析/重建索引/删除/单文件抽取）、SSE 流式问答（引用可点开原分块）、任务看板（抽取/创建/确认/指派/流转/删除）、风险看板（识别/创建/确认/流转/删除，关联任务可追溯）、报告（定时 AI 任务/周报模板/立即生成/启停/报告查看）、工作流（新建/步骤编辑/手动运行/启停/删除/运行详情含节点状态与 tokens）、GitHub（绑定仓库/手动同步/活动列表）和用量（LLM 调用次数/token/预估成本），解析、索引、任务、风险、报告、仓库变更与工作流运行经 WS 实时刷新；支持 `?group=<群组ID>` 与 `#files` / `#ask` / `#board` / `#risks` / `#reports` / `#workflows` / `#git` / `#usage` 深链。仅为本地演示与手工验收，不是产品前端；静态资源经 `go:embed` 打包，无构建步骤。

更多规划见 `docs/PRD.md`；协作与开发约定见 `AGENTS.md`。
