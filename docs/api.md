# ForgeGrid V0 HTTP API

ForgeGrid V0 提供严格 JSON-over-HTTP API。本文记录当前代码的 wire contract，不包含尚未实现的规划接口。

## 1. 通用约定

- 默认地址：`http://127.0.0.1:8080`
- mutation body 必须是单个 JSON object；若提供 `Content-Type`，必须为 `application/json`
- body 最大 1 MiB；未知字段、第二个 JSON value 和尾随非空内容会被拒绝
- 客户端可传 `X-Request-ID`；服务端在响应中返回 request ID
- create experiment 与 create run 支持 `Idempotency-Key`
- list 接口的 `limit` 默认 100，允许 1–1000
- 时间使用 RFC 3339 JSON 格式；容量字段统一使用 bytes
- JSON 响应设置 `Cache-Control: no-store`

`resource_requirements`、Worker `resources` 和 claim `allocation` 是值对象，客户端应允许它们以空对象或零值字段出现；不要用字段是否存在来判断 Run 是否需要 GPU，应读取 `gpu_count`。

`Idempotency-Key` trim 后长度必须为 1–128，且匹配：

```text
[A-Za-z0-9][A-Za-z0-9._:/-]*
```

同一 key 和同一 payload 在服务重启后仍返回原资源；同一 key 配合不同 payload 返回 `409 conflict`。成功 replay 仍返回 create 接口的 `201`。

## 2. 鉴权

设置服务端环境变量后，`/v1/*`、`/v1` 和 `/metrics` 要求 bearer token：

```http
Authorization: Bearer <CONTROL_PLANE_API_TOKEN>
```

`/healthz`、`/readyz` 和静态 UI 不鉴权。控制面监听非 loopback 地址时，如果没有配置 token 会拒绝启动。

## 3. Endpoint 概览

### Operator / query

| Method | Path | 成功响应 | 作用 |
| --- | --- | ---: | --- |
| `GET` | `/healthz` | 200 | 进程存活与版本 |
| `GET` | `/readyz` | 200 | snapshot invariant/readiness |
| `GET` | `/metrics` | 200 | Prometheus text metrics |
| `POST` | `/v1/experiments` | 201 | 创建 Experiment |
| `GET` | `/v1/experiments?limit=N` | 200 | 列出 Experiment |
| `GET` | `/v1/experiments/{experiment_id}` | 200 | 读取 Experiment |
| `POST` | `/v1/experiments/{experiment_id}/runs` | 201 | 创建 Run |
| `GET` | `/v1/experiments/{experiment_id}/runs?limit=N` | 200 | 列出 Run |
| `GET` | `/v1/runs/{run_id}` | 200 | 读取 Run 当前状态 |
| `GET` | `/v1/runs/{run_id}/attempts?limit=N` | 200 | 读取 Attempt 历史与 allocation evidence |
| `POST` | `/v1/runs/{run_id}/cancel` | 200 | 请求取消 |
| `GET` | `/v1/runs/{run_id}/events?after=N&limit=N` | 200 | 游标式读取 Event |

### Worker protocol

| Method | Path | 成功响应 | 作用 |
| --- | --- | ---: | --- |
| `POST` | `/v1/workers` | 201 | 注册/刷新 Worker 与 session/resources |
| `GET` | `/v1/workers?limit=N` | 200 | 列出 Worker 与动态 offline 投影 |
| `POST` | `/v1/workers/{worker_id}/heartbeat` | 200 | 更新状态、能力与资源 |
| `POST` | `/v1/workers/{worker_id}/claim` | 200/204 | 领取一次 Attempt/allocation |
| `POST` | `/v1/runs/{run_id}/attempts/{attempt_id}/start` | 200 | 确认进程启动 |
| `POST` | `/v1/runs/{run_id}/attempts/{attempt_id}/heartbeat` | 200 | 续租 Attempt |
| `POST` | `/v1/runs/{run_id}/attempts/{attempt_id}/complete` | 200 | 提交终态结果 |

所有 list 接口返回：

```json
{
  "items": [],
  "count": 0
}
```

## 4. Experiment

### Create

```http
POST /v1/experiments
Content-Type: application/json
Idempotency-Key: forgegrid-demo-exp
```

```json
{
  "name": "SGLang latency comparison",
  "description": "Compare two serving configurations",
  "labels": {
    "team": "inference"
  }
}
```

