# ForgeGrid

ForgeGrid 是一个用 Go 编写的、小型但真实可用的 GPU 实验控制面：把分散在 Linux、Windows 和 macOS 机器上的执行进程组织成一个可观察、可追溯的实验网格。

它解决的不是“再造 Kubernetes”，而是一个更具体的问题：团队已经有几台能跑实验的机器，却缺少统一的任务入口、动态 GPU 容量判断、可靠的租约、失败重试和结果证据。ForgeGrid V0 让 worker 主动向控制面出站连接，报告 GPU 状态并领取与自身能力匹配的任务。

> V0 的部署假设是 **一台 node 运行一个 ForgeGrid worker，并且一个 worker 同时只持有一个 allocation**。不要在同一台 GPU 主机上启动多个 worker 进程；V0 不会跨进程协调同一块物理 GPU。

## V0 已实现

- Go REST API、`expctl` CLI 和内嵌 Web 控制台
- Experiment → Run → Attempt 的可追溯执行模型
- worker 主动注册、心跳和长轮询 claim；控制面不需要反向 SSH
- 直接执行 `nvidia-smi` 的结构化 GPU 探测，不经过 shell
- GPU UUID、index、型号、厂商、总/空闲显存、利用率、温度上报
- Run 声明 `gpu_count` 和“每张 GPU 最少空闲显存”
- 在当前 worker 内按空闲显存升序选择 GPU 的 best-fit 分配
- claim 时原子写入 worker reservation、attempt、lease 和 allocation evidence
- worker 根据分配结果覆盖 `CUDA_VISIBLE_DEVICES`
- worker process `session_id` 防止旧进程世代继续 heartbeat 或 claim
- 默认 30 秒 worker freshness；超时节点在查询中显示为 `offline`，且不能 claim
- expiring lease、随机 token hash、单调 fence、租约续期和过期回收
- at-least-once dispatch、有限重试、协作取消、迟到结果拒绝
- completion receipt：相同完成回调可在响应丢失或重启后安全重放
- `GET /v1/runs/{id}/attempts` 返回完整 attempt 历史，但不泄露 lease token
- allocation 同时保存在 Attempt 和 Run 的 `last_allocation`，重试后仍可审计每次放置
- 类型化、无 shell 的 `demo.sleep` 与 `sglang.serving-benchmark` recipe
- `demo.sleep` 由 worker 自身的内部子命令实现，不依赖 Unix `sleep`，可用于 Linux、Windows、macOS smoke test
- JSON snapshot、append-only Run 事件、Prometheus HTTP 指标和本地 artifact 索引

`demo.sleep` 是跨平台验证路径；`sglang.serving-benchmark` 是当前真实 GPU 工作负载集成。ForgeGrid 不接受任意 shell 字符串，worker 只执行代码中注册并验证过的 recipe。

## Quick Start：CPU smoke

需要 Go 1.26 或更新版本。`demo.sleep` worker 本身可在 macOS、Linux 或 Windows 运行；下面的可复制命令使用 Bash/Zsh 语法，PowerShell 需要改写续行、环境变量和 JSON 取值部分。

终端 1，启动控制面：

```bash
cd ai-infra-control-plane
go run ./cmd/controlplane
```

终端 2，启动一个 CPU smoke worker：

```bash
cd ai-infra-control-plane
go run ./cmd/worker \
  -id cpu-node-01 \
  -name cpu-node-01 \
  -adapter demo.sleep \
  -labels pool=local
```

worker 会自动生成本次进程的 `session_id`，并自动补充 `os`、`arch` 标签。没有 `nvidia-smi` 时资源探测状态为 `unavailable`，但 `gpu_count=0` 的任务仍可正常运行。

终端 3，创建 Experiment 和一个 2 秒 Run：

```bash
cd ai-infra-control-plane

EXPERIMENT_JSON="$(go run ./cmd/expctl -compact experiment create \
  -name "ForgeGrid CPU smoke" \
  -idempotency-key forgegrid-cpu-smoke-exp)"
printf '%s\n' "$EXPERIMENT_JSON"

EXPERIMENT_ID="$(printf '%s\n' "$EXPERIMENT_JSON" | sed -E 's/.*"id":"([^"]+)".*/\1/')"

go run ./cmd/expctl run create \
  -experiment "$EXPERIMENT_ID" \
  -adapter demo.sleep \
  -require-label pool=local \
  -max-attempts 2 \
  -idempotency-key forgegrid-cpu-smoke-run \
  -- 2s
```

