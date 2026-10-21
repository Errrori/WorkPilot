# 星河技术架构

## 服务划分

服务以单二进制运行，发布使用双实例部署，数据库迁移只做向前兼容。

## 数据与检索

- 星河的 Markdown 分块约 800 字符/块、100 字符重叠，索引写入 Postgres pgvector。
- Postgres 16 保存群组、文件、消息、任务、风险和向量分块，Redis 承载缓存与 asynq 队列。
- 问答先检索 Top K 分块，再交给 OpenAI 兼容的 ChatModel 生成带引用回答。
- 日志使用 slog 结构化输出，LLM 调用记录 tokens、延迟和失败状态。

## 安全与权限

当前暂无鉴权，WebSocket 使用 user 与 group_id 临时标识，后续替换为真实认证。

## 可观测性

访问日志记录 request_id、method、path、status、latency 和 group_id；问答与定时任务统一记录 llm_usage。
