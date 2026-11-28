#!/usr/bin/env python3
"""Writes and times Qwen3 embeddings from a llama.cpp server, as embed.py does
for ONNX models.

  embed_llamacpp.py vectors|latency --url URL --bench bench.json --out DIR

"vectors" embeds the texts listed in the vec-qwen3-int8.npz that embed.py wrote,
which uses the same prefixes.
"""
import argparse, json, pathlib, statistics, time, urllib.request

import numpy as np


def vec(url, text):
    req = urllib.request.Request(url + "/v1/embeddings", json.dumps({"input": text}).encode(), {"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=120) as r:
        v = np.array(json.load(r)["data"][0]["embedding"], dtype=np.float32)
    return v / np.linalg.norm(v)


if __name__ == "__main__":
    ap = argparse.ArgumentParser()
    ap.add_argument("cmd", choices=["vectors", "latency"])
    ap.add_argument("--url", default="http://127.0.0.1:18090")
    for flag in ("bench", "out"):
        ap.add_argument(f"--{flag}", type=pathlib.Path, required=True)
    a = ap.parse_args()
    bench = json.load(open(a.bench))
    prefix = "Instruct: Given a user request, retrieve the agent that should handle it\nQuery:"
    if a.cmd == "latency":
        res = {}
        for ds in ("clinc", "k8s"):
            ms = []
            for i, (_, t) in enumerate(bench[ds]["test"][:210]):
                start = time.perf_counter()
                vec(a.url, prefix + t)
                if i >= 10:
                    ms.append((time.perf_counter() - start) * 1000)
            ms.sort()
            res[ds] = {"p50": statistics.median(ms), "p95": ms[int(0.95 * (len(ms) - 1))]}
        json.dump(res, open(a.out / "latency-qwen3-gguf.json", "w"))
        print({k: {x: round(y, 1) for x, y in v.items()} for k, v in res.items()})
    else:
        # The same texts the ONNX run embedded: cards as written, requests and
        # examples with this model's query prefix.
        keys = [str(k) for k in np.load(a.out / "vec-qwen3-int8.npz")["keys"]]
        np.savez(a.out / "vec-qwen3-gguf.npz", keys=np.array(keys), vecs=np.stack([vec(a.url, k) for k in keys]))
        print("embedded", len(keys))