打开 [http://127.0.0.1:8080](http://127.0.0.1:8080) 可查看 Runs、Nodes、GPU 资源和 allocation。也可使用：

```bash
go run ./cmd/expctl run list -experiment "$EXPERIMENT_ID"
go run ./cmd/expctl worker list
curl http://127.0.0.1:8080/metrics
```

## Quick Start：GPU placement

在装有 NVIDIA 驱动且 `nvidia-smi` 位于 `PATH` 的主机上启动一个 worker：

```bash
go run ./cmd/worker \
  -id gpu-node-01 \
  -name gpu-node-01 \
  -adapter demo.sleep \
  -labels pool=gpu-lab
```

探测成功后，worker 会自动增加 `accelerator=nvidia` 标签。下面的 Run 要求 1 张 GPU，并要求所选 GPU 在最近一次有效报告中至少有 16 GiB 空闲显存：

```bash
MIN_FREE_GPU_BYTES=$((16 * 1024 * 1024 * 1024))

go run ./cmd/expctl run create \
  -experiment "$EXPERIMENT_ID" \
  -adapter demo.sleep \
  -require-label pool=gpu-lab \
  -require-label accelerator=nvidia \
  -gpus 1 \
  -min-free-gpu-memory "$MIN_FREE_GPU_BYTES" \
  -max-attempts 1 \
  -idempotency-key forgegrid-gpu-placement-run \
  -- 5s
```

这个 smoke run 不会计算 GPU，但会走完整的 GPU 资源匹配、原子 reservation、allocation evidence 和 `CUDA_VISIBLE_DEVICES` 注入路径。`-min-free-gpu-memory` 的单位是 bytes，并且门槛作用于**每一张**被选择的 GPU，而不是多卡空闲显存之和。

真实 SGLang benchmark 使用同一套资源参数：

```bash
go run ./cmd/worker \
  -id sglang-node-01 \
  -name sglang-node-01 \
  -adapter sglang.serving-benchmark \
  -labels pool=gpu-lab \
  -artifacts ./artifacts

go run ./cmd/expctl run create \
  -experiment "$EXPERIMENT_ID" \
  -adapter sglang.serving-benchmark \
  -require-label pool=gpu-lab \
  -require-label accelerator=nvidia \
  -gpus 1 \
  -min-free-gpu-memory "$MIN_FREE_GPU_BYTES" \
  -max-attempts 1 \
  -- \
  --base-url http://127.0.0.1:30000 \
  --dataset-name random \
  --num-prompts 100 \
  --random-input-len 1024 \
  --random-output-len 256 \
  --max-concurrency 16 \
  --disable-tqdm
```

worker 控制 `--output-file`，解析 SGLang JSONL 中的数值指标，并记录原始结果、stdout、stderr 的 URI、字节数和 SHA-256。

## 可选的 macOS development bundle

仓库也能构建一个本地 macOS 包，内含 controlplane、Web 控制台和一个 `demo.sleep` worker：

```bash
make macos-app
open "dist/Experiment Control Plane.app"
```

当前 bundle 文件名仍保留旧的 `Experiment Control Plane.app`，但运行的是同一套 ForgeGrid V0 控制面。它绑定系统分配的 loopback 端口，以内存中的随机 token 保护本地 API，并把状态写入：

```text
~/Library/Application Support/Experiment Control Plane/
```

这是开发包；公开发布仍需 Developer ID 签名与 notarization。可运行 `make test-macos-app` 验证 bundle。SGLang、Windows 和 Kubernetes worker 不会被该应用自动启动。

## 调度语义

worker 发起 claim 时，ForgeGrid 执行以下判断：

1. worker 必须存在、为 `online`、`session_id` 匹配且心跳未过期；
2. worker 当前不能已有 active attempt；
3. Run 必须处于 `queued`、未耗尽重试次数，adapter 和静态标签匹配；
4. GPU Run 还要求资源报告为 `ok`、未过期，并有足够数量的 GPU 分别满足空闲显存门槛；
5. 在该 worker 内，将合格 GPU 按空闲显存升序、UUID 次序排序，选前 `gpu_count` 张；
6. 在多个合格 Run 中按 priority 降序、创建时间升序、ID 升序选择；
7. 一次持久化 mutation 原子创建 attempt、递增 fence、保存 token hash、占用 worker 并记录 allocation。

这里的 best-fit 是**单个 claimant worker 内的 GPU 选择**。V0 是 worker pull 模式，不会等待并比较所有节点后再选择全局最优 worker。

## Session、Lease、Fence 与 Evidence

- `session_id` 标识 worker 进程世代。官方 worker 未指定时自动生成 UUID。旧 session 不能 heartbeat 或 claim；worker 尚有 active allocation 时，新 session 也不能抢占同一 worker ID。
- `lease_token` 是一次 attempt 的临时执行凭据。明文只在 claim 成功时返回一次，JSON snapshot 中只保存 SHA-256 hash。
- `fence` 在同一个 Run 内随 Attempt 单调递增。start、attempt heartbeat、complete 都必须同时携带正确的 token 和 fence。
- lease 过期、token 错误、旧 fence、非 active attempt 或终态 Run 均返回 `409 lease_lost`。
- fencing 阻止旧 worker 写入错误结果，但不能让网络隔离中的旧进程立刻停止耗费 GPU；因此交付语义是 at-least-once，不是 exactly-once。
- allocation 是 claim 时的不可变证据快照。Run 保存最近一次 allocation；Attempt 列表保存每次重试各自的 allocation，并且永远不返回 lease token。

详见 [HTTP API](docs/api.md)、[设计说明](docs/design.md)、[架构图](docs/control-plane-architecture.html) 和 [Run 生命周期](docs/run-lifecycle.html)。

## 安全与远程访问

loopback 开发模式可以不设置鉴权。监听非 loopback 地址时，服务端强制要求一个共享 bearer token：

```bash
export CONTROL_PLANE_API_TOKEN="$(openssl rand -hex 32)"
go run ./cmd/controlplane -listen 0.0.0.0:8080
```

worker 与 `expctl` 自动读取同一环境变量。Web 控制台在首次 `401` 后询问 token，只保存在当前 tab 的 `sessionStorage`，不会放入 URL 或 `localStorage`。

V0 的 token 是单一部署级凭据，不是多租户身份系统。远程部署仍需要 TLS、网络边界和独立的 secret 管理。

## V0 明确边界

当前版本适合个人实验室、小团队内网和单控制面验证，不应被描述为生产级集群调度器：

- JSON snapshot 后端只支持单个 controlplane 进程；没有数据库锁、多副本或 HA
- 一 node = 一 worker = 最多一个 active allocation；没有同节点并发任务或跨 worker 的物理 GPU 去重
- 没有 workload checkpoint、断点续跑、跨节点迁移或抢占恢复
- 没有 GPU P2P/NVLink/NUMA 拓扑感知，也没有 gang scheduling
- 没有 MIG、GPU time-slicing、配额或公平共享
- 没有多租户、租户级 RBAC、审计隔离或计费
- artifact 使用本地文件 URI；没有对象存储复制和远端生命周期管理
- 没有任意容器/任意 shell 执行；内建执行器目前只有 `demo.sleep` 和 `sglang.serving-benchmark`

仓库中的 `kube-adapter` 只负责确定性渲染 one-shot Worker Job；它不是 Kubernetes operator，也不会把 V0 变成多副本调度器。

这些限制是 V0 的刻意取舍：先把资源真实性、租约正确性和执行证据做成闭环，再扩展存储与调度规模。

## Repository layout

```text
cmd/controlplane     HTTP API、内嵌 UI、lease reconciler
cmd/worker           出站轮询 worker 与类型化执行器
cmd/expctl           CLI 客户端
cmd/kube-adapter     确定性的 one-shot Kubernetes Job 渲染器
internal/domain      状态机与资源/执行模型
internal/store       JSON snapshot、队列、reservation、lease、fence、retry
internal/api         严格 JSON HTTP transport 与 metrics
internal/worker      NVIDIA probe、recipe、执行与 artifact 收集
web                  ForgeGrid operator console
docs                 API、设计、架构与生命周期
deploy/k8s           Kubernetes adapter 示例
```

## Validation

```bash
go test -race ./...
go vet ./...
staticcheck ./...
git diff --check
```

测试覆盖并发 claim 单赢家、单 worker reservation、GPU 门槛与 best-fit、stale worker、session fencing、lease expiry/retry、迟到完成拒绝、completion replay、取消竞争、JSON 重启恢复，以及 snapshot 不含明文 lease token。
