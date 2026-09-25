import importlib.util
from pathlib import Path
import sys
import types
import unittest
from unittest.mock import Mock, patch


class DetectorModelPinTest(unittest.TestCase):
    def test_runtime_uses_only_the_baked_onnx_model(self):
        fastapi = types.ModuleType("fastapi")
        fastapi.FastAPI = lambda: types.SimpleNamespace(
            get=lambda _: lambda fn: fn,
            post=lambda _: lambda fn: fn,
        )
        pydantic = types.ModuleType("pydantic")
        pydantic.BaseModel = type("BaseModel", (), {})
        transformers = types.ModuleType("transformers")
        transformers.AutoTokenizer = types.SimpleNamespace(from_pretrained=Mock())
        transformers.AutoConfig = types.SimpleNamespace(from_pretrained=Mock(
            return_value=types.SimpleNamespace(id2label={0: "benign", 1: "injection"})
        ))
        onnxruntime = types.ModuleType("onnxruntime")
        onnxruntime.InferenceSession = Mock()
        numpy = types.ModuleType("numpy")
        modules = {
            "fastapi": fastapi,
            "pydantic": pydantic,
            "transformers": transformers,
            "onnxruntime": onnxruntime,
            "numpy": numpy,
        }
        with patch.dict(sys.modules, modules):
            spec = importlib.util.spec_from_file_location("detector_app", Path(__file__).with_name("app.py"))
            module = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(module)

        transformers.AutoTokenizer.from_pretrained.assert_called_once_with(
            "/opt/hf/model", local_files_only=True
        )
        transformers.AutoConfig.from_pretrained.assert_called_once_with(
            "/opt/hf/model", local_files_only=True
        )
        onnxruntime.InferenceSession.assert_called_once_with(
            "/opt/hf/model/model.onnx", providers=["CPUExecutionProvider"]
        )


if __name__ == "__main__":
    unittest.main()
