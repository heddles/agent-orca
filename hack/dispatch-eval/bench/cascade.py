#!/usr/bin/env python3
"""Simulates asking the local router first and the hosted judge only when the
local one is unsure, on the test rows the judge answered.

  cascade.py --bench bench.json --vectors DIR --judge DIR [--models ...] [--shots ...]

Both thresholds come from calibration rows. A request neither accepts goes to
the tenant's default agent, so it counts as not routed.
"""
import argparse, json, pathlib, random, sys, zlib

import numpy as np

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))  # -I leaves it out
from embed import card_texts, pool
from score import Vectors, example_scores, fit_temperature, softmax, threshold, SEEDS
from score_judge import arrays, threshold as judge_threshold


def local(v, d, k, seed):
    """Pick, and whether it is accepted at each target, for every test text."""
    names = list(d["cards"])
    texts = [card_texts(n, d["cards"][n]) for n in names]
    examples = []
    for n in names:
        p = pool(d["examples"], n)
        random.Random(zlib.crc32(f"{n}/{seed}".encode())).shuffle(p)
        examples.append(p[:k])
    y_cal = np.array([-1 if l == "none" else names.index(l) for l, _ in d["calib"]])
    cal = example_scores(v, v.q([t for _, t in d["calib"]]), texts, examples, "logreg", "lines")
    test = example_scores(v, v.q([t for _, t in d["test"]]), texts, examples, "logreg", "lines")
    temp = fit_temperature(cal, y_cal)
    c_cal, c = softmax(cal, temp).max(axis=1), softmax(test, temp).max(axis=1)
    ok_cal = cal.argmax(axis=1) == y_cal
    out = {}
    for target in (0.90, 0.95):
        thr = threshold(c_cal, ok_cal, target)
        out[target] = {t: (names[i], bool(x >= thr)) for (_, t), i, x in zip(d["test"], test.argmax(axis=1), c)}
    return out


def judge(path, target=0.90):
    rows = [json.loads(l) for l in open(path)]
    cal = [r for r in rows if r["split"] == "calib"]
    named, ok, _, conf, _ = arrays(cal)
    thr = judge_threshold(np.where(named, conf, -1.0), ok, target)
    return {r["text"]: (r["label"], r["pick"], r["pick"] not in (None, "none") and r["conf"] >= thr, r["ms"])
            for r in rows if r["split"] == "test"}


def tally(decisions):
    """decisions: (label, pick or None when not routed, asked the judge)."""
    inside = sum(l != "none" for l, _, _ in decisions)
    routed = [(l, p) for l, p, _ in decisions if p is not None]
    none = [p for l, p, _ in decisions if l == "none"]
    return np.array([sum(l != "none" for l, _ in routed) / inside,
                     sum(l == p for l, p in routed) / len(routed) if routed else np.nan,
                     sum(p is None for p in none) / len(none) if none else np.nan,
                     sum(a for _, _, a in decisions) / len(decisions)])


if __name__ == "__main__":
    ap = argparse.ArgumentParser()
    for flag in ("bench", "vectors", "judge"):
        ap.add_argument(f"--{flag}", type=pathlib.Path, required=True)
    ap.add_argument("--models", nargs="*", default=["gte-small", "gte-modernbert-int8", "gemma-q4"])
    ap.add_argument("--shots", nargs="*", type=int, default=[5, 20])
    a = ap.parse_args()
    bench = json.load(open(a.bench))

    def show(name, t):
        pct = lambda x: "  –" if np.isnan(x) else f"{x:4.0%}"
        print(f"  {name:44s} routed {pct(t[0])} at {pct(t[1])}, none rejected {pct(t[2])}, asked the judge {pct(t[3])}")

    for ds in ("k8s", "clinc", "massive"):
        j = judge(a.judge / f"judge-{ds}-use-conf.jsonl")
        print(f"\n== {ds} ({len(j)} requests)")
        show("judge only", tally([(l, p if ok else None, True) for l, p, ok, _ in j.values()]))
        for model in a.models:
            v = Vectors(model, a.vectors / f"vec-{model}.npz")
            for k in a.shots:
                runs = [local(v, bench[ds], k, s) for s in range(SEEDS)]
                for target in (0.90, 0.95):
                    alone, both = [], []
                    for r in runs:
                        one, two = [], []
                        for text, (label, jp, jok, _) in j.items():
                            pick, ok = r[target][text]
                            one.append((label, pick if ok else None, False))
                            two.append((label, pick, False) if ok else (label, jp if jok else None, True))
                        alone.append(tally(one))
                        both.append(tally(two))
                    show(f"{model} k={k} local@{int(target * 100)} only", np.mean(alone, axis=0))
                    show(f"{model} k={k} local@{int(target * 100)} then judge", np.mean(both, axis=0))
