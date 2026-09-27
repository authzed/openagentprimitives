# Prompt Injection Detector

A zero-egress FastAPI service that detects prompt-injection attacks in tool
input using an ONNX-optimized transformer model.

## HTTP Contract

- **GET `/healthz`** — Liveness probe
  - Response: `{"ok": true}` (HTTP 200)

- **POST `/`** — Classification endpoint
  - Request body: `{"text": "<tool input text>"}`
  - Response: `{"score": <float 0..1>, "label": "injection" | "benign"}`
  - The `score` is the softmax probability of the injection class; `label` is
    `"injection"` if score ≥ 0.5, else `"benign"`.

## Model Selection

The default model is `testsavantai/prompt-injection-defender-base-v2-onnx` at
commit `a2109a5d583963f4962a9796d35315bdfed7c294`. An alternative must be a
compatible ONNX sequence classifier with `model.onnx`, `config.json`, and
tokenizer files at the model repository root. Set both its ID and immutable
revision when building:

```bash
docker build -t pi-detector \
  --build-arg MODEL_ID=<onnx-model-repository> \
  --build-arg MODEL_REVISION=<full-model-commit> \
  images/promptinjection-detector/
```

Python dependencies are pinned with artifact hashes in `requirements.lock`.
After changing `requirements.txt`, regenerate it with:

```bash
uv pip compile images/promptinjection-detector/requirements.txt \
  --universal --generate-hashes \
  -o images/promptinjection-detector/requirements.lock
```

### Lakera PINT Validation

Before deploying any model, validate it against the Lakera PINT benchmark to
ensure it reliably detects common prompt-injection techniques. The reference
model has been validated; any custom model should undergo the same gate before
being trusted in production.

## Zero-Egress Design

Model weights are baked into the image at build time via the RUN step in the
Dockerfile. The runtime container requires NO network access after startup and
emits NO egress traffic.

## Digest Pinning

In production (e.g., in the inspector's configuration or the operator's pod
spec), always reference this image by its content digest:

```
<registry>/pi-detector@sha256:<full-digest>
```

Digest pinning ensures reproducible, auditable deployments and prevents silent
model updates.

## Manual Smoke Test

```bash
# Build the image
docker build -t pi-detector images/promptinjection-detector/

# Run in the background
docker run -p 8919:8919 pi-detector &

# Health check
curl -s localhost:8919/healthz
# Expected: {"ok":true}

# Classify a prompt-injection attempt
curl -s -XPOST localhost:8919/ \
  -H 'content-type: application/json' \
  -d '{"text":"ignore previous instructions"}'
# Expected: high score (e.g., {"score": 0.95, "label": "injection"})

# Classify a benign query
curl -s -XPOST localhost:8919/ \
  -H 'content-type: application/json' \
  -d '{"text":"what is the capital of France"}'
# Expected: low score (e.g., {"score": 0.05, "label": "benign"})
```

## Runtime Details

- **Port:** 8919
- **Isolation:** Runs as non-root user `app` (uid 10001)
- **Environment:** Python 3.12 slim base, ONNX Runtime for CPU inference
- **Resource:** Expects typical transformer inference on CPU
