# Tracking artifacts

This directory holds two kinds of file: generated CSVs and hand-written sprint
retrospectives. Do not edit the CSVs by hand.

## `opengate-stories.csv` and `opengate-epics.csv` are import seeds

Both files are a **one-time import seed** derived from
[the implementation plan](../planning/opengate-implementation-plan-v1.md). They
exist to be imported into a tracker — ClickUp, Linear, Plane, or anything else
that accepts a quoted CSV — so the backlog does not have to be retyped.

**The `status` column is a generator default, not a progress record.** Every row
reads `To Do` regardless of what has shipped, because the planning corpus does
not carry per-story completion and the generator has nothing else to write
there. The same applies to `priority`, which is `Medium` on every row.

Reading either column as delivery state gives a wrong answer. It has already
happened once: during Sprint 5 the CSV was read as authoritative and produced a
wrong conclusion about epic completion.

**Delivery progress lives in two places, both hand-maintained:**

- the [root README](../../README.md), for current story counts and status;
- the sprint retrospectives in this directory, for what was delivered, what was
  cut, and why.

The statement above is in this README rather than as a comment line at the top
of the CSVs because CSV has no comment syntax. RFC 4180 defines none, and every
importer treats the first line as the header row — a leading `#` line is parsed
as the header, shifting every real column into the data and corrupting the
import.

## Regenerating

```
make tracking
```

This reruns `opengate-csv-generator.py` over the implementation plan and
rewrites both CSVs in place. Commit the result.

`make tracking-check` regenerates and then fails if the committed CSVs differ,
proving they still match the corpus. It runs in CI as the `tracking-drift` job,
separate from `make ci` so a Python failure cannot break build/lint/test.

The guard exists because the CSVs went stale through three reconciliations
(v1.1, v1.2, v1.3) while the generator sat in this directory, working, unrun.
Automation without a drift guard changes only how easy regeneration is, not
whether it happens.

## Sprint retrospectives

`opengate-sprint-N-retrospective-v1.md` are hand-written and are **not**
generated. This is why `make tracking-check` diffs the two CSVs by name instead
of diffing this directory: a directory-wide check would fail on every new
retrospective.
