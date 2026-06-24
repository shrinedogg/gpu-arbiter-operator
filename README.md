# gpu-arbiter-operator

A Kubernetes operator that arbitrates access to a shared GPU between a **vLLM**
inference Deployment and transient **gaming-session** pods. It is the Go /
controller-runtime port of the original bash `gpu-arbiter` Deployment.

## What it does

`GPUArbiter` is a cluster-scoped CRD (`gpu.biggs.dog/v1alpha1`) that drives a
single reconciler. Each reconcile pass performs two jobs, mirroring the old
bash loop:

1. **vLLM scaler** — lists gaming pods (e.g. `app=direwolf-worker` in
   `dreamcast`). While any active gaming pod exists, it scales the `vllm`
   Deployment (in `ai-system`) to `0`; when idle, back to `1`. This releases
   the GPU VRAM vLLM was holding.
2. **Scheduling-gate remover** — for each gaming pod carrying the
   `gpu.biggs.dog/await-vram` scheduling gate, it removes the gate once VRAM is
   free, evaluated in this priority order:
   - **Primary** — the vLLM Deployment reports `status.replicas == 0` (a fresh
     ~1–2s signal that VRAM has actually been released).
   - **Secondary** — `DCGM_FI_DEV_FB_FREE` (via VictoriaMetrics) is `>= freeMiB`
     (the DCGM scrape lags ~30s, so this is a backup).
   - **Fallback** — a bounded `timeoutSeconds` elapses, so a stuck metric
     degrades to the old software-encode path instead of 401-ing the session.

The gate itself is injected onto every gaming pod at CREATE time by a native
[`MutatingAdmissionPolicy`](config/default/schedulinggate_policy.yaml)
(carried over from the original), not by this operator.

## From bash env vars to a CRD

The original Deployment was configured via env vars. They now live on the
`GPUArbiter` spec, which makes the config declarative and tunable without a
redeploy:

| env var        | spec field                   | default                                            |
| -------------- | ---------------------------- | -------------------------------------------------- |
| `VLLM_NS`      | `spec.vllm.namespace`        | —                                                  |
| `VLLM_DEPLOY`  | `spec.vllm.name`             | —                                                  |
| `GAME_NS`      | `spec.game.namespace`        | —                                                  |
| `GAME_SELECTOR`| `spec.game.labelSelector`    | —                                                  |
| `GATE`         | `spec.gateName`              | `gpu.biggs.dog/await-vram`                         |
| `VM_URL`       | `spec.metrics.url`           | `http://vmsingle-stack.observability.svc:8428`     |
| `VM_QUERY`     | `spec.metrics.query`         | `max(DCGM_FI_DEV_FB_FREE)`                          |
| `VM_TIMEOUT`   | `spec.metrics.timeoutSeconds`| `4`                                                |
| `FREE_MIB`     | `spec.freeMiB`               | `8000`                                             |
| `GATE_TIMEOUT` | `spec.timeoutSeconds`        | `45`                                               |
| `INTERVAL`     | `spec.intervalSeconds`       | `2`                                                |

The full spec is `vllm{namespace,name}`, `game{namespace,labelSelector}`,
`gateName`, `metrics{url,query,timeoutSeconds}`, `freeMiB`, `timeoutSeconds`,
and `intervalSeconds`. A ready-to-apply instance encoding the original values
is in
[`config/samples/gpu_v1alpha1_gpuarbiter.yaml`](config/samples/gpu_v1alpha1_gpuarbiter.yaml).

The CR also reports **status** (`observedGeneration`, `gamePods`,
`currentVLLMReplicas`, `desiredVLLMReplicas`, `freeVRAMMiB`, `gatedPods[]`,
`lastReconcile`, `message`) that replaces the old bash log lines —
`kubectl describe gpuarbiter cluster0` now shows the controller's last
decision.

## Design notes

