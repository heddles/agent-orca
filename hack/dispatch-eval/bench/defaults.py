#!/usr/bin/env python3
"""What orca-decide's fixed settings are based on.

  defaults.py --bench bench.json --vectors vec-gte-small-q8.npz

1. One temperature per method and one threshold for both, measured on test rows.
2. Agents with unequal numbers of examples: half the agents get the larger
   count, half the smaller (which may be none).
"""
import argparse, json, pathlib, random, sys, zlib

import numpy as np
from sklearn.linear_model import LogisticRegression

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))  # -I leaves it out
from embed import card_texts, pool
from score import Vectors, card_scores, softmax

DATASETS = ("k8s", "clinc", "massive")
T_DESCRIPTIONS, T_EXAMPLES, THRESHOLD = 0.02, 0.5, 0.8


def shuffled(d, name, seed):
    p = pool(d["examples"], name)
    random.Random(zlib.crc32(f"{name}/{seed}".encode())).shuffle(p)
    return p


def logreg(sets, q, **kw):
    x = np.concatenate(sets)
    y = np.concatenate([[j] * len(s) for j, s in enumerate(sets)])
    return LogisticRegression(C=10, max_iter=3000, **kw).fit(x, y).decision_function(q)


def measure(s, temp, y, rich):
    ok, conf = s.argmax(axis=1) == y, softmax(s, temp).max(axis=1)
    accepted, inside = conf >= THRESHOLD, y >= 0
    group = lambda m: ok[m].mean() if m.any() else np.nan
    return [group(inside), group(inside & np.isin(y, list(rich))), group(inside & ~np.isin(y, list(rich))),
            (accepted & inside).sum() / inside.sum(), group(accepted)]


if __name__ == "__main__":
    ap = argparse.ArgumentParser()
    ap.add_argument("--bench", type=pathlib.Path, required=True)
    ap.add_argument("--vectors", type=pathlib.Path, required=True)
    a = ap.parse_args()
    bench, v = json.load(open(a.bench)), Vectors("gte-small-q8", a.vectors)
    data = {}
    for ds in DATASETS:
        d = bench[ds]
        names = list(d["cards"])
        texts = [card_texts(n, d["cards"][n]) for n in names]
        y = np.array([-1 if l == "none" else names.index(l) for l, _ in d["test"]])
        data[ds] = (d, names, texts, v.q([t for _, t in d["test"]]), y)

    print(f"Fixed settings (T {T_DESCRIPTIONS} / {T_EXAMPLES}, accept at >= {THRESHOLD}): routed % at precision %")
    for k in (0, 1, 5, 10, 20):
        line = []
        for ds, (d, names, texts, q, y) in data.items():
            if k == 0:
                s, temp = card_scores(v, q, texts, "lines"), T_DESCRIPTIONS
            else:
                sets = [np.concatenate([v.d(t["lines"]), v.q(shuffled(d, n, 0)[:k])]) for n, t in zip(names, texts)]
                s, temp = logreg(sets, q, class_weight="balanced"), T_EXAMPLES
            m = measure(s, temp, y, set())
            line.append(f"{ds}: {m[3] * 100:3.0f} @ {m[4] * 100:3.0f}")
        print(f"  examples per agent {k:<3d} " + "   ".join(line))

    print("\nUnequal examples: top-1 all / larger / smaller | routed % at precision %")
    for lo, hi in ((0, 5), (0, 20), (1, 20), (3, 20), (5, 20), (10, 20), (5, 50)):
        for ds, (d, names, texts, q, y) in data.items():
            got = {}
            for seed in range(4):
                rich = set(random.Random(seed).sample(range(len(names)), len(names) // 2))
                ex = [shuffled(d, n, seed)[: hi if j in rich else lo] for j, n in enumerate(names)]
                lines = [v.d(t["lines"]) for t in texts]
                sets = [np.concatenate([l, v.q(e)]) if e else l for l, e in zip(lines, ex)]
                fewest = [np.concatenate([l, v.q(e[:lo])]) if lo else l for l, e in zip(lines, ex)]
                runs = {"descriptions only": (card_scores(v, q, texts, "lines"), T_DESCRIPTIONS),
                        "logreg": (logreg(sets, q), T_EXAMPLES),
                        "logreg balanced": (logreg(sets, q, class_weight="balanced"), T_EXAMPLES),
                        "logreg capped at fewest": (logreg(fewest, q), T_EXAMPLES)}
                for name, (s, temp) in runs.items():
                    got.setdefault(name, []).append(measure(s, temp, y, rich))
            for name, rows in got.items():
                m = np.nanmean(np.array(rows), axis=0) * 100
                print(f"  {f'{lo}/{hi}':6s}{ds:8s}{name:25s}{m[0]:3.0f} / {m[1]:3.0f} / {m[2]:3.0f} | {m[3]:3.0f} @ {m[4]:3.0f}")
