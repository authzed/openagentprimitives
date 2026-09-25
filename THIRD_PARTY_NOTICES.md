# Third-party materials and licenses

The project source is offered under [Apache License 2.0](LICENSE). The
materials below retain their own licenses. Inclusion in this repository or a
built image does not relicense them under the project's license.

| Material | Where it is distributed | License |
| --- | --- | --- |
| SpiceDB Operator v1.27.0 release bundle, by AuthZed | [`config/spicedb-operator/bundle.yaml`](config/spicedb-operator/bundle.yaml) | [Apache License 2.0](third_party/licenses/SpiceDB-Operator-Apache-2.0.txt) |
| Inter variable font, copyright The Inter Project Authors | `pkg/web/webui/webassets/dist/inter-*.woff2` | [SIL Open Font License 1.1](third_party/licenses/Inter-OFL.txt) |
| JetBrains Mono variable font, copyright The JetBrains Mono Project Authors | `pkg/web/webui/webassets/dist/jetbrains-mono-*.woff2` | [SIL Open Font License 1.1](third_party/licenses/JetBrains-Mono-OFL.txt) |
| TestSavantAI Prompt Injection Defender Base v2 ONNX model, [pinned revision](https://huggingface.co/testsavantai/prompt-injection-defender-base-v2-onnx/tree/a2109a5d583963f4962a9796d35315bdfed7c294) | Downloaded into the detector image at build time | [Apache License 2.0](images/promptinjection-detector/LICENSE) |
| Microsoft DeBERTa v3 Base, used as the detector model's base model | Detector image | [MIT License](third_party/licenses/DeBERTa-MIT.txt) |
| OWASP Top 10 for Agentic Applications 2026, by the OWASP GenAI Security Project | [`docs/references/OWASP-Top-10-for-Agentic-Applications-2026.pdf`](docs/references/OWASP-Top-10-for-Agentic-Applications-2026.pdf) | [CC BY-SA 4.0](https://creativecommons.org/licenses/by-sa/4.0/legalcode); [source and attribution](docs/references/README.md) |

Dependency metadata also identifies [MPL 2.0](https://www.mozilla.org/en-US/MPL/2.0/)
components (including `certifi` and several Go modules) and
[CC BY 4.0](https://creativecommons.org/licenses/by/4.0/legalcode) data in
`caniuse-lite`. Their licenses apply to those components and data, not to the
project's Apache-licensed source. The locked detector image uses CPU-only
ONNX Runtime and does not include PyTorch or NVIDIA CUDA packages.

The detector image includes the model and base-model license texts at
`/licenses/`. Third-party Go, JavaScript, Python, and base-image dependencies
remain under their respective upstream licenses; their package manifests and
lockfiles identify the versions used by this project.
