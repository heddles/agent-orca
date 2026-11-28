#!/usr/bin/env python3
"""Does a llama.cpp server reproduce the ONNX vectors of gte-small?

  check_runtime.py --url URL --bench bench.json --reference vec-gte-small.npz \
                   --cut vec-gte-small-cut128.npz --out FILE.npz

Embeds the reference's texts through llama.cpp, with requests cut to
--max-tokens by tokenizing first and sending the token ids, as the service does.
"""
import argparse, json, pathlib, sys, urllib.request

import numpy as np

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))  # -I leaves it out
from embed import pool


def post(url, body):
    req = urllib.request.Request(url, json.dumps(body).encode(), {"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=120) as r:
        return json.load(r)


def embed(url, text, limit):
    ids = post(url + "/tokenize", {"content": text, "add_special": True})["tokens"]
    if len(ids) > limit:
        ids = ids[: limit - 1] + ids[-1:]  # keep the closing [SEP]
    v = np.array(post(url + "/v1/embeddings", {"input": ids})["data"][0]["embedding"], dtype=np.float32)
    return v / np.linalg.norm(v)


if __name__ == "__main__":
    ap = argparse.ArgumentParser()
    ap.add_argument("--url", required=True)
    ap.add_argument("--max-tokens", type=int, default=128)
    for flag in ("bench", "reference", "cut", "out"):
        ap.add_argument(f"--{flag}", type=pathlib.Path, required=True)
    a = ap.parse_args()
    bench = json.load(open(a.bench))
    requests = {t for d in bench.values() for _, t in d["calib"] + d["test"]}
    full, cut = np.load(a.reference), np.load(a.cut)
    want = {str(k): v for k, v in zip(cut["keys"], cut["vecs"])}
    keys = [str(k) for k in full["keys"]]
    vecs = np.stack([embed(a.url, k, a.max_tokens if k in requests else 512) for k in keys])
    cos = np.array([float(v @ want[k]) for k, v in zip(keys, vecs)])
    print(f"{len(keys)} texts, cosine with ONNX fp32: min {cos.min():.5f}  p1 {np.percentile(cos, 1):.5f}  median {np.median(cos):.5f}")
    np.savez(a.out, keys=np.array(keys), vecs=vecs)
