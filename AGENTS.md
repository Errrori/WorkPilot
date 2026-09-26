# AGENTS.md

WorkPilot：面向小型研发团队的项目进度智能助手。Go 单体（Gin）+ Python sidecar + Postgres(pgvector) + Redis；AI 部分用 Eino。

## 开发流程（必须遵守）

- 一次只开发一个模块，禁止跨多个模块同时改动
- 每个模块按固定顺序推进：实现 → 审查 → 验收 → 提交 GitHub → 才允许开始下一个模块
- 审查/验收最低要求：`go build ./... && go vet ./... && go test ./...` 通过、相关接口手工验证、文档（README/PRD）状态同步
- 提交只包含本模块的改动，不夹带无关修改；未提交前不动下一个模块
- 跨模块的需求先记入待办（PRD/TODO），不要顺手实现
- 模块划分与顺序以 `docs/PRD.md` 路线图为基准

## 命令

```powershell
docker compose up -d          # 启动 Postgres(pgvector) + Redis
go run ./cmd/migrate          # 应用迁移（forward-only）
go run ./cmd/server           # 启动服务 :8080
go build ./... && go vet ./... && go test ./...   # 提交前验证
gofmt -l .                    # 格式检查（有输出说明未格式化）
docker compose config -q      # 校验 compose

# sidecar（按需）
cd sidecar
python -m venv .venv
.\.venv\Scripts\pip install -r requirements.txt
.\.venv\Scripts\uvicorn app.main:app --port 8000
```

## 结构

- `cmd/server` 入口；`cmd/migrate` 迁移；`internal/httpapi` 路由；`internal/ws` 聊天 Hub；`internal/store` Postgres 访问层；`internal/config` 环境变量
- `internal/store/migrations/` 由 `go:embed` 打包；新增迁移只加新文件，不改已应用的文件
- `sidecar/` 只做文档解析与离线评测，不持有业务状态；Go 通过 HTTP 调用
- 产品范围见 `docs/PRD.md`

## 约定与坑

- go.mod 声明 Go 1.25（pgx v5.11 等依赖要求）；本地 Go 版本更低时 Go 会自动下载工具链，属正常现象
- 所有配置走环境变量，默认值见 `internal/config/config.go`，样例见 `.env.example`；不要硬编码连接串
- Eino 的 pgvector `Retriever`/`Indexer` 适配器需自行实现（eino-ext 无 pgvector 组件）；实现后放在 `internal/rag`
- 暂无鉴权：WS 用 `?user=&group_id=` 临时标识，后续替换为真实认证；`CheckOrigin` 当前放开
- WS 是单实例内存 Hub；要水平扩展需接 Redis Pub/Sub
- 数据库 schema 变更必须新建迁移文件；不要编辑历史迁移
- Go 代码注释用英文且只写必要处；文档（README/PRD/AGENTS）用中文
- LLM/Embedding 提供方未定：接入前先确认（eino-ext 已有 dashscope/ark/ollama/openai 组件），不要擅自绑定某家 SDK
- 没有试点团队：开发依赖种子/演示数据（seed 命令待做）
- 本机已装 PostgreSQL/Redis 时注意端口占用：宿主机 PostgreSQL 服务会抢占 5432，导致 Docker Postgres 映射不可用（表现为密码验证失败），本地开发前需停用该服务，或用 `POSTGRES_PORT` / `REDIS_PORT` 调整 compose 宿主机端口并同步 `DATABASE_URL` / `REDIS_ADDR`
