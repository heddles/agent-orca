#!/usr/bin/env python3
"""Builds bench.json from the downloaded datasets.

  prepare.py --clinc DIR --clinc-domains DIR --massive DIR --k8s DIR --ops DIR --out FILE

Every set has agent cards, an example pool (what a tenant could put in
spec.routing.examples), a calibration split and a test split. The three splits
never share a request. The label "none" means no agent fits.
"""
import argparse, json, random, re, pathlib

import pyarrow.parquet as pq
import yaml

CALIB, CALIB_NONE, TEST, TEST_NONE = 400, 100, 800, 200


def split(rows_by_label, calib_n, test_n, rng, exclude=()):
    """Draws calibration and test rows, spread evenly over the labels."""
    calib, test = [], []
    labels = sorted(rows_by_label)
    for label in labels:
        rows = [t for t in rows_by_label[label] if t not in exclude]
        rng.shuffle(rows)
        c, t = calib_n // len(labels), test_n // len(labels)
        if len(rows) < c + t:  # keep the same ratio in a small class
            c = len(rows) * c // (c + t)
            t = len(rows) - c
        calib += [[label, x] for x in rows[:c]]
        test += [[label, x] for x in rows[c : c + t]]
    rng.shuffle(calib)
    rng.shuffle(test)
    return calib, test


def assemble(cards, examples, in_calib_test, none_texts, rng, none_n=(CALIB_NONE, TEST_NONE)):
    calib, test = split(in_calib_test, CALIB, TEST, rng)
    if none_texts:
        nc, nt = split({"none": none_texts}, none_n[0], none_n[1], rng)
        calib, test = calib + nc, test + nt
        rng.shuffle(calib)
        rng.shuffle(test)
    return {"cards": cards, "examples": examples, "calib": calib, "test": test}


def clinc(data, domains, cards, rng):
    meta = json.loads(pq.read_schema(data / "test.parquet").metadata[b"huggingface"])
    names = meta["info"]["features"]["intent"]["names"]
    domain = {i: d for d, intents in json.load(open(domains / "domains.json")).items() for i in intents}

    def by_domain(split_name):
        out, oos = {}, []
        t = pq.read_table(data / f"{split_name}.parquet").to_pylist()
        for r in t:
            name = names[r["intent"]]
            if name == "oos":
                oos.append(r["text"])
            else:
                out.setdefault(domain[name], []).append(r["text"])
        return out, oos

    pool, _ = by_domain("train")
    held, oos = by_domain("test")
    return assemble(cards, pool, held, oos, rng)


def massive(data, cards, rng):
    def read(name):
        out = {}
        for line in open(data / f"{name}.jsonl"):
            r = json.loads(line)
            out.setdefault(r["label_text"], []).append(r["text"])
        return out

    pool, held = read("train"), read("test")
    none = held.pop("general")  # MASSIVE's catch-all stands for "no agent fits"
    pool.pop("general")
    assert set(held) == set(cards), set(held) ^ set(cards)
    return assemble(cards, pool, held, none, rng)


SIGS = ["node", "network", "storage", "scheduling", "api-machinery", "auth", "apps", "cli", "autoscaling", "instrumentation"]


def clean_md(s):
    s = re.sub(r"!?\[([^\]]*)\]\([^)]*\)", r"\1", s)
    s = re.sub(r"^\[[^\]]+\]:.*$", "", s, flags=re.M)
    s = re.sub(r"\[([^\]]+)\](\[[^\]]*\])?", r"\1", s)
    s = re.sub(r"^#+\s*", "", s, flags=re.M)
    s = re.sub(r"^\s*[-*]\s*", "", s, flags=re.M)
    headings = ("sig readme", "code, binaries and services", "cross-cutting and externally facing processes")
    return [l.strip() for l in s.splitlines() if len(l.strip()) > 3 and l.strip().lower() not in headings]


def section(charter, title):
    m = re.search(rf"^### {title}\s*$(.*?)(?=^##\s|^### (?!#)|\Z)", charter, flags=re.M | re.S | re.I)
    return clean_md(m.group(1)) if m else []