`name` trim 后必填。响应：

```json
{
  "id": "experiment_...",
  "name": "SGLang latency comparison",
  "description": "Compare two serving configurations",
  "labels": {
    "team": "inference"
  },
  "created_at": "2026-08-12T08:00:00Z",
  "updated_at": "2026-08-12T08:00:00Z"
}
```

## 5. Run 与 resource requirements

### Create

```http
POST /v1/experiments/experiment_123/runs
Content-Type: application/json
Idempotency-Key: serving-a-run-1
```

```json
{
  "recipe": {
    "adapter": "sglang.serving-benchmark",
    "command": [
      "--base-url", "http://127.0.0.1:30000",
      "--dataset-name", "random",
      "--num-prompts", "100",
      "--disable-tqdm"
    ],
    "working_dir": "/srv/bench",
    "environment": {
      "PYTHONUNBUFFERED": "1"
    },
    "expected_outputs": [],
    "timeout_seconds": 1800
  },
  "required_labels": {
    "os": "linux",
    "accelerator": "nvidia",
    "pool": "gpu-lab"
  },
  "resource_requirements": {
    "gpu_count": 1,
    "min_free_gpu_memory_bytes": 17179869184
  },
  "priority": 10,
  "max_attempts": 2
}
```

验证规则：

- `recipe.adapter` 必填；`recipe.command` 至少一项且每一项非空；
- `timeout_seconds >= 0`；
- `max_attempts` 省略或为 0 时默认 3，负数非法；
- `gpu_count` 为 0–16；
- `min_free_gpu_memory_bytes >= 0`；
- 显存门槛大于 0 时，`gpu_count` 必须大于 0；
- path 中的 Experiment ID 覆盖 body 中可能出现的 `experiment_id`。

Run 响应示例：

```json
{
  "id": "run_...",
  "experiment_id": "experiment_123",
  "state": "queued",
  "recipe": {
    "adapter": "sglang.serving-benchmark",
    "command": ["--base-url", "http://127.0.0.1:30000", "--dataset-name", "random", "--num-prompts", "100", "--disable-tqdm"],
    "working_dir": "/srv/bench",
    "environment": {
      "PYTHONUNBUFFERED": "1"
    },
    "timeout_seconds": 1800
  },
  "required_labels": {
    "os": "linux",
    "accelerator": "nvidia",
    "pool": "gpu-lab"
  },
  "resource_requirements": {
    "gpu_count": 1,
    "min_free_gpu_memory_bytes": 17179869184
  },
  "priority": 10,
  "max_attempts": 2,
  "attempt_count": 0,
  "created_at": "2026-08-12T08:00:00Z",
  "updated_at": "2026-08-12T08:00:00Z"
}
```

Run `state` 为 `queued`、`active`、`cancel_requested`、`succeeded`、`failed` 或 `cancelled`。`cancel_requested` 是 active Run 在 API 层的投影。

执行后 Run 还可能包含：

```json
{
  "active_attempt_id": "attempt_...",
  "cancel_requested_at": "2026-08-12T08:01:00Z",
  "cancellation_reason": "operator stop",
  "finished_at": "2026-08-12T08:02:00Z",
  "result": {
    "outcome": "succeeded",
    "exit_code": 0,
    "metrics": {
      "request_throughput": 42.5
    },
    "artifacts": [
      {
        "name": "benchmark.jsonl",
        "uri": "file:///srv/artifacts/benchmark.jsonl",
        "sha256": "...",
        "size_bytes": 2048
      }
    ]
  },
  "last_allocation": {
    "worker_id": "gpu-node-01",
    "gpu_ids": ["GPU-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"],
    "gpus": [
      {
        "id": "GPU-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
        "index": 0,
        "name": "NVIDIA RTX 4090",
        "vendor": "NVIDIA",
        "total_memory_bytes": 25769803776,
        "free_memory_bytes": 21474836480,
        "utilization_percent": 4,
        "temperature_celsius": 43
      }
    ],
    "resources_observed_at": "2026-08-12T08:00:01Z"
  }
}
```

`last_allocation` 是最近一次 Attempt 的快照。若需要每次重试的历史，应读取 Attempt 列表。

## 6. Attempt list

```http
GET /v1/runs/run_123/attempts?limit=100
```

按 `number` 升序返回：

