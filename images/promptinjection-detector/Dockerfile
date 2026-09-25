FROM python:3.12-slim
WORKDIR /app
COPY requirements.lock .
RUN pip install --no-cache-dir --require-hashes -r requirements.lock
COPY app.py .
COPY LICENSE LICENSE-DeBERTa-MIT /licenses/
ARG MODEL_ID=testsavantai/prompt-injection-defender-base-v2-onnx
ARG MODEL_REVISION=a2109a5d583963f4962a9796d35315bdfed7c294
ENV MODEL_ID=${MODEL_ID}
ENV MODEL_REVISION=${MODEL_REVISION}
# Pin the HuggingFace cache to a fixed path so the weights baked below (as root)
# are found by the non-root runtime user — otherwise from_pretrained would look in
# the runtime user's ~/.cache, miss them, and try to DOWNLOAD at runtime, which the
# zero-egress NetworkPolicy blocks (detector would never become Ready).
ENV HF_HOME=/opt/hf
# Bake weights so the runtime container needs NO network (no-egress).
RUN python -c "from huggingface_hub import snapshot_download; import os; snapshot_download(repo_id=os.environ['MODEL_ID'], revision=os.environ['MODEL_REVISION'], local_dir='/opt/hf/model', allow_patterns=['model.onnx', 'config.json', 'tokenizer.json', 'tokenizer_config.json', 'special_tokens_map.json', 'added_tokens.json', 'spm.model', 'README.md'])"
RUN test -s /opt/hf/model/model.onnx && test -s /opt/hf/model/config.json && test -s /opt/hf/model/tokenizer_config.json
# Force fully-offline model loading at RUNTIME. Baking the weights above is NOT
# sufficient: transformers / huggingface_hub still issue a network HEAD to
# huggingface.co to check for a newer revision on from_pretrained. Under the
# detector's zero-egress NetworkPolicy that HEAD hangs and retries (~80s of DNS
# failures), blowing past the startup probe so the pod never becomes Ready
# (CrashLoopBackOff). These must come AFTER the bake step so the build-time
# download still has network. With them, cold start drops from ~80s to ~6s.
ENV HF_HUB_OFFLINE=1 \
    TRANSFORMERS_OFFLINE=1
RUN useradd -u 10001 -m app && chown -R app /app /opt/hf
USER 10001
EXPOSE 8919
# Listen on the port the cosidecar assigns via MCP_PORT (the operator allocates
# a per-pod port and points the startup probe + runner endpoint + NetworkPolicy
# ingress at it). Hardcoding 8919 made the probe (on the allocated port) hit a
# closed port → connection refused → restart loop. Fall back to 8919 for a bare
# `docker run` with no MCP_PORT. Shell form + exec so uvicorn stays PID 1 and
# receives SIGTERM.
CMD ["sh", "-c", "exec uvicorn app:app --host 0.0.0.0 --port ${MCP_PORT:-8919}"]
