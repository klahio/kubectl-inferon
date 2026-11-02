# kubectl-inferon

A kubectl plugin that diagnoses machine learning (ML) model inference services on Kubernetes: why a model is not serving, whether it answers a real request, and what its containers printed.

An inference service turns into many Kubernetes objects: a serving runtime, Deployments, pods that download the model before the server starts, Services, an Ingress or Gateway API HTTPRoutes, and an autoscaler. When a model is not serving, the reason is usually several objects away, and the service's own status often points somewhere else: when a pod runs out of memory, it may report only that the ingress was not created.

General pod diagnosis tools stop at the pod. `kubectl inferon` knows the path a model takes from storage to an answered request: the model download, the serving runtime that must support the model format, the model server's crash log (for vLLM it finds the actual cause, such as GPU out of memory, behind "Engine core initialization failed"), the Ingress or Gateway API route that must really reach the model, and the inference protocol the server speaks. It answers three questions with one command each:

| Question | Command |
|---|---|
| Is my model serving, and if not, why? | `kubectl inferon status TYPE/NAME` |
| Does it actually answer a request? | `kubectl inferon test TYPE/NAME -f input.json`, or `--prompt "..."` for an LLM |
| What did its containers print? | `kubectl inferon logs TYPE/NAME` |

**Currently supported**: [KServe](https://kserve.github.io/website/) **InferenceService** and **LLMInferenceService** (KServe's resource for large language models (LLMs) served with vLLM), in KServe's Standard deployment mode (not Knative serverless). Commands always take the resource as `TYPE/NAME`, the way kubectl does: `isvc/NAME` is an InferenceService, `llmisvc/NAME` is an LLMInferenceService. See [Compatibility](#compatibility) for versions.

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

### status: summary and diagnosis

```console
$ kubectl inferon status isvc/sklearn-canary -n kserve-test
sklearn-canary   namespace: kserve-test   mode: Standard   Ready   age: 14m

Endpoints
  external     https://sklearn-canary-kserve-test.k8s.internal
  in-cluster   http://sklearn-canary-predictor.kserve-test.svc.cluster.local
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
$ kubectl inferon status isvc/bad-storage -n kserve-test
...
Diagnosis
  ✗ Model download failed   predictor pod/bad-storage-predictor-f4cfc7765-wxst8
      storage-initializer exited with code 1 (restarts: 15)
      ... Storage initialization failed: GCS resource 'kfserving-examples' not found.
      last log lines of storage-initializer:
        ...
      hint: check spec.predictor.model.storageUri and the storage credentials
```

What it diagnoses:
- model download failures
- images that cannot be pulled
- crashing servers and out-of-memory kills
- pods that cannot be scheduled (for example "Insufficient nvidia.com/gpu")
- model formats with no serving runtime
- pods rejected by a quota or an admission policy
- canaries that are not ready
- an external address that cannot be reached, for both Ingress and Gateway API, even when KServe reports `IngressReady=True`
- an Ingress left behind after switching KServe to Gateway API

| Flag | Meaning |
|---|---|
| `-o json\|yaml` | Structured summary for scripts |
| `--events` | Also list recent events (the controller's harmless update-conflict retries are filtered out) |
| `-w, --watch` | Redraw every 2 seconds |
| `--exit-code` | Exit 1 when the InferenceService is not Ready, for health checks |

### test: smoke-test inference request

```console
$ kubectl inferon test isvc/sklearn-iris -n kserve-test -f examples/iris-input.json
POST https://sklearn-iris-kserve-test.k8s.internal/v1/models/sklearn-iris:predict
  connected to 192.168.4.30:443
  protocol v1 (input has "instances") · via HTTPRoute sklearn-iris at 192.168.4.30:443
200 OK · 6 ms

{
  "predictions": [1, 1]
}
```

You don't need to know the address. `test` uses the first route that answers (a TCP connection plus a `GET /` that gets an HTTP answer other than 502, 503 or 504):
1. `--url`, if given
2. the external address, when `status` considers it routable and a healthy Ingress or HTTPRoute publishes it
3. the external host name connected to the Ingress or Gateway address (like `curl --resolve`), when the name does not resolve on your machine
4. a port-forward to a ready pod behind the stable predictor's (or transformer's) Service. When that Service speaks HTTPS, the server's certificate is not verified, and the route line says so

The inference request itself is sent once, on the chosen route.

You don't need to know the protocol either. `test` uses the first rule that gives an answer: `--protocol`, the InferenceService's `protocolVersion`, the shape of the input (`instances` means v1, `inputs` means the v2 Open Inference Protocol), the runtime's first protocol, and finally v1. If the choice contradicts the input or the runtime, or the server's `/openapi.json` lacks the path, `test` stops before sending the inference request.

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

When more than one container is shown, each line is prefixed with `[component/pod/container]`. `--since`, `--tail` (default 100 lines; `-1` for all) and `--prefix auto|true|false` work as you would expect.

### LLMInferenceService

The same three commands work on `llmisvc/NAME`:

```console
$ kubectl inferon test llmisvc/smollm -n kserve-test --prompt "What is Kubernetes? Answer in one sentence."
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

  Known causes so far: the driver library path above, GPU out of memory, no GPU visible, and a refused Hugging Face download.
- `test llmisvc/NAME` sends OpenAI-compatible requests: `--prompt "text"` (with `--max-tokens`, default 64), or `-f` with a body that has `messages` (chat) or `prompt` (completion). The model name comes from the input, `spec.model.name`, or `/v1/models`. The same OpenAI mode works for InferenceServices on KServe's Hugging Face runtime, which serves the API under `/openai`. The default timeout is 2 minutes, because the first request to a freshly started model server can be slow. Streaming is not supported yet.
- `logs llmisvc/NAME` shows the vLLM container `main`; `-c` picks `main`, `prefill`, `scheduler`, `worker` or `all`.

### Common flags and exit codes

Every command takes the usual kubectl connection flags: `-n`, `--context`, `--kubeconfig`, `--as` and the others. `--no-color` or `NO_COLOR` turns colors off.

| Exit code | Meaning |
|---|---|
| 0 | Success |
| 1 | The command ran but the result is a failure: a non-2xx response, `status --exit-code` on a service that is not Ready, or no readable log |
| 2 | Wrong usage or bad input |
| 3 | Cluster unreachable, InferenceService not found, or permission denied |

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
- `create` on `pods/portforward`, for `test`

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