```json
{
  "items": [
    {
      "id": "attempt_1",
      "run_id": "run_123",
      "number": 1,
      "worker_id": "gpu-node-01",
      "state": "lost",
      "fence": 1,
      "lease_expires_at": "2026-08-12T08:00:31Z",
      "created_at": "2026-08-12T08:00:01Z",
      "updated_at": "2026-08-12T08:00:33Z",
      "finished_at": "2026-08-12T08:00:33Z",
      "result": {
        "outcome": "failed",
        "error_message": "lease expired"
      },
      "allocation": {
        "worker_id": "gpu-node-01",
        "gpu_ids": ["GPU-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"],
        "gpus": [
          {
            "id": "GPU-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
            "index": 0,
            "name": "NVIDIA RTX 4090",
            "vendor": "NVIDIA",
            "total_memory_bytes": 25769803776,
            "free_memory_bytes": 21474836480,
            "utilization_percent": 4,
            "temperature_celsius": 43
          }
        ],
        "resources_observed_at": "2026-08-12T08:00:01Z"
      }
    }
  ],
  "count": 1
}
```

Attempt state 为 `leased`、`running`、`succeeded`、`failed`、`cancelled` 或 `lost`。该接口会返回 fence 和 allocation，但**永远不返回 lease token**。目前没有单个 Attempt 的 GET endpoint。

## 7. Worker registration 与 resources

### GPU resource schema

```json
{
  "probe_status": "ok",
  "observed_at": "2026-08-12T08:00:00Z",
  "gpus": [
    {
      "id": "GPU-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
      "index": 0,
      "name": "NVIDIA RTX 4090",
      "vendor": "NVIDIA",
      "total_memory_bytes": 25769803776,
      "free_memory_bytes": 21474836480,
      "utilization_percent": 4,
      "temperature_celsius": 43
    }
  ]
}
```

`probe_status` 允许空字符串、`ok`、`unavailable`、`error`。空字符串和 `unavailable` 不得携带 GPU；`error` 可保留 last-good GPU 快照用于诊断，但只有 `ok` 能参与调度。每张 GPU：

- `id`、`name`、`vendor` 必填；同一次报告中 ID 和 index 均不可重复；
- `index >= 0`；
- `0 <= free_memory_bytes <= total_memory_bytes`；
- utilization 为有限的 0–100；temperature 为有限的 0–200；

### Register

```http
POST /v1/workers
Content-Type: application/json
```

```json
{
  "id": "gpu-node-01",
  "name": "gpu-node-01",
  "adapter": "sglang.serving-benchmark",
  "labels": {
    "os": "linux",
    "arch": "amd64",
    "accelerator": "nvidia",
    "pool": "gpu-lab"
  },
  "version": "dev",
  "session_id": "a4d04d5b-4247-4d84-bcd9-593ad64bfa7a",
  "resources": {
    "probe_status": "ok",
    "gpus": [
      {
        "id": "GPU-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
        "index": 0,
        "name": "NVIDIA RTX 4090",
        "vendor": "NVIDIA",
        "total_memory_bytes": 25769803776,
        "free_memory_bytes": 21474836480,
        "utilization_percent": 4,
        "temperature_celsius": 43
      }
    ]
  }
}
```

`name`、`adapter`、`session_id` 必填；`id` 可省略，由服务端生成。复用 Worker ID 时 adapter 不可变。如果该 Worker 仍有 active allocation，另一个 session 注册同一 ID 返回 `409 conflict`。

服务端以收到报告的当前时间覆盖客户端提供的 `resources.observed_at`，同时更新 `last_heartbeat_at`。响应 Worker：

```json
{
  "id": "gpu-node-01",
  "name": "gpu-node-01",
  "adapter": "sglang.serving-benchmark",
  "labels": {
    "os": "linux",
    "arch": "amd64",
    "accelerator": "nvidia",
    "pool": "gpu-lab"
  },
  "state": "online",
  "version": "dev",
  "resources": {
    "probe_status": "ok",
    "observed_at": "2026-08-12T08:00:01Z",
    "gpus": []
  },
  "active_run_id": "run_...",
  "active_attempt_id": "attempt_...",
  "last_heartbeat_at": "2026-08-12T08:00:01Z",
  "registered_at": "2026-08-12T08:00:01Z"
}
```

上例为简洁省略了 GPU 数组内容；真实响应回显经过验证的完整 GPU 列表。
响应不会回显 `session_id`；它是 worker 进程世代凭据，而不是控制台展示字段。

