# 天穹技术架构

## 服务划分

采集、存储、查询三层独立部署，灰度期间新旧集群双写。

## 数据与检索

- 天穹的 Markdown 分块约 800 字符/块、100 字符重叠，索引写入 Postgres pgvector。
- Kafka 使用 3 个 broker，ClickHouse 使用 3 个分片，热数据保留 30 天，冷数据保留 180 天。
- OpenTelemetry Collector 负责日志、指标和追踪接入，Grafana 提供统一查询入口。
- 平台日志使用结构化 JSON，查询与告警链路记录 trace_id、租户和耗时。

## 部署与回滚

采集、存储、查询三层独立部署，灰度期间新旧集群双写。

## 安全与权限

多租户通过租户 ID 和资源配额隔离，内测阶段使用平台组白名单。

## 可观测性

平台自身也接入 Prometheus，监控 Kafka 堆积、ClickHouse 磁盘和告警送达延迟。
