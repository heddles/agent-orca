#!/usr/bin/env python3
"""Scores the local routing methods from the vectors embed.py wrote.

  score.py --bench bench.json --vectors DIR --out local.json [--rows DIR]

Anything tuned (temperature, penalty weight, confidence threshold) is tuned on
the calibration split and measured on the test split.
"""
import argparse, json, math, pathlib, random, sys, zlib

import numpy as np
from sklearn.linear_model import LogisticRegression

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))  # -I leaves it out
from embed import MODELS, card_texts, pool

SHOTS = (1, 3, 5, 10, 20)
SEEDS = 3
LAMBDAS = (0.0, 0.1, 0.25, 0.5, 1.0)
TARGETS = (0.90, 0.95)


class Vectors:
    def __init__(self, model, path):
        z = np.load(path)
        self.index = {k: i for i, k in enumerate(z["keys"])}
        self.m = z["vecs"]
        _, _, self.qp, self.dp = MODELS[model]

    def q(self, texts):
        return self.m[[self.index[self.qp + t] for t in texts]]

    def d(self, texts):
        return self.m[[self.index[self.dp + t] for t in texts]]


# --- scorers: each returns a (requests x agents) matrix ---------------------

def card_scores(v, queries, texts, variant):
    return np.stack([(queries @ v.d(t[variant]).T).max(axis=1) for t in texts], axis=1)


def negative_penalty(v, queries, texts, pos, lam, gated):
    """Lowers an agent's score by how close the request is to its "not for"
    lines. Gated: only when it is closer to one of those than to the agent."""
    out = pos.copy()
    for j, t in enumerate(texts):
        if t["neg"]:
            neg = (queries @ v.d(t["neg"]).T).max(axis=1)
            out[:, j] -= lam * (np.maximum(0, neg - pos[:, j]) if gated else neg)
    return out


def example_scores(v, queries, texts, examples, how, card="short"):
    """examples[j] holds agent j's example requests. card names the card texts
    that sit beside them."""
    sets = [np.concatenate([v.d(t[card]), v.q(e)]) for t, e in zip(texts, examples)]
    if how == "nearest":
        return np.stack([(queries @ s.T).max(axis=1) for s in sets], axis=1)
    if how == "top3":
        return np.stack([np.sort(queries @ s.T, axis=1)[:, -3:].mean(axis=1) for s in sets], axis=1)
    if how == "centroid":
        c = np.stack([s.mean(axis=0) for s in sets])
        return queries @ (c / np.linalg.norm(c, axis=1, keepdims=True)).T
    x = np.concatenate(sets)
    y = np.concatenate([[j] * len(s) for j, s in enumerate(sets)])
    clf = LogisticRegression(C=10, max_iter=2000).fit(x, y)
    return clf.decision_function(queries)


# --- confidence and thresholds ----------------------------------------------

def softmax(s, temp):
    e = np.exp((s - s.max(axis=1, keepdims=True)) / temp)
    return e / e.sum(axis=1, keepdims=True)


def fit_temperature(s, y):
    keep = y >= 0
    grid = [10 ** (i / 20) for i in range(-60, 41)]
    return min(grid, key=lambda t: -np.log(softmax(s[keep], t)[np.arange(keep.sum()), y[keep]] + 1e-12).sum())


def features(s, temp):
    top2 = np.sort(s, axis=1)[:, -2:]
    p = np.sort(softmax(s, temp), axis=1)
    return np.stack([top2[:, 1], top2[:, 1] - top2[:, 0], p[:, -1], p[:, -1] - p[:, -2]], axis=1)


def threshold(conf, ok, target):
    order = np.argsort(-conf, kind="stable")
    precision = np.cumsum(ok[order]) / np.arange(1, len(ok) + 1)
    good = np.nonzero(precision >= target)[0]
    # The most requests that can be accepted while still meeting the target. A
    # handful of lucky picks does not count as meeting it.
    good = good[good >= 19]
    return conf[order][good[-1]] if len(good) else math.inf