### Worker heartbeat

```http
POST /v1/workers/gpu-node-01/heartbeat
Content-Type: application/json
```

```json
{
  "session_id": "a4d04d5b-4247-4d84-bcd9-593ad64bfa7a",
  "state": "online",
  "labels": {
    "os": "linux",
    "arch": "amd64",
    "accelerator": "nvidia",
    "pool": "gpu-lab"
  },
  "version": "dev",
  "resources": {
    "probe_status": "ok",
    "gpus": []
  }
}
```

正式协议必须发送与注册记录匹配的 `session_id`；省略或错误 session 会返回 `409 conflict`。JSON body 在语法上仍可为空，仅用于兼容早期空 session 的 store 测试数据。可选字段的更新规则：

- 省略 `state`、`labels`、`version`、`resources` 时保留旧值；
- 显式 `labels: {}` 会清空 labels；
- 空 `version` 不会清空已有版本；
- resources 省略时保留旧 observation，因此节点 heartbeat 可能 fresh，而 GPU observation 已 stale。

Worker state 可为 `online`、`draining`、`offline`。默认 30 秒未收到 heartbeat 的 Worker，在 list 响应中动态显示为 `offline`；stale Worker 不能 claim。TTL 可由 `-worker-stale-after` 配置。

## 8. Claim 与 allocation

```http
POST /v1/workers/gpu-node-01/claim?lease_ttl=30s&wait=20s
Content-Type: application/json

{"session_id":"a4d04d5b-4247-4d84-bcd9-593ad64bfa7a"}
```

- body 中的 `session_id` 必须与注册 session 精确匹配；服务端暂时兼容旧客户端的 query 参数，但官方 Worker 不把 fencing credential 放进 URL；
- `lease_ttl` 默认 30 秒，范围 1 秒–10 分钟；
- `wait` 默认 0，范围 0–30 秒；
- 无合格工作时返回 `204 No Content`；
- Worker 不存在返回 404；非 online、stale 或 session 不匹配返回 409；
- Worker 已有 active Attempt 时按“当前无工作”处理，等待结束后返回 204。

成功响应：

```json
{
  "attempt_id": "attempt_...",
  "run_id": "run_...",
  "fence": 2,
  "lease_token": "明文只返回这一次",
  "expires_at": "2026-08-12T08:00:31Z",
  "recipe": {
    "adapter": "sglang.serving-benchmark",
    "command": ["--base-url", "http://127.0.0.1:30000", "--dataset-name", "random", "--num-prompts", "100", "--disable-tqdm"]
  },
  "allocation": {
    "worker_id": "gpu-node-01",
    "gpu_ids": ["GPU-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"],
    "gpus": [
      {
        "id": "GPU-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
        "index": 0,
        "name": "NVIDIA RTX 4090",
        "vendor": "NVIDIA",
        "total_memory_bytes": 25769803776,
        "free_memory_bytes": 21474836480,
        "utilization_percent": 4,
        "temperature_celsius": 43
      }
    ],
    "resources_observed_at": "2026-08-12T08:00:01Z"
  }
}
```

资源匹配要求每张所选 GPU 的 `free_memory_bytes` 都达到门槛。调度器在这个 worker 内按空闲显存升序、GPU ID 升序选择设备；这是 worker 内 best-fit，不是跨 worker 的全局 best-fit。

claim 成功时，Run、Attempt、Worker reservation、token hash、fence 和 allocation 在一次 JSON snapshot mutation 中一起持久化。V0 一个 Worker 同时最多一个 active allocation。

## 9. Attempt mutation

### Start

```http
POST /v1/runs/run_123/attempts/attempt_1/start
Content-Type: application/json
```

```json
{
  "lease_token": "...",
  "fence": 1
}
```

`lease_token` 非空且 fence 必须为正数。`leased → running`；重复 start 一个仍为 running 的 Attempt 是幂等 200。响应为当前 Run。

### Heartbeat lease

```http
POST /v1/runs/run_123/attempts/attempt_1/heartbeat
Content-Type: application/json
```

```json
{
  "lease_token": "...",
  "fence": 1,
  "extend_seconds": 30
}
```

`extend_seconds` 为 1–600。新的 expiry 是“服务端当前时间 + extension”，而不是在旧 expiry 上累加。响应：

```json
{
  "attempt_id": "attempt_1",
  "run_id": "run_123",
  "fence": 1,
  "expires_at": "2026-08-12T08:01:00Z"
}
```