def capped(lines, chars=1200):
    out, n = [], 0
    for l in lines:
        if n + len(l) > chars:
            break
        out.append(l)
        n += len(l)
    return out


def issue_text(title, body):
    body = re.sub(r"<!--.*?-->", " ", body, flags=re.S)
    keep = []
    for line in body.splitlines():
        s = line.strip()
        # Prow commands and template headings, then template boilerplate.
        if s.startswith(("/", "#")) or re.match(r"(?i)^(\*\*)?(what happened|what did you expect|how can we reproduce|anything else|kubernetes version|cloud provider|os version|install tools|container runtime|related plugins)", s):
            continue
        keep.append(s)
    text = f"{title}\n{' '.join(k for k in keep if k)}"
    # Remove anything that names the owning team.
    teams = "|".join(s.replace("-", "[ -]?") for s in SIGS + ["api machinery", "testing", "windows", "architecture", "release", "scalability"])
    text = re.sub(rf"(?i)\[?\bsigs?[ \-/:]*({teams})\b\]?", " ", text)
    text = re.sub(r"@[\w\-/]+", " ", text)  # people and team handles
    return " ".join(text.split())[:1000]


def k8s(data, rng):
    sigs = {s["dir"]: s for s in yaml.safe_load(open(data / "sigs.yaml"))["sigs"]}
    cards, pool, held = {}, {}, {}
    for s in SIGS:
        charter = (data / f"charter-{s}.md").read_text()
        use, out = section(charter, "In scope"), section(charter, "Out of scope")
        assert use and out, f"{s}: charter sections not found"
        cards[f"sig-{s}"] = {"short": " ".join(sigs[f"sig-{s}"]["mission_statement"].split()),
                             "use": capped(use), "not": capped(out)}
        rows, seen = [], set()
        for line in open(data / f"{s}.jsonl"):
            r = json.loads(line)
            if r["bot"] or [l for l in r["labels"] if l.startswith("sig/")] != [f"sig/{s}"]:
                continue
            text = issue_text(r["title"], r["body"])
            if len(text) >= 60 and text not in seen:
                seen.add(text)
                rows.append(text)
        rng.shuffle(rows)
        pool[f"sig-{s}"], held[f"sig-{s}"] = rows[:25], rows[25:]
    return assemble(cards, pool, held, [], rng)


def ops(data):
    """The hand-written set from the first spike. Even rows calibrate and odd rows
    test, within each label, as run.py does. It has no example pool."""
    cards = {l.split("\t")[0]: {"short": l.rstrip("\n").split("\t", 1)[1], "use": [], "not": []}
             for l in open(data / "descriptions.tsv") if l.strip()}
    seen, calib, test = {}, [], []
    for line in open(data / "requests.tsv"):
        if line.strip():
            label, text = line.rstrip("\n").split("\t", 1)
            i = seen[label] = seen.get(label, -1) + 1
            (calib if i % 2 == 0 else test).append([label, text])
    return {"cards": cards, "examples": {}, "calib": calib, "test": test}


if __name__ == "__main__":
    ap = argparse.ArgumentParser()
    for name in ("clinc", "clinc-domains", "massive", "k8s", "ops", "cards", "out"):
        ap.add_argument(f"--{name}", type=pathlib.Path, required=True)
    a = ap.parse_args()
    cards = json.load(open(a.cards))
    out = {
        "k8s": k8s(a.k8s, random.Random(1)),
        "clinc": clinc(a.clinc, a.clinc_domains, cards["clinc"], random.Random(2)),
        "massive": massive(a.massive, cards["massive"], random.Random(3)),
        "ops": ops(a.ops),
    }
    json.dump(out, open(a.out, "w"))
    for name, d in out.items():
        none = sum(l == "none" for l, _ in d["test"])
        print(f"{name}: {len(d['cards'])} agents, calib {len(d['calib'])}, test {len(d['test'])} ({none} none), "
              f"examples/agent min {min(map(len, d['examples'].values()), default=0)}")
