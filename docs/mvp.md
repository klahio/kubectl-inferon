# kubectl-inferon — minimum viable product (version 0.1)

Status: **draft for review** · Last updated: 2026-10-29 · Supports KServe v0.16 – v0.21 (lab cluster runs v0.21.0) · Resources: InferenceService and LLMInferenceService

## 1. Purpose

`kubectl-inferon` is a kubectl plugin for day-to-day operation of a single KServe model service: an **InferenceService** (short name `isvc`) or an **LLMInferenceService** (`llmisvc`, KServe's resource for large language models; section 6a).

One InferenceService fans out into many Kubernetes objects: a ServingRuntime, Deployments, ReplicaSets, Pods, an init container that downloads the model, a Service, an Ingress or HTTPRoute, and a HorizontalPodAutoscaler. When a model is not serving, finding the reason means checking each of them by hand. When it is serving, calling it means working out the right address, protocol and request body.

Version 0.1 answers three questions with one command each:

| Question | Command |
|---|---|
| Is my model serving, and if not, why? | `kubectl inferon status <name>` or `status llmisvc/<name>` |
| Does it actually answer a request? | `kubectl inferon test <name> -f input.json` or `test llmisvc/<name> --prompt "..."` |
| What did its containers print? | `kubectl inferon logs <name>` or `logs llmisvc/<name>` |

Sections 4 – 6 describe the commands for InferenceServices; section 6a describes what changes for LLMInferenceServices.

### Goal

Acceptance into the official krew plugin index (`kubernetes-sigs/krew-index`).

### Positioning

There is no existing krew plugin for KServe InferenceService (checked 2026-10-29). The closest neighbor is `inferaudit`, a read-only, cluster-wide audit report of model-serving pods.

> inferaudit = cluster-wide audit report; kubectl-inferon = day-to-day operational commands for a single InferenceService or LLMInferenceService.

## 2. Supported environment

| Item | Version 0.1 |
|---|---|
| KServe deployment mode | **Standard mode only** (formerly called RawDeployment) |
| KServe version | **v0.16.0 (released 2025-11-03) through v0.21.0**. See section 2.1 |
| Kubernetes | Whatever the KServe version in use requires (v0.21.0: 1.32 or newer) |
| InferenceService API | `serving.kserve.io/v1beta1` |
| LLMInferenceService API | `serving.kserve.io/v1alpha2` (KServe v0.17+), falling back to `v1alpha1` (v0.16). See section 6a |
| Network exposure | Kubernetes Ingress **or** Gateway API HTTPRoute, both fully supported (section 2.2). Neither is required, because `test` falls back to port-forward |

Knative (Serverless) mode is out of scope for 0.1. If `status` sees the annotation `serving.kserve.io/deploymentMode` with any value other than `Standard` or `RawDeployment`, it prints a clear "not supported in version 0.1" message instead of wrong output.

### 2.1 Version differences the plugin must handle

v0.16 is the oldest supported version. It is the first release where everything `status` relies on is in place:
- the mode is called `Standard`
- the mode is recorded in `status.deploymentMode`
- the selected runtime is recorded in the status
- Gateway API HTTPRoute exposure is available (it arrived in v0.15)

These differences were checked in the KServe source at each release tag:

| Difference | v0.16 – v0.20 | v0.21 |
|---|---|---|
| Name of the mode | `Standard`; the old name `RawDeployment` is still accepted | same |
| Where the mode is recorded | annotation `serving.kserve.io/deploymentMode`, plus `status.deploymentMode` | same |
| Selected runtime in status | `status.clusterServingRuntimeName` / `status.servingRuntimeName` | same |
| Gateway API HTTPRoute exposure | yes | yes |
| Standard-mode canary (`spec.canary[]`) | no | yes |

Rules that follow from the table:
- **Detect fields, not version numbers.** The plugin reads whatever fields exist and never parses the KServe version. Example: it uses `status.deploymentMode`, and falls back to the annotation while a new InferenceService has no status yet.
- **Both mode names.** Treat `Standard` and `RawDeployment` as the same mode, because objects created before an upgrade may still carry the old name.
- **Canary.** Show the canary section only when `spec.canary` exists. Before v0.21 it never does. The older `spec.predictor.canaryTrafficPercent` field belongs to Knative mode, so it is out of scope.
- **Older KServe.** On v0.15 or older, `status.deploymentMode` and the runtime fields are missing. The plugin then still runs, but it prints one warning line: "KServe older than v0.16 is not supported; some fields may be missing".

Standard-mode canary in v0.21 works like this:
- Each item in the `spec.canary[]` field has a `predictor` and a `trafficPercent`.
- Its state is reported in `status.canaryStatuses[]` and in the `CanaryPredictorReady` condition.
- KServe's validation allows `spec.canary` only in Standard mode.
- The traffic is split through HTTPRoutes, so it needs the Gateway API.

### 2.2 Ingress and Gateway API

KServe in Standard mode exposes an InferenceService in one of two ways, depending on its configuration (`kserve.controller.gateway.ingressGateway.enableGatewayApi`):
- **Kubernetes Ingress** (the default): one Ingress per InferenceService.
- **Gateway API**: one or more HTTPRoutes attached to a shared Gateway.

The plugin finds these objects through their owner reference to the InferenceService, so it does not depend on naming conventions (pods, Deployments and autoscalers are found through the `serving.kserve.io/inferenceservice` label). It then reports, in the same terms for both kinds:
- whether the route was accepted by its controller
- whether it points at existing Services
- the address of the Ingress load balancer, or of the Gateway

## 3. Common behavior

### Flags shared by all commands

The standard kubectl connection flags come from `k8s.io/cli-runtime` and behave exactly as they do in kubectl: `-n/--namespace`, `--context`, `--kubeconfig`, `--cluster`, `--user`, `--as`, `--request-timeout`.

Additional flag:

| Flag | Default | Meaning |
|---|---|---|
| `--no-color` | colors on when the output is a terminal | Turn off colored output. The `NO_COLOR` environment variable also turns it off |

### Exit codes

| Code | Meaning |
|---|---|
| 0 | Command succeeded |
| 1 | Command ran, but the result is a failure. Examples: the inference request returned an error, or `status --exit-code` found the InferenceService not Ready |
| 2 | Wrong usage (unknown flag, missing argument, bad input file) |
| 3 | Could not reach the cluster, the InferenceService was not found, or permission was denied |

By default `status` is informational: it exits 0 whatever the InferenceService's state. With `--exit-code`, it exits 1 when the InferenceService is not Ready, so scripts can use it as a health check.

### Permissions

Version 0.1 only needs read access, plus `pods/portforward` for `test`.
- `get` and `list` in the namespace on: `inferenceservices`, `servingruntimes`, `pods`, `deployments`, `horizontalpodautoscalers`, `services`, `ingresses`, `httproutes`, `events`; `get` on `pods/log`
- Cluster-wide: `get` and `list` on `clusterservingruntimes` and `ingressclasses`; `get` on `gateways` in the Gateway's namespace
- For LLMInferenceServices also: `llminferenceservices` and `leaderworkersets` (multi-node only), and API discovery of the `serving.kserve.io` group
- Missing permission for an optional object (for example `gateways`) downgrades that part of the output to "unknown (no permission)" instead of failing the command.
- `get` and `create` on `pods/portforward` (`get` for the WebSocket transport, `create` for the older SPDY one)

The plugin never creates, changes or deletes cluster objects.

## 4. `kubectl inferon status <name>`

A one-screen summary of an InferenceService and, when it is not working, the most likely root cause.

### What it shows

1. **Header**: name, namespace, deployment mode, overall Ready state, and age.
2. **Endpoints**:
   - the external address (`status.url`)
   - the in-cluster address (`status.address.url`)
   - how it is exposed: an Ingress (with its IngressClass), or HTTPRoutes (with their Gateway)
   - whether the external address is actually routable
3. **Model**:
   - model format and storage location (`storageUri`)
   - the selected runtime (`status.clusterServingRuntimeName` or `status.servingRuntimeName`), with the protocols it supports
   - the model state from `status.modelStatus`, including `lastFailureInfo` when present
4. **Components**: one row per component that exists (predictor, transformer, explainer), with:
   - its Ready condition
   - its Deployment's ready replicas compared to the desired count
   - the autoscaler's minimum and maximum
5. **Canary** (only when `spec.canary` is set): for each canary predictor, its traffic percentage and Ready state, plus the traffic left on the stable predictor.
6. **Conditions**: every condition in `status.conditions`, with status, reason and message.
7. **Diagnosis**: shown only when something is wrong (section 4.2).

### 4.2 Diagnosis rules

The plugin follows the chain InferenceService → Deployment → ReplicaSet → Pod → container and reports the first problem it finds. Each rule names the evidence it used.

| Situation | How it is detected | What it prints |
|---|---|---|
| Model download failed | `storage-initializer` init container exited non-zero or is in back-off | Exit code, plus the last log lines of `storage-initializer` |
| Image cannot be pulled | Container waiting reason `ErrImagePull` / `ImagePullBackOff` | The image name and the pull error message |
| Server crashes on start | `kserve-container` restarting (`CrashLoopBackOff`) | Restart count, last termination reason and exit code, and the last log lines |
| Out of memory | Last termination reason `OOMKilled` | The memory limit that was hit |
| Pod cannot be scheduled | Pod `PodScheduled=False` | The scheduler message (for example "Insufficient nvidia.com/gpu") |
| No runtime fits the model | `modelStatus.lastFailureInfo.reason` is `NoSupportingRuntime`, or there is no runtime name | The model format that has no supporting runtime |
| External address not routable (Ingress) | An Ingress exists, but its `ingressClassName` does not match any IngressClass, or it has no load balancer address, **even though `IngressReady=True`** | A warning, and a hint to use `kubectl inferon test` (which falls back to port-forward) |
| External address not routable (Gateway API) | An HTTPRoute's `status.parents[]` condition `Accepted` or `ResolvedRefs` is not True; or its parent Gateway is missing, not `Programmed`, or has no address | Which route and which Gateway, with the condition's reason and message |
| Canary not ready | `CanaryPredictorReady=False` (v0.21 and newer) | The canary predictor's name, and the same diagnosis rules applied to its pods |
| Leftover Ingress | An Ingress owned by the InferenceService whose hosts do not include the external address host. KServe keeps the old Ingress after a switch to Gateway API | A warning with a delete hint. The Ingress does not count against routability, and `test` never uses its address |

Events are shown only when they help. Known noise is hidden. Example: `UpdateFailed` or `InternalError` events whose message contains "the object has been modified" are optimistic-locking retries by the controller, not failures.

### Flags

| Flag | Default | Meaning |
|---|---|---|
| `-o, --output` | human-readable | `json` or `yaml`: a structured summary (not the raw object) for scripts |
| `--events` | off | Also list every recent event for the related objects |
| `-w, --watch` | off | Redraw every 2 seconds until interrupted |
| `--exit-code` | off | Exit 1 when the InferenceService is not Ready (section 3). With `--watch`, the exit code reflects the state when the user stops watching |

### Example output (target design)

```
sklearn-iris   namespace: kserve-test   mode: Standard   READY   age: 57m

Endpoints
  external    http://sklearn-iris-kserve-test.example.com   ⚠ not routable (IngressClass "istio" not found)
  in-cluster  http://sklearn-iris-predictor.kserve-test.svc.cluster.local

Model
  format      sklearn            storage  gs://kfserving-examples/models/sklearn/1.0/model
  runtime     kserve-sklearnserver (ClusterServingRuntime)   protocols: v1, v2
  state       Loaded (UpToDate)

Components
  predictor   Ready   replicas 1/1   autoscaling 1–1 (cpu 80%)

Conditions
  Ready            True
  PredictorReady   True   NewReplicaSetAvailable
  IngressReady     True
  Stopped          False

Hint: the external address is not reachable; `kubectl inferon test isvc/sklearn-iris` will use port-forward.
```

### Acceptance criteria

- With the healthy `sklearn-iris` sample, it shows Ready, both addresses, runtime `kserve-sklearnserver` and protocols v1 and v2.
- It flags the external address as not routable on the lab cluster, where IngressClass `istio` does not exist.
- With KServe switched to Gateway API on the lab cluster, it shows the HTTPRoute and the `lab-gateway` Gateway with its address.
- `status --exit-code` exits 0 for a Ready InferenceService and 1 for each broken sample.
- Against KServe v0.16 it shows the same sections, except canary. (Not verified yet; see section 7, version matrix.)
- For each deliberately broken sample in section 7, the diagnosis line names the right cause.
- It finishes in under 2 seconds on the lab cluster for a single InferenceService.

## 5. `kubectl inferon test <name> -f <input.json>`

Sends one smoke-test inference request and prints the response. You don't need to know the address or the protocol.

### Choosing the target

1. If a transformer exists, send to the transformer. Otherwise, send to the predictor.
2. If `--url` is given, use that address.
3. Otherwise, try the external address (`status.url`) with a short timeout (2 seconds), but only when an Ingress or HTTPRoute that the diagnosis found healthy publishes it. When those objects cannot be read, nothing vouches for the host name, which with KServe's default domain may belong to an unrelated site.
4. If its host name does not resolve from this machine, but the Ingress load balancer or the Gateway has an address, keep the external URL and only send the TCP connection to that address, as `curl --resolve` does. The Host header, the TLS server name and the certificate check then all use the real host name. This tests the real ingress path without any DNS setup. A leftover Ingress (see 4.2), and an Ingress, HTTPRoute or Gateway that the diagnosis found not routable, is never used.
5. If that fails too, port-forward to the component's Service (port 80) on a free local port, waiting at most 30 seconds for it to start. Close the port-forward when the command ends.

A route counts as working when the TCP connection succeeds and a `GET /` through it gets an HTTP answer other than 502, 503 or 504; a TLS failure or a proxy error moves on to the next route. The inference request itself is sent only once, on the chosen route.

The command always says which route it used ("via port-forward to svc/sklearn-iris-predictor").

### Choosing the protocol

Use the first rule that gives an answer:
1. The `--protocol` flag (`v1` or `v2`)
2. `spec.predictor.model.protocolVersion` on the InferenceService
3. The shape of the input file: a top-level `instances` key means v1, a top-level `inputs` key means v2, and a top-level `messages` or `prompt` key means the OpenAI-compatible API (KServe's Hugging Face runtime serves it under `/openai`; section 6a.3)
4. The first entry in the selected runtime's `protocolVersions`
5. v1

If the server publishes `/openapi.json`, check that the chosen path exists there. If it doesn't, stop with a clear error instead of sending a request that is bound to fail. KServe's Python model servers publish it; others may not, and then this check is skipped.

### Request paths

| Protocol | Method and path |
|---|---|
| v1 | `POST /v1/models/<model>:predict` |
| v2 (Open Inference Protocol) | `POST /v2/models/<model>/infer` |
| OpenAI-compatible, InferenceService (Hugging Face runtime) | `POST /openai/v1/chat/completions` (`messages`) or `/openai/v1/completions` (`prompt`) |

The model name defaults to the InferenceService name, which is how KServe's built-in runtimes register it. It can be overridden with `--model`.

### Flags

| Flag | Default | Meaning |
|---|---|---|
| `-f, --filename` | required | Request body file. `-` means read from standard input |
| `--protocol` | automatic | `v1` or `v2` |
| `--model` | InferenceService name | Model name used in the request path |
| `--url` | automatic | Send to this base address instead of discovering one |
| `--port-forward` | `auto` | `auto`, `always`, or `never` |
| `-H, --header` | none | Extra request header, repeatable (for example an authorization token) |
| `--timeout` | `30s`; `2m` for OpenAI requests | Request timeout. The first request to a freshly started LLM server took 23 seconds on the lab cluster, the next ones milliseconds |
| `-o, --output` | `pretty` | Output format, one of `pretty`, `summary`, `raw` or `json` (see Output) |

### Output

The user chooses the format with `-o`:

| Format | What is printed | Typical use |
|---|---|---|
| `pretty` (default) | A header line with route, protocol, address, status code and duration, then the response body pretty-printed | Interactive use |
| `summary` | One line, for example `HTTP 200 · 31 ms · via port-forward · predictions: [1, 1]`. Long bodies are shortened | Quick smoke tests, many models in a loop |
| `raw` | Only the response body, byte for byte, with no header | Piping into `jq` or saving to a file |
| `json` | A JavaScript Object Notation (JSON) document with route, protocol, address, status code, duration and the parsed body | Scripts and continuous integration |

Exit code 0 for a 2xx response, 1 otherwise, in every format.

### Acceptance criteria

- `kubectl inferon test isvc/sklearn-iris -f examples/iris-input.json` returns `{"predictions":[1,1]}` on the lab cluster through port-forward, with no extra flags.
- A v2 input file (`inputs` key) against the same InferenceService uses `/v2/models/sklearn-iris/infer`.
- A wrong path, or a protocol the runtime doesn't support, gives a clear error before any request is sent.
- With KServe on Gateway API, it reaches the model over HTTPS through the `lab-gateway` address, without port-forward. Verified 2026-10-29: `HTTP 200 · via HTTPRoute sklearn-iris`.
- Each `-o` format prints as described, and `-o raw` output can be piped straight into `jq`.

## 6. `kubectl inferon logs <name>`

Shows logs from the right pods and containers. You don't need to know pod names, labels or container names.

### What it selects

- Pods with the label `serving.kserve.io/inferenceservice=<name>`. Narrow them by component with the label `component=predictor|transformer|explainer`.
- **By default**: the `kserve-container` of every predictor pod, every replica included.
- With `--init`: the `storage-initializer` init container instead. This is the first place to look when the model never loads.

### Flags

| Flag | Default | Meaning |
|---|---|---|
| `-c, --component` | `predictor` | `predictor`, `transformer`, `explainer`, or `all` |
| `--init` | off | Show `storage-initializer` logs instead of the server's |
| `--all-containers` | off | Every container in the selected pods, sidecars included |
| `-f, --follow` | off | Keep streaming. New pods that appear (for example during a rollout) are picked up |
| `--since` | none | Only logs newer than a duration, for example `10m` |
| `--tail` | `100` per container (`-1` for all) | Number of lines from the end |
| `-p, --previous` | off | Logs of the previous, crashed container instance |
| `--prefix` | on when more than one stream | Prefix every line with `[component/pod/container]` |

### Acceptance criteria

- `kubectl inferon logs isvc/sklearn-iris` prints the sklearn server's startup lines without being told any pod name.
- `kubectl inferon logs isvc/sklearn-iris --init` prints the model download.
- `-f` keeps following across a pod restart.

## 6a. LLMInferenceService (`llmisvc/NAME`)

Added to version 0.1 on 2026-10-29 (decision 6). Implemented the same day; verified on the lab cluster as described in 6a.5. The three commands keep their names and flags; `TYPE/NAME` (decision 5) picks the resource: `kubectl inferon status llmisvc/smollm`.

### 6a.1 What KServe creates

Observed on the lab cluster with `examples/llmisvc-smollm.yaml` (KServe v0.21.0, no `spec.router`):

| Object | Name | Notes |
|---|---|---|
| Deployment (single node) or LeaderWorkerSet (multi-node, `spec.worker`) | `<name>-kserve` | Owned by the LLMInferenceService |
| Pods | `<name>-kserve-...` | Init container `storage-initializer`, server container `main` (vLLM, image `ghcr.io/llm-d/llm-d-cuda`) |
| Service | `<name>-kserve-workload-svc` | Port 8000, plain HTTP on the lab cluster |
| Secret | `<name>-kserve-self-signed-certs` | Created even when TLS is off |
| With `spec.router`: HTTPRoute, InferencePool, scheduler Deployment | | Need a Gateway implementation that supports the Gateway API Inference Extension; not set up on the lab cluster yet |

Labels on the workload objects: `app.kubernetes.io/part-of=llminferenceservice`, `app.kubernetes.io/name=<name>`, `kserve.io/component=workload` (other values for the scheduler), and `llm-d.ai/role` = `both`, `prefill` or `decode`. The plugin selects with both `part-of` and `name`, because `app.kubernetes.io/name` alone could match unrelated objects.

Status (v1alpha2): `url` (only with a router), `addresses[].url`, `conditions` (`Ready`, `MainWorkloadReady`, `WorkloadsReady`, `RouterReady`, `PresetsCombined`, and prefill or scheduler conditions when those exist), `workloads.primary|prefill|scheduler|service` (references with `readyReplicas`), `router` (`gateways`, `scheduler.inferencePool`), and `appliedConfigs` (the LLMInferenceServiceConfig presets merged into the spec).

**Version differences** (compared in the CRD schemas): `v1alpha1`, the only version in KServe v0.16, has no `status.workloads` and no `status.appliedConfigs`. There, the plugin finds the workloads through the labels above. The plugin asks the API server which version it serves and prefers `v1alpha2`.

### 6a.2 `status llmisvc/NAME`

Shows, in the same layout as section 4:
1. **Header**: name, namespace, Ready, age, API version
2. **Endpoints**: `status.url`, the first `status.addresses[].url`; with a router, the HTTPRoutes and their Gateways
3. **Model**: `spec.model.uri`, the served model name (`spec.model.name`), LoRA adapters, the `main` container image, the parallelism settings (tensor, data, pipeline), and the presets in `appliedConfigs`
4. **Workloads**: primary (decode), prefill and scheduler Deployments and LeaderWorkerSets, each with ready and desired replicas
5. **Conditions** and **Diagnosis**

Diagnosis: all pod rules of section 4.2 apply to the workload pods (model download, image pull, crash, out of memory, scheduling). In addition:

| Situation | How it is detected | What it prints |
|---|---|---|
| Server crashes, root cause earlier in the log | `main` restarts with a non-zero exit. vLLM's last line is usually only "Engine core initialization failed" | Searches the previous instance's last 2,000 log lines for known causes and error lines, and prints the first match instead of the last line (table below) |
| Server still loading, not crashed | Startup probe failing, no restarts yet | "Starting: model loading can take minutes", with the elapsed time, so it is not mistaken for a failure |
| A preset cannot be applied | `PresetsCombined=False` | The condition message (for example a missing LLMInferenceServiceConfig in `spec.baseRefs`) |
| Multi-node without LeaderWorkerSet | `spec.worker` set, and the `leaderworkersets` API is missing | "LeaderWorkerSet is not installed" |
| Router problems | `RouterReady=False`; HTTPRoute `Accepted` or `ResolvedRefs` not True; Gateway missing, not Programmed or without an address | The failing object and its condition. A Gateway implementation without Inference Extension support shows up as an HTTPRoute whose backend is not resolved. The InferencePool object itself is not read in version 0.1 |

Known vLLM start-up causes. Each one is matched in the log and printed with a hint:

| Log text | Meaning | Hint |
|---|---|---|
| `cannot find -l:libcuda.so.1` | Triton cannot find the NVIDIA driver library; seen on the lab cluster | Set `TRITON_LIBCUDA_PATH` to the node's driver directory (`/usr/lib/x86_64-linux-gnu` on Ubuntu or Debian nodes, `/usr/lib64` on RHEL-family nodes) |
| `CUDA out of memory`, `No available memory for the cache blocks` | GPU memory too small for the model plus KV cache | Lower `--max-model-len`, raise `--gpu-memory-utilization`, or use a larger GPU |
| `No CUDA GPUs are available`, `Failed to infer device type` | No GPU visible in the container | Check the GPU request and, where the cluster needs one, `runtimeClassName` |
| `401`, `gated repo`, `Access to model ... is restricted` | Hugging Face refuses the download | Provide a Hugging Face token Secret |

The table is a starting set; new cases are added as they are seen, each with a fixture.

### 6a.3 `test llmisvc/NAME`

LLMInferenceServices serve vLLM's OpenAI-compatible API, at the root (`/v1/...`), not under `/openai` like KServe's Hugging Face runtime.

- **Input**: `-f` with an OpenAI request body: `messages` means `POST /v1/chat/completions`, `prompt` means `POST /v1/completions`. Or `--prompt "text"`, which builds a chat request with one user message. `--max-tokens` (default 64) caps the answer.
- **Model name**: `"model"` from the input if present, otherwise `spec.model.name`, otherwise the first id from `GET /v1/models`.
- **Route**: `--url`; `status.url` when routable (with a router); the Gateway address with the external host name (as in section 5); otherwise port-forward to `<name>-kserve-workload-svc`, port from the Service. HTTPS when the Service port says so.
- **Streaming**: version 0.1 sends `"stream": false` only.
- **Timeout**: `2m` by default for OpenAI requests (section 5 flags); a timeout explains that a model server's first request can be much slower than later ones.
- **Output**: `pretty` shows the answer text, the finish reason and the token usage; `summary` is one line, for example `HTTP 200 · 412 ms · via port-forward · "Kubernetes is ..." · 41+40 tokens`; `raw` and `json` as in section 5.
- The same OpenAI mode works for InferenceServices whose server publishes `/openai/v1/chat/completions` (KServe's Hugging Face runtime). The `/openapi.json` check of section 5 tells the two path layouts apart.

### 6a.4 `logs llmisvc/NAME`

- Default: container `main` of every primary workload pod.
- `-c`/`--component`: `main` (default), `prefill`, `scheduler`, `worker` (the non-leader pods of a multi-node LeaderWorkerSet), or `all`.
- `--init`, `--all-containers`, `-f`, `--since`, `--tail`, `-p` and `--prefix` work as in section 6.

### 6a.5 Acceptance criteria

- With `examples/llmisvc-smollm.yaml` Ready on the lab cluster:
  - `status llmisvc/smollm` shows Ready, the in-cluster address, model `HuggingFaceTB/SmolLM2-135M-Instruct`, the preset `kserve-config-llm-template` and the primary workload 1/1.
  - `test llmisvc/smollm --prompt "What is Kubernetes?"` returns an answer through port-forward, with no other flags.
  - `logs llmisvc/smollm` shows the vLLM start-up lines.
- With `TRITON_LIBCUDA_PATH` removed from the sample, `status` names `cannot find -l:libcuda.so.1` and the hint, not "Engine core initialization failed".
- Against `v1alpha1` objects (KServe v0.16), `status` still lists the workloads (found through their labels). Not verifiable on the lab cluster; covered by a unit test with an edited fixture.

### 6a.6 Lab setup

Minimal install, enough without a router (2026-10-29): Helm charts `kserve-llmisvc-crd` and `kserve-llmisvc-resources` (with `kserve.createSharedResources=false`, because `kserve-resources` already owns the shared objects), then `kserve-runtime-configs` with `kserve.llmisvcConfigs.enabled=true` for the presets. The resources chart also installs the Gateway API Inference Extension CustomResourceDefinitions. A router (Envoy Gateway with Envoy AI Gateway, or another Gateway implementation with Inference Extension support) is not installed; router diagnosis is tested with fixtures until it is.

## 7. Test plan

Unit tests use the fake dynamic client and the fake Kubernetes client from client-go, fed with real objects captured from the lab cluster.

End-to-end checks run against the lab cluster, namespace `kserve-test`, with these samples.

| Sample | Expected result |
|---|---|
| `examples/sklearn-iris.yaml` | Healthy; all three commands succeed |
| `examples/broken/bad-storage.yaml` | `status` diagnoses a model download failure; `logs --init` shows the error |
| `examples/broken/bad-image.yaml` | `status` diagnoses an image pull failure |
| `examples/broken/oom.yaml` | `status` diagnoses out of memory |
| `examples/broken/unschedulable.yaml` | `status` diagnoses a scheduling failure |
| `examples/broken/no-runtime.yaml` | `status` diagnoses that no serving runtime supports the model format |
| `examples/sklearn-canary.yaml` | `status` shows stable 80%, canary `v2` 20% with its own replicas (needs Gateway API, see below) |
| HTTPRoute pointing at a Gateway that does not exist | `status` diagnoses the Gateway API problem (unit test with an edited fixture) |
| `examples/qwen-llm.yaml` (InferenceService, Hugging Face runtime on a time-sliced graphics processing unit) | Healthy; `test` in OpenAI mode reaches `/openai/v1/chat/completions` |
| `examples/llmisvc-smollm.yaml` (LLMInferenceService, no router) | Healthy; acceptance criteria of section 6a.5 |
| `examples/broken/llmisvc-libcuda.yaml` (the sample without `TRITON_LIBCUDA_PATH`) | `status` names `cannot find -l:libcuda.so.1` with the hint |
| `examples/broken/llmisvc-bad-model.yaml` (a model URI that does not exist) | `status` diagnoses the model download failure |
| LLMInferenceService with a router whose Gateway lacks Inference Extension support | Not tested yet: no router on the lab cluster and no fixture. The HTTPRoute and Gateway rules shared with InferenceServices are unit-tested |

### Exposure setups

Both setups must pass before a release. Fixtures for both are in `internal/status/testdata/v0.21` (Ingress) and `internal/status/testdata/v0.21-gateway-api`.

- **Kubernetes Ingress**: KServe's default.
- **Gateway API**: the lab cluster has used it since 2026-10-29, with the existing Cilium Gateway `gateway/lab-gateway`. That Gateway accepts only `*.k8s.internal` host names, and other namespaces only on its HTTPS listener. The Helm values for `kserve-resources` are:
  ```
  kserve.controller.gateway.ingressGateway.enableGatewayApi=true
  kserve.controller.gateway.ingressGateway.kserveGateway=gateway/lab-gateway
  kserve.controller.gateway.ingressGateway.className=cilium
  kserve.controller.gateway.domain=k8s.internal
  kserve.controller.gateway.urlScheme=https
  ```
  Existing InferenceServices move to HTTPRoutes only on their next reconcile, and their old Ingress stays behind.

**Canary objects.** The canary predictor's Deployment, Service and pods carry the same `component=predictor` label as the stable predictor. Only the `app` label tells them apart: `isvc.<name>-predictor` for the stable one, `isvc.<name>-<canary>-predictor` for a canary. `status` counts their replicas separately, and `test` port-forwards to the stable Service. Sample: `examples/sklearn-canary.yaml`, verified with an HTTPRoute split of 80/20.

### Version matrix

**Current state (2026-10-29): only KServe v0.21.0 is tested.** The unit-test fixtures are `internal/status/testdata/v0.21` (Ingress) and `v0.21-gateway-api`; LLMInferenceService fixtures are in `v0.21-llmisvc` (captured with `hack/capture-llm-fixtures.sh`, including the crashed server's log). Support for v0.16 – v0.20 rests on comparing the KServe source at each release tag (section 2.1), not on tests. The README says so too.

Target, not done yet:
- **Fixtures** from each supported line: v0.16 (oldest), v0.18 (middle) and v0.21 (newest), captured with `hack/capture-fixtures.sh`.
- **End-to-end tests** in continuous integration against a kind cluster with KServe installed, once per version in the matrix. They would run on a schedule, not on every pull request. This machine has no Docker or kind, so the version-matrix work needs that first.

Continuous integration (`.github/workflows/ci.yml`) runs the unit tests on every push and pull request.

## 8. Decisions

Decided 2026-10-29:

1. **KServe versions**: support v0.16 through v0.21 by detecting fields, not version numbers (section 2.1).
2. **Gateway API**: fully supported next to Kubernetes Ingress in `status` and `test` (sections 2.2, 4, 5), and tested on the lab cluster (section 7).
3. **`status` exit code**: informational by default; `--exit-code` turns "not Ready" into exit 1 (sections 3, 4).
4. **`test` output**: the user chooses with `-o pretty|summary|raw|json`; `pretty` is the default (section 5).
5. **Plugin name**: `inferon` (binary `kubectl-inferon`, repository `klahio/kubectl-inferon`). Named `isvc` until 2026-10-29, then `kserve` until 2026-11-02.
   - **Why not `kserve`**: the krew-index maintainer closed the `kserve` submission (kubernetes-sigs/krew-index#6555, 2026-10-31): a plugin named after a project must come from that project. Earlier krew-index decisions say the same (`eks-viewer`, `openclaw`, `flink`, `cloudflare-tunnel`), and they also refuse names that are common in the Kubernetes world (`gateway`, `helm`, `trace`). The written naming guide does not say this; only the closed pull requests do.
   - **Why `inferon`**: a coined word from "inference", so it points at model serving without naming a project or a whole category (`model-serving`, `served-model`). It is not used by a krew plugin or krew-index pull request, and no same-domain project uses it (checked 2026-11-02). Coined names like `datadiff` and `podtrace` were accepted in 2026.
   - **Trade-off**: a coined word does not explain itself, so the krew description has to say "KServe" early.
   - **`TYPE/NAME` arguments**: like kubectl (`kubectl logs deploy/x`), every command takes `TYPE/NAME`. `isvc/NAME`, `inferenceservice/NAME` and the plural and API-group spellings mean an InferenceService. `llmisvc/NAME` (and `llminferenceservice/NAME`) means an LLMInferenceService (decision 6). Other KServe types (`ig`, `tm`, the runtimes) give a usage error saying they are not supported yet, and unknown types give "unknown resource type". Each type has its own handler behind the same commands.
   - **No bare `NAME`** (decided 2026-11-02): a bare `NAME` used to mean an InferenceService. It is now a usage error that suggests `isvc/NAME` or `llmisvc/NAME`, because a default type becomes ambiguous once other serving platforms (for example NVIDIA NIM Operator's NIMService) are supported.
   - **Keeping a rename cheap**: the name is spelled out in one place in the code, `version.Name`. Messages build "kubectl inferon" from `version.Command`.

6. **LLMInferenceService in version 0.1**: `status`, `test` and `logs` support `llmisvc/NAME` (section 6a). This brings the OpenAI-compatible request mode into 0.1, for LLMInferenceServices and for InferenceServices on the Hugging Face runtime. Router-based setups (Gateway API Inference Extension) are diagnosed from `RouterReady` and their HTTPRoutes and Gateways, but not tested until a router is installed on the lab cluster.
7. **No `create` command**: creating or editing resources stays with YAML and `kubectl apply`. The plugin stays read-only.
8. **Release**: done. The early `v0.1.0` tag (before decision 6) was deleted, and `v0.1.0` was released from `3170603` on 2026-10-30 with section 6a. Fixes from the review before the krew-index submission go into `v0.1.1`.

### Still open

- Whether the newest KServe release at release time joins the matrix automatically, or only after it has been checked by hand.

## 9. Out of scope for version 0.1

| Item | Why it waits | Earliest |
|---|---|---|
| Knative (Serverless) mode | Different objects (Knative Services and Revisions) and different status fields; the biggest scope risk | 0.2 |
| `kubectl inferon list` | `kubectl get isvc` is good enough for 0.1 | 0.2 |
| ServingRuntime listing | Runtime information already appears in `status` | 0.2 |
| Streaming responses (`"stream": true`) | Server-sent events need a different output path | 0.2 |
| Disaggregated prefill and decode, and multi-node workloads, on the lab cluster | One GPU, one node; covered by fixtures only | When hardware allows |
| InferenceGraph (`ig/NAME`) | Its API is still `v1alpha1`, and it is rarely used | Later. First as a "used by graph X" line in `status`, then `kubectl inferon status ig/NAME` |
| TrainedModel (`tm/NAME`, multi-model serving) | Rarely used | Later |
| Creating or editing InferenceServices or LLMInferenceServices | YAML plus `kubectl apply` already does this well; the plugin stays read-only (decision 7) | Not planned |
| Saving non-JSON responses (for example generated images) to a file | Needs custom predictors with their own response formats | Later |

## 10. Implementation notes

- **Language and libraries**: Go, `spf13/cobra` for commands, `k8s.io/cli-runtime` for kubectl-compatible flags, and `k8s.io/client-go` for everything else: the dynamic client, the typed core and apps clients, and port-forward.
- **No KServe Go types.** Read InferenceService and ServingRuntime objects through the dynamic client as unstructured data. KServe's Go module pulls in a very large dependency tree and ties the plugin to one KServe version. This also makes it possible to support several KServe versions (section 2.1).
- **Version handling in one place.** A small internal package turns an unstructured InferenceService into one plugin-side model: mode, runtime, components, canaries and exposure. All the "field exists or not" fallbacks live there, so commands never branch on KServe versions themselves.
- **Gateway API objects** (HTTPRoute, Gateway) are read through the dynamic client too. If the Gateway API CustomResourceDefinitions are not installed in the cluster, that part is silently skipped.
- **Go module path**: `github.com/klahio/kubectl-inferon`
- **License**: Apache License 2.0 (`LICENSE`, included in every release archive and extracted by krew)
- **Release**:
  - GoReleaser and GitHub Actions build per-platform archives: Linux, macOS and Windows on amd64 and arm64, with sha256 checksums and fixed version tags.
  - A `.krew.yaml` template feeds the pull request to `krew-index`.
  - `krew-release-bot` handles later version bumps.
- **krew acceptance basics**: real value beyond plain kubectl, no overlap with an existing plugin, the `kubectl-` binary name, a valid plugin manifest, and per-platform archives.