### Complete

```http
POST /v1/runs/run_123/attempts/attempt_1/complete
Content-Type: application/json
```

```json
{
  "lease_token": "...",
  "fence": 1,
  "outcome": "succeeded",
  "exit_code": 0,
  "metrics": {
    "request_throughput": 42.5,
    "mean_ttft_ms": 18.2
  },
  "artifacts": [
    {
      "name": "benchmark.jsonl",
      "uri": "file:///srv/artifacts/benchmark.jsonl",
      "sha256": "...",
      "size_bytes": 2048
    }
  ]
}
```

HTTP 接口允许 `outcome` 为 `succeeded`、`failed` 或 `cancelled`。失败或取消时可传 `error`。正常协议先 start 再 complete；`leased` 不能直接转换为终态。

失败且还有 attempt budget 时，响应 Run 回到 `queued`；否则为 `failed`。成功时为 `succeeded`。HTTP 响应是 Run，不单独暴露内部 `retry_scheduled`。

完成回调具有持久重放语义：相同 path、token、fence 和完全相同 payload 可在完成后或服务重启后重复提交；更改 payload 返回 `409 conflict`；错误凭据返回 `409 lease_lost`。

## 10. Lease 与 fence 授权

start、Attempt heartbeat 和 complete 必须同时满足：

- Attempt 存在且属于 path 中的 Run；
- Attempt 为 active 状态；
- Run 为 active 且 `active_attempt_id` 对应；
- fence 与该 Attempt 相同；
- lease 尚未过期；
- lease token hash 匹配。

任一条件失败统一返回 `409 lease_lost`，包括不存在的 Attempt ID。token 明文只在 claim 中返回；服务端 snapshot 只保存 hash。fence 在每个 Run 内随新 Attempt 单调递增。

## 11. Cancel 与 Events

### Cancel

```http
POST /v1/runs/run_123/cancel
Content-Type: application/json
```

body 可空，或：

```json
{
  "reason": "operator stop"
}
```

没有 active Attempt 的 queued Run 立即 cancelled。active Run 对外显示 `cancel_requested`，等待 worker 协作停止或 lease reconciliation。重复取消已 cancelled Run 返回 200；取消 succeeded/failed Run 返回 409。

### Events

```http
GET /v1/runs/run_123/events?after=17&limit=100
```

`after` 默认为 0，必须为非负整数；只返回 `sequence > after`。Event：

```json
{
  "sequence": 18,
  "run_id": "run_123",
  "attempt_id": "attempt_1",
  "type": "attempt.leased",
  "timestamp": "2026-08-12T08:00:01Z",
  "data": {
    "worker_id": "gpu-node-01",
    "fence": 1,
    "expires_at": "2026-08-12T08:00:31Z",
    "gpu_ids": ["GPU-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"]
  }
}
```

当前事件类型包括 `run.created`、`attempt.leased`、`attempt.started`、`attempt.completed`、`run.cancel_requested`、`attempt.lease_expired`。

## 12. Error envelope

```json
{
  "error": {
    "code": "lease_lost",
    "message": "lease is expired, fenced, or owned by another worker",
    "request_id": "..."
  }
}
```

| Code | HTTP | 含义 |
| --- | ---: | --- |
| `invalid_argument` | 400 | 类型化输入或 query 非法 |
| `invalid_json` | 400 | body 不是一个只含已知字段的 JSON object |
| `invalid_idempotency_key` | 400 | idempotency key 格式非法 |
| `unauthorized` | 401 | bearer token 缺失或不匹配 |
| `not_found` | 404 | 资源不存在 |
| `conflict` | 409 | session、状态或 idempotency 冲突 |
| `lease_lost` | 409 | token、fence、expiry 或 active Attempt 校验失败 |
| `body_too_large` | 413 | body 超过 1 MiB |
| `unsupported_media_type` | 415 | Content-Type 不是 application/json |
| `unavailable` | 503 | readiness 依赖不可用 |
| `internal` | 500 | 未分类服务端错误 |

上表适用于 ForgeGrid handler 与 service 生成的错误。HTTP 路由器自身生成的 `405 Method Not Allowed` 不保证使用 JSON envelope。

V0 API 没有 tenant、checkpoint migration、P2P topology、全局最优 placement 或多 Worker gang scheduling 字段。完整边界见 [设计说明](design.md)。
