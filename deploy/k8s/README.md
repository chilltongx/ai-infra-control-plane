# Kubernetes one-shot pull worker

This slice renders a deterministic `batch/v1` Job for the existing worker. It
uses only the Go standard library and invokes no shell.

## Current boundary

The Job is a **generic pull worker**, not a pre-assigned attempt. An external
scaler or operator chooses when to create it. The Pod registers with a stable
worker ID, makes one bounded claim for any run matching its adapter and worker
labels, executes at most one claim, and exits. An empty queue is a successful
no-op.

The control plane does not yet create these Jobs automatically, and `launch_id`
is an external idempotency/correlation key rather than an attempt ID. End-to-end
attempt-bound Kubernetes dispatch would require a separate pre-claimed lease
protocol plus orphan reconciliation.

## Render and apply

Build a worker image containing `/usr/local/bin/worker`, then render a Job:

```bash
go run ./cmd/kube-adapter render \
  --namespace experiments \
  --launch-id launch-sglang-gpu-001 \
  --image ghcr.io/chilltongx/ai-infra-worker:v0.1.0 \
  --adapter sglang.serving-benchmark \
  --control-plane https://controlplane.example.internal \
  --worker-label os=linux \
  --worker-label accelerator=nvidia \
  --node-selector accelerator=nvidia \
  --gpus 1 \
  --active-deadline-seconds 3600 \
  --lease-ttl 45s \
  --claim-wait 20s \
  --api-token-secret control-plane-api \
  --api-token-secret-key token \
  > /tmp/aicp-job.json

kubectl --namespace experiments apply --filename /tmp/aicp-job.json
```

The checked-in [`job.example.json`](job.example.json) is the deterministic
output of that command, with the extra metadata label `workload=sglang`.

`backoffLimit: 0` and `restartPolicy: Never` keep retry ownership in the Go
control plane. `activeDeadlineSeconds` is a hard Kubernetes safety deadline;
the worker recipe timeout remains a separate execution deadline.

## GPU resources

`--gpus N` sets the selected extended resource to the same integer in both
requests and limits. The default resource is `nvidia.com/gpu`; override it with
`--gpu-resource`. A zero count omits GPU resources entirely.

## Authentication

When `--api-token-secret` is configured, the Pod reads
`CONTROL_PLANE_API_TOKEN` through a Kubernetes `secretKeyRef`. The renderer
never accepts the credential value, so it cannot place the token in labels,
annotations, command arguments, or a checked-in manifest. Create the referenced
Secret out of band and use HTTPS whenever traffic leaves a trusted development
network.

## Cancellation and deletion

First request run cancellation through the control-plane API so the worker can
report a cooperative terminal result. To clean up the external Job, delete it
with foreground propagation and wait for its Pod to disappear:

```bash
kubectl --namespace experiments delete job \
  aicp-launch-sglang-gpu-001-44e1740829 \
  --cascade=foreground \
  --wait=true \
  --ignore-not-found=true
```

The adapter exposes the exact argv as JSON without shell quoting:

```bash
go run ./cmd/kube-adapter delete-command \
  --namespace experiments \
  --launch-id launch-sglang-gpu-001
```

Foreground deletion stops the current Pod, but the control plane recognizes
the lost attempt only after its lease expires and then applies its retry policy.
Fencing prevents stale completion; it cannot instantly reclaim GPU compute.
