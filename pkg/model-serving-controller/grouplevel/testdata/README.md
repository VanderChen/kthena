# Vectors of the group-level plan

`plan_vectors.jsonl.gz` is generated. Do not edit it by hand.

Each line is one call of `plan` of the reference model (the Python model `grouplevel.py`, final
rule set): the input in the shape of this package, the writes the model
returned (`acts`), and the classes and counts of the input (`view`). `plan_vectors_test.go`
replays every line through `Plan` and `Classify` and compares the result in full.

## How it was produced

The exporter and the model are not part of this repository. They live in the task workspace that
holds it:

- script: `issues/features/010-modelserving-servinggroup-cr-impl-*/evidence/model-vectors/export_vectors.py`
- model: `issues/features/010-modelserving-servinggroup-cr-impl-*/evidence/model-vectors/model-010/`
  (`grouplevel.py`, sha256 `06fd955b1bf0c8b034ab08752c7123a4b73247011f6f4537e322663fbcde6ed8`). It is the
  reviewed model of the design, `issues/features/008-modelserving-servinggroup-cr-design-IP/model/`
  (`grouplevel.py`, sha256 `04e9fc1436f151182ea8a49a8f13858737eebe1738fa4f70d6583261fe4e0d27`), with
  one rule changed after the conformance run: G3b reclaims a surge group only when it is surplus (N
  other live groups have their members) or when there is no room. `model-010.diff` next to it is the
  whole difference; `results-vs-008.md` is what the change does to the results of the model's
  drivers

```bash
cd issues/features/010-modelserving-servinggroup-cr-impl-*/evidence/model-vectors
uv run --no-project python export_vectors.py            # writes this file (about 18 minutes)
uv run --no-project python export_vectors.py --check    # compares the lines with this file
```

The script wraps `grouplevel.plan`, runs the drivers of the model and records every call made
with the final rules. Calls under any other rule set are skipped. Identical inputs are written
once. The mapping of the model's types to the types of this package is in the docstring of the
script.

| Source | What | Inputs | Kept here |
| --- | --- | --- | --- |
| `scenarios` | 43 scenarios of the contract, 58 added scenarios, 14 lookup rows, 5 stop points of runner cases | 504 | 504 |
| `runner` | 35 compound runner cases, every interleaving | 57 747 | 3 500 |
| `normal` | 68 cases of the `normal` runner suite, every interleaving | 23 187 | 3 500 |
| `group` | exhaustive explorer of the model | 31 509 | 3 500 |
| `reference` | random states of the model's own differential check | 199 814 | 2 500 |
| `fuzz` | random inputs made by the script for what the drivers rarely set (failed, not observed, not fresh, memberless, empty, `RecreateRequired`, ownerless Pods, foreign objects, `RoleRollingUpdate`, unknown baseline) | 5 811 | 4 000 |
| total | | 318 572 | 17 504 |

The file keeps every vector of `scenarios` and a sample with a fixed seed of the other sources,
so that it stays below 600 KB: 17 504 lines, 587 945 bytes, sha256 of the uncompressed lines
`91fbdb1f2b937b0fd0461a3d04bf19253f5936ce2fae082b2f48102283b7773a` (Python 3.12.16).

The Go tests were also run once on the whole corpus of 318 572 inputs (`export_vectors.py --all`).
The test reads every `testdata/plan_vectors*.jsonl.gz`, so a larger file can be dropped in next
to this one for such a run.