def evaluate(cal, y_cal, test, y_test):
    """cal and test are score matrices; y holds the right agent, or -1."""
    temp = fit_temperature(cal, y_cal)
    pick_cal, pick = cal.argmax(axis=1), test.argmax(axis=1)
    ok_cal, ok = pick_cal == y_cal, pick == y_test
    inside = y_test >= 0
    top3 = (np.argsort(-test, axis=1)[:, :3] == y_test[:, None]).any(axis=1)
    out = {"top1": ok[inside].mean(), "top3": top3[inside].mean()}
    signals = {"softmax": (softmax(cal, temp).max(axis=1), softmax(test, temp).max(axis=1))}
    # A gate learned on the calibration split from how the scores are spread.
    f_cal, f = features(cal, temp), features(test, temp)
    mean, std = f_cal.mean(axis=0), f_cal.std(axis=0) + 1e-9
    gate = LogisticRegression(max_iter=2000).fit((f_cal - mean) / std, ok_cal)
    signals["gate"] = (gate.predict_proba((f_cal - mean) / std)[:, 1], gate.predict_proba((f - mean) / std)[:, 1])
    for name, (c_cal, c) in signals.items():
        for target in TARGETS:
            thr = threshold(c_cal, ok_cal, target)
            accepted = c >= thr
            key = f"{name}@{int(target * 100)}"
            out[key] = {"routed": (accepted & inside).sum() / inside.sum(),
                        "precision": ok[accepted].mean() if accepted.any() else None,
                        "none_rejected": (~accepted[~inside]).mean() if (~inside).any() else None}
    conf = signals["gate"][1]
    rows = {"pick": pick.tolist(), "conf": conf.tolist(),
            "accept90": (conf >= threshold(signals["gate"][0], ok_cal, 0.90)).tolist(),
            "accept95": (conf >= threshold(signals["gate"][0], ok_cal, 0.95)).tolist()}
    return out, rows


def mean_of(results):
    first = results[0]
    if isinstance(first, dict):
        return {k: mean_of([r[k] for r in results]) for k in first}
    vals = [r for r in results if r is not None]
    return float(np.mean(vals)) if vals else None


def run(v, d):
    names = list(d["cards"])
    texts = [card_texts(n, d["cards"][n]) for n in names]
    label = lambda rows: np.array([-1 if l == "none" else names.index(l) for l, _ in rows])
    y_cal, y = label(d["calib"]), label(d["test"])
    q_cal, q = v.q([t for _, t in d["calib"]]), v.q([t for _, t in d["test"]])
    results, rows = {}, {}

    def add(key, score):
        results[key], rows[key] = evaluate(score(q_cal), y_cal, score(q), y)

    has_use = any(c["use"] for c in d["cards"].values())
    variants = ("name", "short") + (("use", "use_not", "lines") if has_use else ())
    for variant in variants:
        add(variant, lambda x: card_scores(v, x, texts, variant))
    if has_use:
        for gated in (False, True):
            def penalised(x, lam):
                return negative_penalty(v, x, texts, card_scores(v, x, texts, "lines"), lam, gated)
            inside = y_cal >= 0
            lam = max(LAMBDAS, key=lambda l: (penalised(q_cal, l).argmax(axis=1) == y_cal)[inside].mean())
            key = "lines+neg-gated" if gated else "lines+neg"
            add(key, lambda x: penalised(x, lam))
            results[key]["lambda"] = lam
            # What the same penalty does at full strength, tuned or not.
            add(key + "(λ=0.5)", lambda x: penalised(x, 0.5))
    if d["examples"]:
        hows = [(h, "short", h) for h in ("nearest", "top3", "centroid", "logreg")]
        if has_use:  # "use when" lines and examples together
            hows += [(h, "lines", f"{h}+lines") for h in ("top3", "centroid", "logreg")]
        for how, card, label in hows:
            for k in SHOTS:
                got = []
                for seed in range(SEEDS):
                    examples = []
                    for n in names:
                        p = pool(d["examples"], n)
                        random.Random(zlib.crc32(f"{n}/{seed}".encode())).shuffle(p)
                        examples.append(p[:k])
                    res, r = evaluate(example_scores(v, q_cal, texts, examples, how, card), y_cal,
                                      example_scores(v, q, texts, examples, how, card), y)
                    got.append(res)
                    if seed == 0:
                        rows[f"{label}-{k}"] = r
                results[f"{label}-{k}"] = mean_of(got)
    return results, rows


if __name__ == "__main__":
    ap = argparse.ArgumentParser()
    for flag in ("bench", "vectors", "out", "rows"):
        ap.add_argument(f"--{flag}", type=pathlib.Path, required=True)
    ap.add_argument("--models", nargs="*", default=list(MODELS))
    a = ap.parse_args()
    bench = json.load(open(a.bench))
    out, rows = {}, {}
    for model in a.models:
        path = a.vectors / f"vec-{model}.npz"
        if not path.exists():
            continue
        v = Vectors(model, path)
        for ds, d in bench.items():
            out.setdefault(model, {})[ds], rows.setdefault(model, {})[ds] = run(v, d)
        print(model, "scored", flush=True)
    json.dump(out, open(a.out, "w"), default=float)
    json.dump(rows, open(a.rows / "rows.json", "w"))
