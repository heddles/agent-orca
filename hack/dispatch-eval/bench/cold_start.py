#!/usr/bin/env python3
"""What happens to precision when a tenant has no labelled calibration traffic,
so the temperature and threshold come only from its own examples (each one held
out in turn).

  cold_start.py --bench bench.json --vectors DIR
"""
import argparse, json, pathlib, random, sys, zlib

import numpy as np

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))  # -I leaves it out
from embed import card_texts, pool
from score import Vectors, example_scores, fit_temperature, softmax, threshold, SEEDS

if __name__ == "__main__":
    ap = argparse.ArgumentParser()
    ap.add_argument("--bench", type=pathlib.Path, required=True)
    ap.add_argument("--vectors", type=pathlib.Path, required=True)
    ap.add_argument("--models", nargs="*", default=["gte-small", "gte-modernbert-int8", "gemma-q4"])
    a = ap.parse_args()
    bench = json.load(open(a.bench))
    for model in a.models:
        v = Vectors(model, a.vectors / f"vec-{model}.npz")
        for k in (5, 10, 20):
            for ds in ("k8s", "clinc", "massive"):
                d = bench[ds]
                names = list(d["cards"])
                texts = [card_texts(n, d["cards"][n]) for n in names]
                y = np.array([-1 if l == "none" else names.index(l) for l, _ in d["test"]])
                q = v.q([t for _, t in d["test"]])
                got = {t: [] for t in (0.90, 0.95)}
                for seed in range(SEEDS):
                    examples = []
                    for n in names:
                        p = pool(d["examples"], n)
                        random.Random(zlib.crc32(f"{n}/{seed}".encode())).shuffle(p)
                        examples.append(p[:k])
                    held, held_y = [], []
                    for j, ex in enumerate(examples):
                        for i, one in enumerate(ex):
                            rest = [e[:i] + e[i + 1 :] if x == j else e for x, e in enumerate(examples)]
                            held.append(example_scores(v, v.q([one]), texts, rest, "logreg", "lines")[0])
                            held_y.append(j)
                    held, held_y = np.stack(held), np.array(held_y)
                    temp = fit_temperature(held, held_y)
                    conf_held = softmax(held, temp).max(axis=1)
                    s = example_scores(v, q, texts, examples, "logreg", "lines")
                    conf, ok = softmax(s, temp).max(axis=1), s.argmax(axis=1) == y
                    for target in got:
                        acc = conf >= threshold(conf_held, held.argmax(axis=1) == held_y, target)
                        inside = y >= 0
                        got[target].append((((acc & inside).sum() / inside.sum()), ok[acc].mean() if acc.any() else np.nan,
                                            (~acc[~inside]).mean() if (~inside).any() else np.nan))
                line = []
                for target, g in got.items():
                    r, p, n = np.nanmean(np.array(g), axis=0) if any(not np.isnan(x[1]) for x in g) else (0, np.nan, np.nan)
                    worst = np.nanmin([x[1] for x in g])
                    line.append(f"target {target:.0%}: routed {r:4.0%} at {p:4.0%} (worst draw {worst:4.0%})"
                                + (f", none rejected {n:4.0%}" if not np.isnan(n) else ""))
                print(f"{model:20s} k={k:<3d}{ds:8s} " + " | ".join(line), flush=True)
