from fastapi import FastAPI
from pydantic import BaseModel
from onnxruntime import InferenceSession
from transformers import AutoConfig, AutoTokenizer
import numpy as np

MODEL_DIR = "/opt/hf/model"
app = FastAPI()
_tok = AutoTokenizer.from_pretrained(MODEL_DIR, local_files_only=True)
_model = InferenceSession(MODEL_DIR + "/model.onnx", providers=["CPUExecutionProvider"])
_labels = AutoConfig.from_pretrained(MODEL_DIR, local_files_only=True).id2label

class Req(BaseModel):
    text: str

def _injection_index() -> int:
    for i, name in _labels.items():
        if "inj" in str(name).lower() or "mali" in str(name).lower() or str(i) == "1":
            return int(i)
    return 1

_INJ = _injection_index()

@app.get("/healthz")
def healthz():
    return {"ok": True}

@app.post("/")
def classify(r: Req):
    enc = _tok(r.text, truncation=True, max_length=512, return_tensors="np")
    inputs = {item.name: enc[item.name] for item in _model.get_inputs()}
    logits = _model.run(None, inputs)[0][0]
    e = np.exp(logits - logits.max()); probs = e / e.sum()
    score = float(probs[_INJ])
    return {"score": score, "label": "injection" if score >= 0.5 else "benign"}
