# WorkPilot

面向小型研发团队的“项目进度智能助手”：团队在群聊中讨论需求、上传文档（PRD、设计稿、会议纪要），AI 读取这些资料，梳理当前进度、识别风险、生成下一阶段任务建议，并支持自定义 AI 任务（如“每周五自动汇总本周进展并生成周报”）。

北极星：**任何群内资料，30 秒内给出带出处的答案，并能一键把结论变成任务。**

## 技术栈

| 层 | 选型 |
|---|---|
| 群聊 / WebSocket / 文件空间 | Go + Gin + gorilla/websocket |
| RAG 管道 | Eino + pgvector（Postgres） |
| Agent 与工作流 | Eino ADK + compose.Graph |
| 缓存 / 队列 | Redis（定时任务后续用 asynq） |
| 文档解析 / 离线评测 | Python sidecar（FastAPI + markitdown） |

产品范围与选型理由见 `docs/PRD.md`。

## 架构

```text
Gin（REST + WebSocket）
 ├─ internal/httpapi   REST 路由与 healthz
 ├─ internal/ws        WebSocket Hub（单实例内存广播）
 ├─ internal/store     Postgres（pgx）+ SQL 迁移
 ├─ internal/storage   上传文件本地磁盘存储
 ├─ internal/rag       Eino RAG 管道（待实现，需自写 pgvector 适配器）
 ├─ internal/agent     Eino Agent / 工作流（待实现）
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
sidecar/               Python 解析服务（不持有业务状态）
docs/                  产品与设计文档
```

## 快速开始

依赖：Go 1.25+（本地版本更低时 Go 会自动下载工具链）、Docker（Compose v2）、Python 3.11+（仅 sidecar 需要）。

```powershell
# 1. 启动 Postgres(pgvector) 与 Redis
docker compose up -d

# 2. 执行数据库迁移
go run ./cmd/migrate

# 3. 启动服务（默认 :8080）
go run ./cmd/server
```

验证：

```powershell
curl.exe http://localhost:8080/healthz
curl.exe http://localhost:8080/api/groups
```

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

Python sidecar（可选，当前仅用于文件解析）：

```powershell
cd sidecar
python -m venv .venv
.\.venv\Scripts\pip install -r requirements.txt
.\.venv\Scripts\uvicorn app.main:app --port 8000
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

配置通过环境变量注入，参考 `.env.example`（`APP_PORT`、`DATABASE_URL`、`REDIS_ADDR`、`REDIS_PASSWORD`、`SIDECAR_URL`、`FILE_STORAGE_DIR`、`MAX_UPLOAD_MB`）；`POSTGRES_PORT` / `REDIS_PORT` 仅控制 compose 的宿主机端口映射，默认 `5432` / `6379`。

## 当前状态

- [x] 服务骨架、健康检查、群组/消息 REST、WebSocket 聊天（持久化 + 广播）
- [x] 文件工作空间：上传/列表/下载/删除（本地磁盘、50MB 上限、WS 文件事件）
- [ ] 文档解析接入（Go ↔ sidecar，文件转 Markdown 入库）
- [ ] Eino RAG：解析 → 分块 → 向量化 → 带引用问答
- [ ] 任务抽取 → 人工确认 → 轻量看板
- [ ] 风险识别与自定义 AI 任务（定时周报）

更多规划见 `docs/PRD.md`；协作与开发约定见 `AGENTS.md`。
