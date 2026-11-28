#!/usr/bin/env python3
"""Sends the test rows to a running orca-decide and scores its answers, to check
the service against the numbers defaults.py computes offline.

  replay.py --url URL --bench bench.json [--shots 0 5 20]
"""
import argparse, json, pathlib, statistics, sys, time, urllib.request

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))  # -I leaves it out
from embed import card_texts, pool
import random, zlib

THRESHOLD = 0.8
# The service's limits. A real Agent cannot exceed them; the longest benchmark
# cards and k8s example issues do, so they are trimmed here.
MAX_DESCRIPTIONS, MAX_TEXT_BYTES = 24, 1200


def trim(text):
    return text.encode()[:MAX_TEXT_BYTES].decode(errors="ignore")


def decide(url, body):
    req = urllib.request.Request(url + "/v1/decisions", json.dumps(body).encode(), {"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=180) as r:
        return json.load(r)


if __name__ == "__main__":
    ap = argparse.ArgumentParser()
    ap.add_argument("--url", required=True)
    ap.add_argument("--bench", type=pathlib.Path, required=True)
    ap.add_argument("--shots", nargs="*", type=int, default=[0, 5, 20])
    a = ap.parse_args()
    bench = json.load(open(a.bench))
    for k in a.shots:
        for ds in ("k8s", "clinc", "massive"):
            d = bench[ds]
            criteria, texts = {}, {}
            for name, card in d["cards"].items():
                lines = card_texts(name, card)["lines"]
                examples = pool(d["examples"], name)
                random.Random(zlib.crc32(f"{name}/0".encode())).shuffle(examples)
                criteria[name] = trim(lines[0])
                texts[name] = {"descriptions": [trim(l) for l in lines[:MAX_DESCRIPTIONS]],
                               "examples": [trim(e) for e in examples[:k]]}
            body = lambda t: {"tenant": f"{ds}-{k}", "state": t, "questions": {"agent": {"type": "choice", "criteria": criteria, "texts": texts}}}
            start = time.perf_counter()
            model = decide(a.url, body(d["test"][0][1]))["model"]
            cold = time.perf_counter() - start
            right = inside = routed = accepted = correct = 0
            ms = []
            for label, text in d["test"]:
                start = time.perf_counter()
                answer = decide(a.url, body(text))["answers"]["agent"]
                ms.append((time.perf_counter() - start) * 1000)
                ok = answer["choice"] == label
                if label != "none":
                    inside += 1
                    right += ok
                if answer["confidence"] >= THRESHOLD:
                    accepted += 1
                    correct += ok
                    routed += label != "none"
            ms.sort()
            print(f"examples {k:<3d}{ds:8s}{model:26s} top-1 {right / inside:4.0%}  routed {routed / inside:4.0%} at "
                  f"{correct / accepted if accepted else 1:4.0%}  prepare {cold:5.2f}s  p50 {statistics.median(ms):5.1f}ms  p95 {ms[int(.95 * (len(ms) - 1))]:5.1f}ms", flush=True)
