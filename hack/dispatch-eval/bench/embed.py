#!/usr/bin/env python3
"""Embeds the benchmark with one ONNX model, or times it.

  embed.py vectors MODEL --dir DIR --bench bench.json --out DIR
  embed.py latency MODEL --dir DIR --bench bench.json --out DIR   (run with --cpus=2)
"""
import argparse, json, pathlib, random, re, statistics, sys, time

import numpy as np
import onnxruntime as ort
from tokenizers import Tokenizer

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))  # -I leaves it out
from embed_cards import card_texts

TASK = "Given a user request, retrieve the agent that should handle it"
POOL_PER_AGENT = 25

# name: (onnx file, pooling, query prefix, document prefix)
MODELS = {
    "potion": ("onnx/model.onnx", "static", "", ""),
    "bge-small": ("onnx/model.onnx", "cls", "Represent this sentence for searching relevant passages: ", ""),
    "gte-small": ("onnx/model.onnx", "mean", "", ""),
    "e5-small": ("model.onnx", "mean", "query: ", "passage: "),
    "bge-base": ("onnx/model.onnx", "cls", "Represent this sentence for searching relevant passages: ", ""),
    "e5-base": ("onnx/model.onnx", "mean", "query: ", "passage: "),
    "gte-modernbert": ("onnx/model.onnx", "cls", "", ""),
    "gte-modernbert-int8": ("onnx/model_int8.onnx", "cls", "", ""),
    "gemma-q4": ("onnx/model_q4.onnx", "sentence", "task: classification | query: ", "title: none | text: "),
    "qwen3-int8": ("onnx/model_int8.onnx", "last", f"Instruct: {TASK}\nQuery:", ""),
    # Served by llama.cpp (embed_llamacpp.py): the int8 ONNX export of Qwen3 did
    # not reproduce it.
    "qwen3-gguf": ("", "last", f"Instruct: {TASK}\nQuery:", ""),
    # llama.cpp builds, written by check_runtime.py.
    "gte-small-q8": ("", "mean", "", ""),
    "gte-small-f32": ("", "mean", "", ""),
    "gemma": ("onnx/model.onnx", "sentence", "task: classification | query: ", "title: none | text: "),
    "e5-large": ("onnx/model.onnx", "mean", "query: ", "passage: "),
    "gte-large": ("onnx/model.onnx", "cls", "", ""),
    "gte-large-int8": ("onnx/model_int8.onnx", "cls", "", ""),
}


class Model:
    def __init__(self, name, directory, threads, max_tokens=512):
        file, self.pooling, self.qp, self.dp = MODELS[name]
        opts = ort.SessionOptions()
        opts.intra_op_num_threads, opts.inter_op_num_threads = threads, 1
        self.s = ort.InferenceSession(str(directory / file), opts, providers=["CPUExecutionProvider"])
        self.tok = Tokenizer.from_file(str(directory / "tokenizer.json"))
        self.tok.no_padding()
        self.tok.enable_truncation(max_length=max_tokens)

    def __call__(self, text):
        enc = self.tok.encode(text, add_special_tokens=self.pooling != "static")
        ids = np.array([enc.ids], dtype=np.int64)
        n = ids.shape[1]
        feed = {}
        for i in self.s.get_inputs():
            if i.name == "input_ids":
                feed[i.name] = ids[0] if self.pooling == "static" else ids
            elif i.name == "offsets":
                feed[i.name] = np.array([0], dtype=np.int64)
            elif i.name == "attention_mask":
                feed[i.name] = np.ones((1, n), dtype=np.int64)
            elif i.name == "token_type_ids":
                feed[i.name] = np.zeros((1, n), dtype=np.int64)
            elif i.name == "position_ids":
                feed[i.name] = np.arange(n, dtype=np.int64)[None]
            elif i.name.startswith("past_key_values"):
                # An empty cache: batch 1, no past tokens.
                shape = [1 if k == 0 else (0 if k == 2 else d) for k, d in enumerate(i.shape)]
                feed[i.name] = np.zeros(shape, dtype=np.float16 if "float16" in i.type else np.float32)
            else:
                raise SystemExit(f"unknown input {i.name}")
        names = [o.name for o in self.s.get_outputs()]
        want = "sentence_embedding" if self.pooling == "sentence" else names[0]
        out = self.s.run([want], feed)[0]
        if self.pooling in ("static", "sentence"):
            v = out.reshape(-1)
        elif self.pooling == "cls":
            v = out[0, 0]
        elif self.pooling == "last":
            v = out[0, -1]
        else:
            v = out[0].mean(axis=0)
        v = v.astype(np.float32)
        return v / np.linalg.norm(v)


def pool(examples, name):
    rows = list(examples.get(name, []))
    random.Random(name).shuffle(rows)
    return rows[:POOL_PER_AGENT]


if __name__ == "__main__":
    ap = argparse.ArgumentParser()
    ap.add_argument("cmd", choices=["vectors", "latency"])
    ap.add_argument("model", choices=list(MODELS))
    for flag in ("dir", "bench", "out"):
        ap.add_argument(f"--{flag}", type=pathlib.Path, required=True)
    ap.add_argument("--max-tokens", type=int, default=512, help="cut every input to this many tokens")
    a = ap.parse_args()
    bench = json.load(open(a.bench))

    if a.cmd == "latency":
        m = Model(a.model, a.dir, threads=2, max_tokens=a.max_tokens)
        res = {}
        for ds in ("clinc", "k8s"):
            texts = [m.qp + t for _, t in bench[ds]["test"][:210]]
            ms, tokens = [], []
            for i, t in enumerate(texts):
                start = time.perf_counter()
                m(t)
                if i >= 10:  # the first runs warm the session up
                    ms.append((time.perf_counter() - start) * 1000)
                    tokens.append(len(m.tok.encode(t).ids))
            ms.sort()
            res[ds] = {"p50": statistics.median(ms), "p95": ms[int(0.95 * (len(ms) - 1))], "tokens": statistics.median(tokens)}
        rss = int(re.search(r"VmRSS:\s+(\d+)", open("/proc/self/status").read()).group(1)) / 1024
        res["rss_mib"] = rss
        json.dump(res, open(a.out / f"latency-{a.model}.json", "w"))
        print(a.model, {k: v if k == "rss_mib" else {x: round(y, 1) for x, y in v.items()} for k, v in res.items()})
    else:
        m = Model(a.model, a.dir, threads=8)
        short = Model(a.model, a.dir, threads=8, max_tokens=a.max_tokens)
        keys, vecs, seen = [], [], set()

        def add(text, model=m):
            if text not in seen:
                seen.add(text)
                keys.append(text)
                vecs.append(model(text))

        for d in bench.values():
            for name, card in d["cards"].items():
                for variant, texts in card_texts(name, card).items():
                    for t in texts:
                        # A "not for" line is compared with the request the same
                        # way a document is.
                        add(m.dp + t)
                for t in pool(d["examples"], name):
                    add(m.qp + t)  # an example is itself a request
            for _, t in d["calib"] + d["test"]:
                add(m.qp + t, short)
        np.savez(a.out / f"vec-{a.model}.npz", keys=np.array(keys), vecs=np.stack(vecs))
        print(a.model, "embedded", len(keys), flush=True)
