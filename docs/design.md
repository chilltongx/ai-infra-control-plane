# ForgeGrid V0 设计说明

## 1. 产品目标

ForgeGrid 面向已经拥有少量异构机器、但还没有可靠实验调度面的个人和小团队。它把“在哪台机器上运行、当时有哪些 GPU、为什么选中这些 GPU、这个结果是否来自仍有效的执行权”变成可查询的系统事实。

V0 的最小闭环是：

1. worker 主动连接控制面并报告稳定能力与动态 GPU 容量；
2. Run 用结构化字段声明 GPU 数量与每卡空闲显存门槛；
3. claim 原子生成 lease、fence、worker reservation 与 allocation evidence；
4. worker 只执行类型化 recipe，并将 allocation 转换为 `CUDA_VISIBLE_DEVICES`；
5. 结果、事件、artifact 元数据和每次 Attempt 的放置证据可在失败重试后审计。

V0 的部署模型是 **一 node = 一 worker = 同时最多一个 active Attempt/allocation**。Worker 同时承担节点身份、资源探针和执行器角色，尚未拆成独立的 Node 与 Executor 资源。

## 2. 系统边界

控制面负责：

- Experiment、Run、Attempt、Worker、Event 的持久化
- 优先级/FIFO、adapter、静态标签与动态 GPU 资源匹配
- worker reservation、lease、fence、重试预算与取消意图
- completion 接受、重放收据、结果和 allocation evidence
- Web/CLI/API 查询与 Prometheus text metrics

worker 负责：

- 向控制面发起出站注册、heartbeat 和 claim
- 直接运行 `nvidia-smi` 并提交结构化资源报告
- 把 allocation 转换为进程环境
- 以 argv 形式启动受支持的类型化 recipe，不经过 shell
- attempt heartbeat、取消轮询、进程终止、指标解析和 artifact 哈希

控制面不会 SSH 到节点，也不会直接执行用户 workload。SGLang、Kubernetes 和其他系统是受控集成边界，不是第二套调度器。

现有集成的精确边界：

| 集成 | V0 中实际负责的内容 |
| --- | --- |
| `demo.sleep` | 重新执行 worker binary 的内部 sleep 子命令，提供跨平台端到端验证 |
| `sglang.serving-benchmark` | 生成 allowlisted argv、控制 JSONL 输出位置、解析指标和 artifact |
| `kube-adapter` | 确定性渲染 one-shot Worker Job；不是 operator，不负责集群扩缩容 |
| macOS bundle | 在本机启动单 controlplane、WebKit UI 和一个 demo worker；不自动启动 GPU/Windows/Kubernetes worker |

## 3. 领域资源

| 资源 | 责任 |
| --- | --- |
| Experiment | 用户可见的实验分组和元数据 |
| Run | 一个逻辑任务、recipe、placement 需求和重试预算 |
| Attempt | 一次物理执行；持有 worker、fence、lease 过期时间、结果和 allocation |
| Worker | V0 中的一台 node/executor；稳定 ID、进程 session、adapter、标签、动态资源与 active reservation |
| WorkerResources | 一次资源观测：probe 状态、服务端记录的观测时间和 GPU 列表 |
| ResourceAllocation | claim 时冻结的 worker/GPU 证据；后续 heartbeat 不会改写 |
| Lease | Attempt 的临时执行权：随机 token、单调 fence 和 expiry |
| Event | 带全局递增 sequence 的 append-only Run 生命周期记录 |
| Artifact | name、URI、SHA-256、bytes；V0 指向 worker 本地文件 |

Run 与 Attempt 的区别很重要：Run 表示用户意图，Attempt 表示某一次实际执行。Run 的 `last_allocation` 方便快速查看最近放置；`GET /v1/runs/{id}/attempts` 才是跨重试的完整历史。

## 4. 稳定能力与动态资源分离

worker labels 用于相对稳定的离散属性，例如：

```json
{
  "os": "linux",
  "arch": "amd64",
  "accelerator": "nvidia",
  "pool": "gpu-lab"
}
```

GPU 空闲显存、利用率和温度不会编码成 label，而是放入 `resources`：

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

官方 worker 每次直接执行：

```text
nvidia-smi --query-gpu=index,uuid,name,memory.total,memory.free,utilization.gpu,temperature.gpu --format=csv,noheader,nounits
```

