#!/usr/bin/env python3
"""Compares ways of deciding whether to accept a hosted judge's pick.

  score_judge.py --judge DIR [--out FILE]

"confidence" thresholds the judge's own number. "fit" accepts only picks it
called supported, with nothing to tune. "fit+confidence" ranks by category and
then by confidence, and thresholds that. Thresholds come from the calibration
rows and are measured on the test rows.
"""
import argparse, json, math, pathlib

import numpy as np

FIT = {"supported": 2, "uncertain": 1, "unsupported": 0, None: 0}


def threshold(score, ok, target):
    """The lowest score whose accepted calibration picks still meet the target.
    Equal scores are accepted together or not at all."""
    best = math.inf
    for t in sorted(set(score), reverse=True):
        sel = score >= t
        if sel.sum() >= 20 and ok[sel].mean() >= target:
            best = t
    return best


def arrays(rows):
    named = np.array([r["pick"] not in (None, "none") for r in rows])
    ok = np.array([r["pick"] == r["label"] for r in rows]) & named
    inside = np.array([r["label"] != "none" for r in rows])
    conf = np.array([r["conf"] for r in rows])
    fit = np.array([FIT.get(r["fit"], 0) for r in rows])
    return named, ok, inside, conf, fit


def measure(accept, ok, inside):
    return {"routed": float((accept & inside).sum() / inside.sum()),
            "precision": float(ok[accept].mean()) if accept.any() else None,
            "none_rejected": float((~accept[~inside]).mean()) if (~inside).any() else None,
            "wrong": int((accept & ~ok).sum()), "n": int(len(ok))}


def score_file(path):
    rows = [json.loads(l) for l in open(path)]
    rows.sort(key=lambda r: (r["split"], r["text"]))
    cal = [r for r in rows if r["split"] == "calib"]
    test = [r for r in rows if r["split"] == "test"]
    named_c, ok_c, _, conf_c, fit_c = arrays(cal)
    named, ok, inside, conf, fit = arrays(test)
    ms = sorted(r["ms"] for r in rows)
    out = {"top1": float(ok[inside].mean()),
           "none_named": float((~named[~inside]).mean()) if (~inside).any() else None,
           "right": {r["text"]: bool(o) for r, o, i in zip(test, ok, inside) if i},
           "p50": ms[len(ms) // 2], "p95": ms[int(0.95 * (len(ms) - 1))],
           "unusable": sum(r["pick"] is None for r in rows),
           "distinct_conf": len({r["conf"] for r in rows}),
           "any_pick": measure(named, ok, inside)}
    signals = {"confidence": (conf_c, conf)}
    if path.stem.endswith("-fit"):
        signals["fit+confidence"] = (fit_c * 10 + conf_c, fit * 10 + conf)
        out["fit_only"] = measure(named & (fit == 2), ok, inside)
        out["fit_share"] = {k: [int((fit == v).sum()), float(ok[fit == v].mean()) if (fit == v).any() else None]
                            for k, v in FIT.items() if k}
    for name, (s_cal, s) in signals.items():
        for target in (0.90, 0.95):
            # A pick that names no agent is never accepted.
            thr = threshold(np.where(named_c, s_cal, -1.0), ok_c, target)
            out[f"{name}@{int(target * 100)}"] = measure(named & (s >= thr), ok, inside) | {"thr": float(thr)}
    return out


if __name__ == "__main__":
    ap = argparse.ArgumentParser()
    ap.add_argument("--judge", type=pathlib.Path, required=True)
    ap.add_argument("--out", type=pathlib.Path)
    a = ap.parse_args()
    res = {p.stem.removeprefix("judge-"): score_file(p) for p in sorted(a.judge.glob("judge-*.jsonl"))}
    pct = lambda x: "–" if x is None else f"{x:.0%}"
    cell = lambda m: f"{pct(m['routed'])} at {pct(m['precision'])}, none {pct(m['none_rejected'])}"
    for key, r in res.items():
        print(f"\n== {key}: top-1 {pct(r['top1'])}  p50 {r['p50']:.0f} ms  p95 {r['p95']:.0f}  unusable {r['unusable']}  "
              f"distinct confidences {r['distinct_conf']}")
        for k in ("any_pick", "fit_only", "confidence@90", "fit+confidence@90", "confidence@95", "fit+confidence@95"):
            if k in r:
                print(f"   {k:20s} {cell(r[k])}  (wrong accepted {r[k]['wrong']}/{r[k]['n']})")
        if "fit_share" in r:
            print("   fit categories [count, share correct]:", {k: [v[0], pct(v[1])] for k, v in r["fit_share"].items()})
    print("\n== top-1, same test requests: only one of the two got it right")
    for ds in sorted({k.rsplit("-", 2)[0] for k in res}):
        for mode in ("conf", "fit"):
            s, rich = res.get(f"{ds}-short-{mode}"), res.get(f"{ds}-rich-{mode}")
            if s and rich:
                better = sum(rich["right"][t] and not s["right"][t] for t in s["right"])
                worse = sum(s["right"][t] and not rich["right"][t] for t in s["right"])
                print(f"   {ds} {mode}: rich only {better}, short only {worse}, of {len(s['right'])}")
    if a.out:
        json.dump({k: {x: y for x, y in v.items() if x != "right"} for k, v in res.items()}, open(a.out, "w"), indent=1)
