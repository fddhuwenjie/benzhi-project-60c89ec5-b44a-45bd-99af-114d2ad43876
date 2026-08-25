# 文物修复质量放行服务

面向文物修复负责人、技师、质检员和保护专家，提供项目登记、方案基线、工序证据、质检整改、专家放行与不可变归档的闭环 HTTP JSON API。

## 构建、运行与测试

```bash
go test ./...
go run ./cmd/server -addr=127.0.0.1:19081
go run ./cmd/server -self-check -addr=127.0.0.1:19081
```

服务默认监听 `127.0.0.1:19081`，可用 `-addr=127.0.0.1:<port>` 或 `PORT` 环境变量覆盖。数据默认写入当前目录 `data`，可通过 `DATA_DIR` 环境变量指定。

## 主要接口

`POST /v1/projects` 登记项目，`PUT /v1/projects/{id}/baseline` 锁定方案基线，`POST /v1/projects/{id}/procedures` 创建工序，`POST /v1/projects/{id}/procedures/{procedureId}/complete` 完成工序并提交证据；质检使用 `/inspections`，整改使用 `/remediations`，专家放行使用 `/release-requests`，并可通过 `/timeline` 和 `/archives` 查询。

项目检索支持 `asset_prefix`、`custodian`、`created_since`、`created_until` 与分页参数，返回 `status_counts`；项目详情附带 `procedures_summary`（完成度和下一工序）。基线、登记、质检和放行请求支持幂等标识，证据提交会校验时间范围、SHA-256 格式及采集元数据。归档清单支持版本、时间区间和分页，并在校验和异常时返回 `integrity_error` 诊断。

新增能力包括 `asset_code` 精确检索与重复登记冲突、`/baseline/history` 修订历史、`/procedures/reorder` 排程重排，以及工序 `pause`/`resume` 状态和有效工时。仪器证据会校验设备校准有效期；质检批次可通过 `/inspections/{id}/freeze` 冻结并保留覆盖统计。整改支持延期审批和责任转派，放行请求可用 `reviewer_roles` 校验高风险保护专家或中风险质量负责人门槛。

基线接口支持 `preflight` 预检；批量工序可通过 `validate_only` 诊断顺序、重名和技师告警。项目证据核验可查询 `/v1/projects/{id}/evidence-verification`，整改队列支持分页、逾期升级及 `/remediations/resolve` 批量复核。质检请求支持抽检工序字段，高风险项目按覆盖门槛校验；放行请求支持 `preflight` 报告指纹，归档查询附带证据根、校验和及发布事件证明视图。

材料基线支持名称、批次、用量和单位的规范化及逐字段预检诊断；批量工序支持 `workload_limit` 容量闸门，超限返回技师与负荷明细。工序完成可提交结构化 `temperature`、`humidity`、`illuminance` 环境参数，异常会阻止进入质检。证据核验附加 `coverage=true` 可查询按工序和类型的覆盖缺口；质检抽样校验工序与证据绑定。整改复核支持 `decision=approve|reject`（驳回需填写 `reason`），时间线支持 `aggregate=true` 返回动作、操作者统计。