调用有 5 秒超时，CSV 中的 MiB 乘以 `1024²` 转为 bytes。找不到命令时状态为 `unavailable`；执行或解析失败时为 `error`；只有 `ok` 报告可以满足 GPU Run。runner 可在错误报告中保留 last-good GPU 数据用于诊断，但调度器仍会拒绝它，避免把旧数据当成可用容量。

服务端收到注册/heartbeat 后，以服务端当前时间覆盖客户端的 `resources.observed_at`，因此 freshness 代表“控制面何时收到这份报告”，而不是依赖节点时钟。

## 5. Placement 与 reservation

Run 的动态资源需求目前只有：

```json
{
  "gpu_count": 2,
  "min_free_gpu_memory_bytes": 17179869184
}
```

约束如下：

- `gpu_count` 为 0–16；
- 最小空闲显存不能为负；
- 设置显存门槛时 `gpu_count` 必须大于 0；
- 显存门槛作用于每一张 GPU，而不是总和。

一次 worker claim 的调度步骤：

1. 验证 Worker 存在、显式状态为 `online`、session 匹配且 heartbeat fresh；
2. 检查该 Worker 没有任何 active Attempt；
3. 收集 `queued`、无 active Attempt、未耗尽次数、adapter 相同且 labels 匹配的 Run；
4. 对 GPU Run，要求资源 `probe_status=ok` 且 observation 使用同一 freshness TTL；
5. 过滤每卡空闲显存达到门槛的 GPU；
6. 在该 Worker 内按 `free_memory_bytes` 升序、GPU ID 升序排序，选择前 `gpu_count` 张；
7. 在合格 Run 中按 priority 降序、created time 升序、Run ID 升序选择一个；
8. 原子更新 Run、Attempt、Worker reservation、lease token hash 和 Event。

选择更紧凑的合格 GPU，可以把空闲显存更大的卡留给后续大任务。这是 **worker 内 best-fit**。由于 worker 主动 pull，V0 不做跨 worker 的全局 best-fit：哪个合格 worker 先 claim，哪个 worker 就可能得到 Run。

`gpu_count=0` 的 Run 不依赖 GPU probe 状态或资源 freshness。它仍会创建不含 GPU 列表的 allocation（包含 `worker_id`，并记录当时可用的 observation 时间），同时占用该 worker 的唯一执行槽。

## 6. Allocation evidence

claim 时冻结：

- `worker_id`
- 所选 GPU UUID 列表
- 每张 GPU 当时的完整资源字段，包括 index、型号和空闲显存
- `resources_observed_at`

同一个 allocation 同时写入 `Attempt.allocation` 与 `Run.last_allocation`。后续 heartbeat 更新 Worker 的实时资源，但不会改写已存在 Attempt 的证据。重试会创建新的 Attempt 和新的 allocation；旧 Attempt 保持不可变终态记录。

worker 使用 allocation 中的 GPU index 生成 `CUDA_VISIBLE_DEVICES`，并删除 recipe 环境中大小写不同的同名旧值。没有 GPU allocation 时保留 recipe 原环境。负数或重复 index 会让执行失败，而不会猜测设备。

allocation 证明的是“调度决策时控制面掌握的资源快照”，不是硬件层排他锁。V0 依赖一 node 一个 worker 的部署约束；控制面外的进程仍可能使用这些 GPU。

## 7. Worker session 与 freshness

稳定 Worker ID 表示节点身份，`session_id` 表示当前 worker 进程世代。官方 worker 未配置时生成 UUIDv4。

- Register、heartbeat、claim 都携带 session；
- heartbeat 和 claim 必须与已注册 session 精确匹配；
- Worker 有 active allocation 时，另一个 session 不能用相同 Worker ID 重新注册；
- Attempt start/heartbeat/complete 不再依赖 session，而由 lease token + fence 授权。

默认 freshness TTL 为 30 秒，可通过 controlplane 的 `-worker-stale-after` 调整。`last_heartbeat_at + TTL <= now` 时：

- `GET /v1/workers` 将该节点动态投影为 `offline`；
- claim 返回 conflict；
- GPU observation 也不能参与 placement。

offline 投影不会单独写回 snapshot；同一 session 恢复 heartbeat 后可以重新变为 fresh。若 heartbeat 不携带 resources，节点 heartbeat 可以 fresh，但旧 GPU observation 仍会过期并阻止 GPU Run。

## 8. Lease、Fence 与 at-least-once

Claim 在同一次持久化 mutation 中：

