# kubectl-inferon

A kubectl plugin that diagnoses machine learning (ML) model inference services on Kubernetes: why a model is not serving, whether it answers an inference request, and what its containers printed.

An inference service involves many Kubernetes objects: the serving runtime it selects, Deployments, pods that download the model before the server starts, Services, an Ingress or Gateway API HTTPRoutes, and an autoscaler. The service's status often names a different object from the one that failed. A pod that runs out of memory, for example, may show up only as an ingress that was not created.

`kubectl inferon` checks the objects between the model in storage and a response: the model download, whether a serving runtime supports the model format, the model server's crash log (vLLM often ends with only "Engine core initialization failed", so it searches the earlier lines for the cause), the Ingress or HTTPRoute in front of the model, and the protocol the server speaks.

| Question | Command |
|---|---|
| Which inference services are there, and which are broken? | `kubectl inferon list` |
| Is my model serving, and if not, why? | `kubectl inferon status TYPE/NAME` |
| Does it actually answer a request? | `kubectl inferon test TYPE/NAME -f input.json`, or `--prompt "..."` for an LLM |
| What did its containers print? | `kubectl inferon logs TYPE/NAME` |

Start with `list`, which prints each service as `TYPE/NAME`, the argument the other commands take. The supported types are `isvc` (InferenceService) and `llmisvc` (LLMInferenceService, for large language models (LLMs) served with vLLM); [Compatibility](#compatibility) lists the platform versions.

The plugin is read-only: it never creates, changes or deletes cluster objects.

## Install

With [krew](https://krew.sigs.k8s.io/) (once the plugin is in the krew index):

```bash
kubectl krew install inferon
```

From a release: download the archive for your platform from [Releases](https://github.com/klahio/kubectl-inferon/releases), then put `kubectl-inferon` on your `PATH`.

From source (Go 1.26 or newer):

```bash
go install github.com/klahio/kubectl-inferon@latest
```

## Usage

### list: what is there

```console
$ kubectl inferon list -n models
NAME                  READY     MODEL                                 PROBLEM                   AGE
isvc/bad-storage      False     sklearn                               Model download failed     3d
isvc/sklearn-canary   True      sklearn                               -                         3d
isvc/sklearn-iris     True      sklearn                               -                         3d
isvc/unschedulable    False     sklearn                               Pod cannot be scheduled   3d
llmisvc/smollm        True      HuggingFaceTB/SmolLM2-135M-Instruct   -                         3d
```

The problem column comes from the same pod checks as `status`, run on one pod list for the whole namespace, so a long list stays fast. Routes and logs are left to `status`.

| Flag | Meaning |
|---|---|
| `-A, --all-namespaces` | List in every namespace |
| `-o name\|json\|yaml` | `name` prints one `TYPE/NAME` per line, for scripts |

### status: summary and diagnosis

```console
$ kubectl inferon status isvc/sklearn-canary -n models
sklearn-canary   namespace: models   mode: Standard   Ready   age: 14m

Endpoints
  external     https://sklearn-canary-models.k8s.internal
  in-cluster   http://sklearn-canary-predictor.models.svc.cluster.local
  httproute    sklearn-canary   via Gateway gateway/lab-gateway   address: 192.168.4.30

Model
  format    sklearn
  storage   gs://kfserving-examples/models/sklearn/1.0/model
  runtime   kserve-sklearnserver (ClusterServingRuntime)   protocols: v1, v2
  state     Loaded (UpToDate)

Components
  predictor   Ready   replicas 1/1   autoscaling 1–1 (cpu 80%)

Canary
  stable   80%
  v2       20%   Ready   replicas 1/1

Conditions
  Ready                  True
  CanaryPredictorReady   True   AllCanariesReady
  ...
```

When something is wrong, `status` follows the chain InferenceService → Deployment → pod → container and names the cause:

```console
$ kubectl inferon status isvc/bad-storage -n models
...
Diagnosis
  ✗ Model download failed   predictor pod/bad-storage-predictor-f4cfc7765-wxst8
      storage-initializer exited with code 1 (restarts: 15)
      ... Storage initialization failed: GCS resource 'kfserving-examples' not found.
      last log lines of storage-initializer:
        ...
      hint: check spec.predictor.model.storageUri and the storage credentials
```

Two more sections cover slow starts and GPU capacity:

```console
GPU
  request   primary     nvidia.com/gpu 1 per pod (1 pod)
  node      gpu-node-1  nvidia.com/gpu 4 allocatable, 1 requested, 3 free (1 by this service)

Startup   pod/smollm-kserve-66c88967bb-fwtlh
  scheduling       33s
  pod start        1s      image pull and container start
  model download   6m48s
  server start     1s      image pull and container start
  model load       5m      until ready
  total            12m
```

- **GPU** appears when the pods request a GPU resource (`nvidia.com/gpu`, MIG slices, other vendors' `*gpu*` resources). It shows how much each node offers and how much is taken, across all namespaces. When a pod cannot be scheduled for lack of GPUs, the diagnosis adds how many are free on each node.
- **Startup** splits the start of the newest server pod into phases, taken from the pod's conditions and container states. If a node restart recreated the pod later, the phases describe that run. A failed download or a crashing server ends the list with the failure.

What it diagnoses:
- model download failures
- images that cannot be pulled
- crashing servers and out-of-memory kills
- pods that cannot be scheduled (for example "Insufficient nvidia.com/gpu")
- model formats with no serving runtime
- pods rejected by a quota or an admission policy
- canaries that are not ready
- an external address that cannot be reached, for both Ingress and Gateway API, even when the service reports `IngressReady=True`
- an Ingress left behind after a switch to Gateway API

| Flag | Meaning |
|---|---|
| `-o json\|yaml` | Structured summary for scripts |
| `--events` | Also list recent events (the controller's harmless update-conflict retries are filtered out) |
| `-w, --watch` | Redraw every 2 seconds |
| `--exit-code` | Exit 1 when the InferenceService is not Ready, for health checks |

### test: smoke-test inference request

```console
$ kubectl inferon test isvc/sklearn-iris -n models -f examples/iris-input.json
POST https://sklearn-iris-models.k8s.internal/v1/models/sklearn-iris:predict
  connected to 192.168.4.30:443
  protocol v1 (input has "instances") · via HTTPRoute sklearn-iris at 192.168.4.30:443
200 OK · 6 ms

{
  "predictions": [1, 1]
}
```

`test` finds the address itself. It uses the first route that answers, meaning a TCP connection succeeds and `GET /` returns an HTTP status other than 502, 503 or 504:
1. `--url`, if given
2. the external address, when `status` considers it routable and a healthy Ingress or HTTPRoute publishes it
3. the external host name connected to the Ingress or Gateway address (like `curl --resolve`), when the name does not resolve on your machine
4. a port-forward to a ready pod behind the stable predictor's (or transformer's) Service. When that Service speaks HTTPS, the server's certificate is not verified, and the route line says so

The inference request itself is sent once, on the chosen route.

It picks the protocol in this order: `--protocol`, the InferenceService's `protocolVersion`, the shape of the input (`instances` means v1, `inputs` means the v2 Open Inference Protocol), the runtime's first protocol, and finally v1. If the choice contradicts the input or the runtime, or the server's `/openapi.json` lacks the path, `test` stops before sending the inference request.

| Flag | Meaning |
|---|---|
| `-f, --filename` | Request body; `-` reads standard input |
| `-o pretty\|summary\|raw\|json` | `summary` prints one line, `HTTP 200 · 1 ms · via port-forward · predictions: [1, 1]`; `raw` prints only the body, for `jq` |
| `--protocol v1\|v2\|openai`, `--model`, `--url` | Override the automatic choices |
| `--port-forward auto\|always\|never` | Route control |
| `-H "Name: value"` | Extra header, repeatable |

### logs: the right containers, without pod names

```bash
kubectl inferon logs isvc/sklearn-iris           # model server of every predictor pod
kubectl inferon logs isvc/sklearn-iris --init    # model download (storage-initializer)
kubectl inferon logs isvc/sklearn-iris -c all -f # every component; follows new and restarted pods
kubectl inferon logs isvc/sklearn-iris -p        # the previous, crashed instance
```

When more than one container is shown, each line is prefixed with `[component/pod/container]`. `--since` and `--tail` (default 100 lines; `-1` for all) work as in `kubectl logs`. `--prefix auto` adds the prefix only when more than one stream is shown; `true` and `false` force it on or off.

### LLMInferenceService

`status`, `test` and `logs` also accept `llmisvc/NAME`:

```console
$ kubectl inferon test llmisvc/smollm -n models --prompt "What is Kubernetes? Answer in one sentence."
POST http://127.0.0.1:44271/v1/chat/completions
  protocol openai (LLMInferenceService) · via port-forward to svc/smollm-kserve-workload-svc
200 OK · 73 ms

Kubernetes is a decentralized cloud-based platform for managing and controlling very large, complex applications, often over the Internet.

model HuggingFaceTB/SmolLM2-135M-Instruct · finish stop · tokens 41 prompt + 26 completion
```

- `status llmisvc/NAME` shows the addresses, the model, the vLLM image, the presets the controller merged in, and the workloads (primary, prefill, scheduler) with their replicas. When the vLLM server crashes, its last log line is usually just "Engine core initialization failed". So `status` searches the last 2,000 log lines for known causes, and otherwise shows the first real error lines:

  ```console
  ✗ Server crashes on start   primary pod/llm-libcuda-kserve-664c9f846b-ktmtd
      container main exited with code 1 (Error), restarts: 1
      cause: Triton cannot find the NVIDIA driver library libcuda.so.1
      from the log of main:
        /usr/bin/ld: cannot find -l:libcuda.so.1
      hint: set TRITON_LIBCUDA_PATH to the node's driver directory: /usr/lib/x86_64-linux-gnu on Ubuntu or Debian nodes, /usr/lib64 on RHEL-family nodes
  ```

  Known causes so far: the driver library path above, GPU out of memory, a KV cache too small for `--max-model-len`, GPU memory already taken by other processes (common on time-sliced GPUs), a tensor or pipeline parallel size larger than the pod's GPUs, no visible GPU, a `/dev/shm` that is too small, an unsupported model architecture, a model that needs `--trust-remote-code`, and a refused Hugging Face download.
- `test llmisvc/NAME` sends OpenAI-compatible requests: `--prompt "text"` (with `--max-tokens`, default 64), or `-f` with a body that has `messages` (chat) or `prompt` (completion). The model name comes from the input, `spec.model.name`, or `/v1/models`. The same OpenAI mode works for InferenceServices on the Hugging Face runtime, which serves the API under `/openai`. The default timeout is 2 minutes, because the first request to a freshly started model server can be slow.
- `--stream` (or `"stream": true` in the input) streams the response and reports the time to the first token and the tokens per second after it. The token count comes from the usage the server reports, or from the number of chunks when it reports none.

  ```console
  $ kubectl inferon test llmisvc/smollm -n models --prompt "Hi" --stream -o summary
  HTTP 200 · 28 ms · via port-forward · "Hello! How can I help you today?" · 31+10 tokens · first token 14 ms · 650.7 tokens/s after it
  ```
- `logs llmisvc/NAME` shows the vLLM container `main`; `-c` picks `main`, `prefill`, `scheduler`, `worker` or `all`.

### Common flags and exit codes

Every command takes the usual kubectl connection flags: `-n`, `--context`, `--kubeconfig`, `--as` and the others. `--no-color` or `NO_COLOR` turns colors off.

| Exit code | Meaning |
|---|---|
| 0 | Success |
| 1 | The command ran but the result is a failure: a non-2xx response, `status --exit-code` on a service that is not Ready, or no readable log |
| 2 | Wrong usage or bad input |
| 3 | Cluster unreachable, resource not found, or permission denied |

## Compatibility

| | Supported |
|---|---|
| KServe | v0.16 – v0.21. Tested on v0.21.0; v0.16 – v0.20 are covered by comparing the KServe source of each release, not yet by tests |
| LLMInferenceService | `serving.kserve.io/v1alpha2` (KServe v0.17+) or `v1alpha1` (v0.16), whichever the cluster serves. Tested without a router. Router setups (Gateway API Inference Extension) are diagnosed from `RouterReady` and their HTTPRoutes and Gateways, but not tested yet |
| Deployment mode | Standard (formerly RawDeployment). Knative (Serverless) mode is not supported yet |
| Exposure | Kubernetes Ingress and Gateway API HTTPRoute |
| Canary rollouts | `spec.canary` (KServe v0.21, Standard mode) |

The plugin reads KServe objects as unstructured data and checks for fields rather than version numbers, so it does not depend on KServe's Go module.

## Permissions

`kubectl inferon` never creates, changes or deletes cluster objects. It needs:
- `get` and `list` in the namespace on `inferenceservices`, `servingruntimes`, `pods`, `deployments`, `horizontalpodautoscalers`, `services`, `ingresses`, `httproutes` and `events`, and `get` on `pods/log`
- cluster-wide `get` and `list` on `clusterservingruntimes` and `ingressclasses`, and `get` on the `gateways` an HTTPRoute attaches to (often in another namespace)
- for LLMInferenceServices also `llminferenceservices` and, for multi-node services, `leaderworkersets`
- `get` and `create` on `pods/portforward`, for `test` (newer API servers open port-forwards over WebSockets, which needs `get`)
- for `list -A`, `list` on the service types and on `pods` in every namespace
- optional, for the GPU section: cluster-wide `list` on `nodes` and `pods`

A missing permission on an optional object shows up as "not checked" instead of failing the command.

## Development

```bash
go test ./...                              # unit tests, using objects captured from a real cluster
go test ./internal/status -update          # refresh the golden output files after an intended layout change
hack/capture-fixtures.sh NAMESPACE DIR     # capture InferenceService fixtures from a cluster
hack/capture-llm-fixtures.sh NAMESPACE DIR # capture LLMInferenceService fixtures, with crashed server logs
go run github.com/goreleaser/goreleaser/v2@v2.18.3 release --snapshot --clean   # local release build
```

`examples/` holds the sample InferenceServices used for testing, including deliberately broken ones in `examples/broken/`. The specification and the design decisions are in [docs/mvp.md](docs/mvp.md).

## License

[Apache License 2.0](LICENSE)