- **Poll-driven, not event-driven.** The reconciler always requeues after
  `spec.intervalSeconds`. Watching pods/deployments would force cluster-wide RBAC
  (broader than the original's tight namespace-scoped Roles) since the watched
  namespaces are per-CR. Polling at 2s matches the original bash loop exactly and
  keeps permissions minimal. The cost when idle is a few targeted GETs/LISTs.
- **vLLM-down is the primary ungate signal.** It's ~1–2s fresh, whereas the
  DCGM scrape lags ~30s. The metric is still consulted as a secondary.
- **Terminal pods don't count.** A `Succeeded`/`Failed` gaming pod has released
  the GPU, so it no longer keeps vLLM pinned to 0. (The original counted raw
  pods; this is a deliberate improvement to avoid vLLM staying down after a
  crash-looping session ends.)
- **Cluster-scoped CR.** Conceptually there is one arbitration policy for the
  shared GPU, so the CRD is cluster-scoped (no namespace).
- **RBAC** is generated from markers via `controller-gen` into `config/rbac`.
  The generated `manager-role` is a ClusterRole bound by a ClusterRoleBinding.
  This is broader than the original's per-namespace Roles + cross-namespace
  RoleBindings; see `config/rbac` if you want to tighten it back to namespaced
  permissions for `pods`/`deployments`.

## Notes / changelog

### v0.1.0

- **Scaling uses the Deployment scale subresource** (`deployments/scale`) via
  `SubResource("scale").Update(...)`. The earlier implementation built a
  `client.MergeFrom` patch backwards, which emitted
  `{"spec":{"replicas":null}}` — deleting the field and letting it default
  back to `1` instead of scaling to the target. The scale subresource sets
  `spec.replicas` to the intended value and matches the operator's RBAC.
- **Status is written with a full `Status().Update()`** instead of a JSON
  merge patch. RFC 7386 merge patches drop `omitempty` zero values, so when
  `gamePods` fell from `1` to `0` the field was omitted and the stale `1`
  persisted. A full Update writes every field authoritatively, so zero-valued
  fields clear correctly.

## Build & develop

Requires Go 1.26+ (tool binaries are installed locally by the Makefile).

```bash
make manifests     # regenerate CRD + RBAC manifests (controller-gen)
make generate      # regenerate deepcopy methods
make build         # fmt + vet + build the manager binary to bin/manager
make test          # run unit tests
make run           # run against the cluster in your kubeconfig
```

## Deploy

```bash
# Build & push the image (override IMG as needed)
make docker-build docker-push IMG=shrinedogg/gpu-arbiter-operator:v0.1.0

# Apply CRD, RBAC, manager Deployment, the MutatingAdmissionPolicy, and the
# sample GPUArbiter instance.
make deploy IMG=shrinedogg/gpu-arbiter-operator:v0.1.0
```

The manager runs under a restricted PodSecurity profile (non-root, distroless,
dropped capabilities).

The published image is **`docker.io/shrinedogg/gpu-arbiter-operator:v0.1.0`**
(`linux/amd64`, matching the cluster nodes), built and pushed with:

```bash
docker buildx build --platform linux/amd64 \
  -t shrinedogg/gpu-arbiter-operator:v0.1.0 --push .
```

### Flux GitOps (biggs.dog)

In the homelab this operator is deployed declaratively via Flux from the
`biggs.dog` repo, not with `make deploy`:

- `clusters/cluster0/kubernetes/apps/dreamcast/gpu-arbiter-operator` — the
  operator (CRD, RBAC, manager Deployment), running in the `dreamcast`
  namespace with cross-namespace RBAC to scale `ai-system/vllm`.
- `clusters/cluster0/kubernetes/apps/dreamcast/gpu-arbiter-instance` — the
  `GPUArbiter` custom resource encoding the live configuration.

### Verifying

```bash
kubectl get gpuarbiter cluster0 -o yaml
# watch the controller ungate a pod after launching a game:
kubectl get pods -n dreamcast -l app=direwolf-worker -w
```

## Layout

```
api/v1alpha1/                 GPUArbiter CRD types (+ generated deepcopy)
internal/controller/          reconciler, gate decision tree, VM metrics client
cmd/manager/                  manager entrypoint
config/                       kustomize manifests (crd, rbac, manager, default, samples)
```
