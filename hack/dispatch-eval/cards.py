#!/usr/bin/env python3
"""Rebuilds agents.json from the public sample Agents in config/samples."""
import glob, json, re, pathlib

root = pathlib.Path(__file__).resolve().parents[2]
wanted = {line.split("\t")[0] for line in open(pathlib.Path(__file__).with_name("descriptions.tsv"))}
agents = {}
for f in sorted(glob.glob(str(root / "config/samples/**/*.yaml"), recursive=True), key=lambda p: ("templates" not in p, p)):
    for doc in re.split(r"^---\s*$", open(f).read(), flags=re.M):
        if not re.search(r"^kind: Agent\s*$", doc, re.M):
            continue
        name = re.search(r"^  name: (\S+)", doc, re.M).group(1)
        block = re.search(r"^  systemPrompt: [|>]-?\n((?:(?:    .*)?\n)+)", doc, re.M)
        if name in wanted and name not in agents and block:
            agents[name] = " ".join(block.group(1).split())
missing = wanted - set(agents)
assert not missing, f"no system prompt found for {missing}"
json.dump(dict(sorted(agents.items())), open(pathlib.Path(__file__).with_name("agents.json"), "w"), indent=1)
print(len(agents), "agents")
