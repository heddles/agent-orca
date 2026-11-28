# Routing benchmark, second round

The first spike used 115 requests written by one person. This round adds
requests that other people wrote and labelled, and measures against a 50 ms
target.

## Test sets

| set | agents | labels come from | "no agent fits" rows | card text |
|---|---|---|---|---|
| k8s | 10 Kubernetes SIGs | contributors triaging real issues | none | SIG mission statement; "use when" and "not for" are the charter's "in scope" and "out of scope" |
| clinc ([CLINC150](https://huggingface.co/datasets/clinc/clinc_oos), CC BY 3.0) | 10 domains | the dataset (crowdsourced requests) | its out-of-scope class | `cards.json`, written from the intent names |
| massive ([MASSIVE](https://huggingface.co/datasets/AmazonScience/massive) en-US, CC BY 4.0) | 17 scenarios | the dataset (human-written requests) | its `general` catch-all | `cards.json`, written from the scenario names |
| ops | 16 sample agents | us | ours | the first spike's one-line descriptions |

Calibration, test and example rows never overlap. Test sizes: k8s 736, clinc
1000, massive 912, ops 53. The hosted judge saw a subset: 260 labelled and, for
clinc and massive, 60 "none" test rows.

## Results

**Speed** (ONNX Runtime, 2 CPUs of an Apple M5 Pro in Docker, one request at a
time, tokenizer included; p50 ms):

| model | ~10-token request | ~270 tokens, uncut | cut to 128 | cut to 64 | RSS |
|---|---|---|---|---|---|
| gte-small | 3 | 71 | 31 | 15 | 335 MiB |
| bge-small | 5 | 73 | 31 | 18 | 336 MiB |
| gte-modernbert int8 | 5 | 104 | 49 | 25 | 392 MiB |
| EmbeddingGemma q4 | 9 | 138 | 60 | 29 | 551 MiB |
| Qwen3-0.6B Q8 (llama.cpp) | 47 | 452 | not run | not run | 1.4 GiB |
| e5-large / gte-large | 45 / 46 | 857 / 1092 | not run | not run | 2.1 GiB |

Server CPUs are often slower than this one; see "Runtime" for a second machine.

Cutting a request lowered k8s top-1 by 2–4 points at 128 tokens and 4–5 at 64
(gte-small and EmbeddingGemma, 20 examples). The short sets did not change.

**Examples** are the largest gain. gte-small, top-1 (k8s / clinc / massive):

| agent card | top-1 |
|---|---|
| name and one line | 37 / 55 / 72 |
| each "use when" line embedded on its own, best one counts | 50 / 81 / 78 |
| "use when" lines + 5 examples, logistic regression | 64 / 77 / 85 |
| "use when" lines + 20 examples, logistic regression | 76 / 84 / 89 |

With 20 examples, other small models were within a few points of each other:
EmbeddingGemma q4 was highest (80 / 92 / 94), gte-modernbert int8 scored
75 / 87 / 89. EmbeddingGemma also scored 94 on massive with names alone but 28
on k8s, so it may have seen massive in training (not verified). Do not rank it
by massive.

**Idea 2, richer cards.** Mean top-1 over 13 models and k8s, clinc and massive:

| format | top-1 |
|---|---|
| one line | 54.1% |
| one line + "use when", one embedding | 66.3% |
| + "not for" in the same text | 63.7% |
| "use when" lines embedded separately | 68.6% |
| + "not for" as a penalty, weight tuned on calibration (mostly 0 or 0.1) | 68.6% |
| + "not for" penalty fixed at 0.5 | 59.8% |

(The mean includes the broken Qwen3 export described below.) For Haiku 5.5 as
judge, "use when" raised clinc top-1 from 82% to 96% (36 requests fixed, none
broken, of 260) and made no measurable difference on k8s or massive. "Not for"
made none on any set. The clinc "use when" lines were written from the intent
names, so they cover its classes more completely than a tenant's would.

**Idea 1, fit categories.** Haiku 5.5 also labelled each pick `supported`,
`uncertain` or `unsupported`. Ranking by category and then confidence never
beat thresholding confidence alone: at the 90% and 95% targets the two chose
the same set in all but two of 14 cases, which differed by a few requests.
Accepting only `supported` needs no calibration and gave 89.7–100% precision on
all seven fit-format runs. A fixed confidence cutoff of 0.8 did about the same
(90–100%), though 0.8 was picked from three cutoffs after seeing test results.
The categories do carry signal (`supported` 90–100% right, `uncertain`
51–61% where there were enough to count, ops excepted, `unsupported` never
right), but confidence already carries it.

**Cascade** (local first, then the judge with "use when" cards if unsure, else
the default agent). gte-small, 20 examples, local threshold at a 95% target:

| set | local alone: routed at precision | asked the judge | cascade | judge for everything |
|---|---|---|---|---|
| k8s | 37% at 94% | 63% | 87% at 88% | 85% at 90% |
| clinc | 45% at 95% | 62% | 94% at 94% | 91% at 96% |
| massive | 76% at 93% | 35% | 91% at 93% | 80% at 95% |

With 5 examples the local router alone accepted 1% (k8s), 31% (clinc) and 64%
(massive) at that target. The judge took about 0.9 s (p50) and 1.3–1.7 s (p95).

**No calibration traffic** (`cold_start.py`: temperature and threshold from the
tenant's own examples, each held out in turn). Precision then misses its target
by an amount that depends on the set: a 95% target gave 83–98%, and a 90% target
78–97%. Rejecting "none" requests got worse with more examples: at the 90%
target with 20 examples on massive, only 38% (gte-small), 27% (gte-modernbert)
or 17% (EmbeddingGemma) were rejected. With 5 examples nothing could be
accepted on k8s.

## Runtime

llama.cpp reproduces gte-small. Against the ONNX fp32 vectors of all 5,468
texts, with requests cut to 128 tokens by tokenizing first and sending token ids
(`check_runtime.py`):

| build | cosine: min / 1st percentile / median | long request, 2 M5 cores | RSS |
|---|---|---|---|
| `ggml-org/gte-small-Q8_0-GGUF` | 0.99455 / 0.99980 / 0.99993 | 16 ms | 124 MiB |
| fp32 GGUF | 0.99469 / 1.00000 / 1.00000 | 50 ms | 193 MiB |

Routing results with the Q8 build were within 1 point of ONNX (ONNX fp32 on the
same cut took 31 ms).

**The same Q8 build on an Intel i7-6700HQ (2015, AVX2), in a Kubernetes pod**,
tokenize and embed, p50 / p95:

| request | 2 CPU limit | 4 CPU limit |
|---|---|---|
| about 10 tokens | 13.5 / 19 ms | 10 / 14 ms |
| long, cut to 64 tokens | 45 / 55 ms | 25 / 37 ms |
| long, cut to 128 | 85 / 99 ms | 47 / 70 ms |
| long, uncut (about 285) | 225 / 338 ms | 132 / 214 ms |

So orca-decide cuts requests to 64 tokens by default.

## orca-decide's settings (`defaults.py`)

Requests have no calibration traffic to fit against, so the temperature and
threshold are fixed: temperature 0.02 when matching descriptions, 0.5 for the
classifier, accept at confidence 0.8 or more. Test rows, routed % at precision %:

| examples per agent | k8s | clinc | massive |
|---|---|---|---|
| none (descriptions) | 0 | 26 at 97 | 31 at 97 |
| 5 | 19 at 93 | 24 at 97 | 38 at 99 |
| 10 | 29 at 92 | 37 at 94 | 55 at 98 |
| 20 | 40 at 93 | 54 at 93 | 72 at 95 |

These three values were chosen after seeing test rows, from a small grid near
the temperatures fitted on calibration rows, so treat the precision as
optimistic.

**Unequal examples matter.** When half the agents had examples and half had
none, a classifier stopped picking the agents without them (top-1 for those fell
from 78% to 4% on clinc with 20 examples), and overall top-1 fell from 81% to
49%. Weighting each agent equally helped but did not fix it. With at least 5
examples on every agent, however unequal, the weighted classifier scored 61 / 79
/ 87 against 49 / 81 / 79 for descriptions (5 and 20 examples; k8s / clinc /
massive), and with at least 10 it matched or beat them on all three. So
orca-decide uses examples only when every candidate has at least 5, and weights
each candidate equally.

## The service end to end (`replay.py`)

The test rows sent to orca-decide, with llama.cpp beside it (read-only root,
384 MiB limit each, 2 + 1 M5 cores). With requests cut to 128 tokens, every
"routed at precision" figure for 0, 5 and 20 examples equalled the offline table
above. At the default of 64 only k8s changed:

| examples per agent | k8s | clinc | massive |
|---|---|---|---|
| none | 1 at 100 | 26 at 97 | 31 at 97 |
| 5 | 17 at 90 | 24 at 97 | 38 at 99 |
| 20 | 39 at 95 | 54 at 93 | 72 at 95 |

Together the two containers used about 110 MiB. Preparing a new set of agents
took 0.1 to 0.9 s for the short sets and up to 9.7 s for 10 k8s agents with 20
examples each of up to 1200 bytes, longer than the client's 3 s timeout.
Preparation carries on after a caller gives up. The card and example limits are
the service's own, so the replay trims the k8s texts to them.

## Limits

- No public set of real requests labelled by agent was found. The closest, a
  [WildChat routing benchmark](https://arxiv.org/html/2606.28925), has labels
  made by keyword rules, so it was not used.
- k8s issues are long bug reports, not chat requests. Their team label is the
  triagers' call; issues with two team labels were left out.
- CLINC150 and MASSIVE are voice-assistant requests. Both are widely published,
  so any model may have trained on them.
- Thresholds come from a few hundred calibration rows, so precision on test
  rows lands a few points either side of the target. Differences of 1–3 points
  are noise.
- One hosted judge. Its confidence behaviour may not carry to other models.
- The int8 ONNX export of Qwen3-0.6B (onnx-community) did not reproduce
  llama.cpp (55% against 75% on the same ops rows). Use the llama.cpp numbers.
  EmbeddingGemma q4 matched its full-precision export within a few points.
- The "not for" result covers embedding similarity and one judge. It does not
  cover a model trained on negative examples.

## Rerunning

Scripts run in a container built from `python:3.12-slim` with `onnxruntime`,
`tokenizers`, `numpy`, `pyarrow`, `scikit-learn` and `pyyaml`. Keep downloads in
their own directories, mount them read-only, and run with `python -I`.

| step | script |
|---|---|
| build the four sets | `prepare.py` |
| vectors and timings (`--max-tokens` to cut requests) | `embed.py`, `embed_llamacpp.py` |
| local methods | `score.py` |
| hosted judge (needs `OPENROUTER_API_KEY`; the full run cost $1.87) and its analysis | `judge.py`, `score_judge.py` |
| cascade, cold start | `cascade.py`, `cold_start.py` |
| llama.cpp fidelity, fixed settings | `check_runtime.py`, `defaults.py` |
| the running service | `replay.py` |

The Kubernetes issues were fetched from the GitHub search API (single `sig/`
label, created after 2022-01-01, flaky and failing-test reports excluded, newest
first, up to 300 per SIG), and charters from `kubernetes/community` at
`f9a22285`. Model revisions are the ones pinned in the table below.

| name | repository | revision |
|---|---|---|
| gte-small | thenlper/gte-small | 17e1f347 |
| bge-small | BAAI/bge-small-en-v1.5 | 5c38ec7c |
| e5-small / base / large | intfloat/e5-*-v2 | ffb93f3b / f52bf8ec / f169b11e |
| bge-base | BAAI/bge-base-en-v1.5 | a5beb1e3 |
| gte-modernbert, gte-large | Alibaba-NLP/gte-*  | e7f32e3c / 64398e60 |
| EmbeddingGemma | onnx-community/embeddinggemma-300m-ONNX | 5090578d |
| Qwen3 ONNX | onnx-community/Qwen3-Embedding-0.6B-ONNX | c25a394d |
| potion | minishlab/potion-base-32M | 1e5a03f8 |
| gte-small GGUF, Q8 | ggml-org/gte-small-Q8_0-GGUF | 1dc93210 |
| gte-small GGUF, fp32 | ChristianAzinn/gte-small-gguf | 240acca7 |