1. 为 Run 递增 fence 和 attempt count；
2. 创建 `leased` Attempt；
3. 生成随机 lease token，仅持久化 SHA-256 hash；
4. 设置 lease expiry；
5. 将 Run 置为 active；
6. 写入 Worker active Run/Attempt 指针；
7. 保存 allocation evidence 和 `attempt.leased` Event。

start、attempt heartbeat、complete 都必须携带 `(run_id, attempt_id, fence, lease_token)`。授权同时验证：

- path 中的 Run/Attempt 对应；
- Attempt 仍为 `leased` 或 `running`；
- Run 仍 active 且 active Attempt 相同；
- fence 等于当前 Attempt fence；
- expiry 严格晚于服务端当前时间；
- token hash 常量时间匹配。

任一条件失败都返回 `409 lease_lost`。每个新 Attempt 使用更大的 fence，因此旧进程无法提交到新的执行世代。

系统提供 at-least-once dispatch，不承诺 exactly-once execution。网络隔离后旧进程可能暂时继续消耗计算；fencing 保护的是控制面状态提交，不能直接杀死不可达进程。

## 9. 重试、完成与取消

控制面定时回收过期 lease（默认每 2 秒 sweep）：

- Attempt → `lost`；
- 删除 active token hash；
- 释放 Worker reservation；
- 有剩余次数时 Run → `queued`，否则 Run → `failed`；
- 已请求取消时 Run → `cancelled`。

失败完成同样由控制面决定是否重试。每次重试产生新 Attempt、新 fence 和新 allocation；旧 Attempt 不被覆盖。

成功或失败 completion 会写入持久 completion receipt，其中包含 token hash、请求指纹和原始响应。响应丢失后，以相同凭据重放完全相同 payload 会得到原响应且不重复发 Event；更改 payload 返回 conflict；错误凭据返回 lease lost。

取消是期望状态与观测状态的分离：queued Run 可立即 cancelled；active Run 先对外显示 `cancel_requested`，worker 轮询到后取消进程并完成。若 success 与取消竞争，取消意图获胜，迟到 success 被审计但结果归一化为 cancelled。

## 10. Store strategy

JSONStore 为每次 mutation 克隆当前 snapshot，验证跨资源不变量，将权限设为 `0600` 的临时文件写入并 `fsync`，原子 rename 后同步目录，最后才发布新的内存 revision。

这能支持单个 Go 进程中的并发 goroutine 和进程重启恢复，但它不是：

- 多进程文件锁；
- 多副本共享数据库；
- 高可用或线性扩展存储；
- 对象存储。

如果进入下一阶段，PostgreSQL 应用短事务、`FOR UPDATE SKIP LOCKED`、active attempt/worker reservation 的部分唯一约束，并保留 token hash、idempotency record、completion receipt、event 与 allocation 的领域契约。

## 11. Execution 与安全边界

- 公开 recipe 是类型化 adapter，不是任意 shell；
- worker 使用 `exec.CommandContext` 运行 argv，不调用 shell；
- `demo.sleep` 重新执行当前 worker binary 的内部命令，因此不依赖平台 `sleep`；
- SGLang 只接受显式 allowlist 参数，worker 自己决定 output path；
- expected artifact 在 symlink 解析后必须仍位于 working directory；
- stdout、stderr 和预期文件记录 URI、size 与 SHA-256；
- 非 loopback 监听必须配置共享 bearer token；token 不写日志；
- lease token 明文只出现在一次 claim 响应中，持久层只保存 hash。

共享 API token 只是部署级保护。V0 没有 tenant identity、tenant RBAC、quota 或隔离账本，远程连接仍需要 TLS 和网络层控制。

## 12. V0 非目标

以下能力当前没有实现，也不应从 V0 文档推断存在：

- workload checkpoint、断点恢复、跨节点 migration 或 preemption；
- GPU P2P/NVLink/PCIe/NUMA 拓扑感知；
- 多节点 gang scheduling 或分布式训练 rendezvous；
- 同 Worker 多任务并发、跨多个 Worker 进程的物理 GPU 锁；
- MIG、fractional GPU、time-slicing 或外部 workload 检测；
- 多租户、细粒度 RBAC、quota、fairness 或计费；
- controlplane 多副本、HA、PostgreSQL 或 S3-compatible artifact replication；
- 任意容器运行时或任意用户命令。

架构视图见 [ForgeGrid V0 Architecture](control-plane-architecture.html)，Run 状态转换见 [Run Lifecycle](run-lifecycle.html)，wire contract 见 [HTTP API](api.md)。
