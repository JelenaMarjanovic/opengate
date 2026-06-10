# OpenGate — Planning Corpus Reconciliation v1.3

**Document version:** 1.3 reconciliation changelog
**Date:** 2026-06-09
**Supersedes:** nothing; this is the authoritative record of the delta from the v1.2 corpus to v1.3. It does not revisit v1.1 or v1.2, whose corrections stand.
**Scope:** records the documented-vs-realized divergences surfaced during the Sprint 4 implementation (US-03.01 event-store tables and RLS, US-03.02 EventStore port and event types, US-03.03 PostgresEventStore adapter). One corpus edit (Database Schema §6.1); one realization recorded without an edit (System Design §2). The PRD, PFD, System Architecture, and Implementation Plan require no v1.3 change.

## How to apply

Each entry names a target document, the location, the exact text before, the exact text after, and a one-line rationale. The single edit is mechanical: locate the *Before* text and replace it with the *After* text.

## 0. Verification summary

Sprint 4 produced the fewest realization divergences of any sprint, consistent with US-03.01 and US-03.02 having no open architectural decisions — the corpus had already locked the event-store schema (Database Schema §6.1/§6.2/§13), the port signatures (System Design §2), and the type shapes. One genuine divergence reaches an authoritative document and is corrected here; one is in an explicitly-illustrative document and is recorded without an edit.

The single corpus edit is the §6.1 `stream_position` wording (C1): §6.1 — an authoritative document — describes the application reading `nextval()` in a separate query before the insert, whereas the realized `Append` (US-03.03) calls `nextval()` inline and returns positions via `RETURNING` in a single statement. The "not a column default" rationale §6.1 gives still holds; the "read before insert" mechanism does not. The recorded-without-edit item is the Model B sequence assignment (B1): System Design §2's `Event` struct presents `Sequence` as a field, and the realized contract is that `Append` assigns it from `expectedSequence`; §2 is explicitly illustrative ("subject to refinement during implementation"), so no §2 edit is made.

Two items that might have been expected here are not v1.3 items. The US-02.06 inline-dependency-field gap — US-03.01 and US-04.01 pointing at the absorbed US-02.06 in their inline fields while the matrix was correct — was a v1.1-application miss, corrected on `main` in `cc48c07` (PR #8) at the start of Sprint 4, recorded in the Sprint 4 retrospective, and not reopened here. The `events` role grants (US-03.03) require no schema-document change: the Database Schema has no role-grants section, so grants live only in the migrations, which are the source of truth.

---

## A. Implementation Plan (`opengate-implementation-plan-v1.md`)

No v1.3 change. The only Sprint-4 implementation-plan correction — the US-03.01 and US-04.01 inline dependency fields, stale at US-02.06 since the v1.1 application missed them while correcting the dependency matrix — was applied directly on `main` in `cc48c07` (PR #8) and is recorded in the Sprint 4 retrospective.

## B. System Design (`opengate-system-design-v1.md`)

### B1. §2 — Model B sequence assignment (recorded, no edit)

The §2 `Event` struct shows `Sequence` as a struct field, and the §2 prose speaks of the handler computing the expected next sequence. The realized contract (US-03.03) is **Model B**: `Append(ctx, aggregateID, expectedSequence, evts)` assigns each event's `sequence` as `expectedSequence + 1 + i`, so on the events passed to `Append` the `Sequence` field is not read — it is populated on read, parallel to `StreamPosition`. The US-03.02 `Sequence` doc comment was corrected to state this.

**No edit is made to §2.** §2 opens by declaring its `Event` struct "illustrative only and subject to refinement during implementation," so the struct does not mislead a reader, and the realized contract is recorded both in the code comment and here. Were §2 not self-flagged as illustrative, this would be a corpus edit on the order of C1.

Rationale: record the realized sequence-assignment contract for traceability without editing a document that already disclaims its own illustrative status.

## C. Database Schema (`opengate-database-schema-v1.md`)

### C1. §6.1 — events `stream_position` is obtained via `nextval()` inline and `RETURNING`, not a pre-read

§6.1 — an authoritative document — describes the application reading the sequence value in a separate query before the insert. The realized `Append` (US-03.03) calls `nextval()` inline within the single `INSERT … SELECT …, nextval('events_stream_position_seq'), … RETURNING stream_position` statement. The "independent of the table rather than a column default" rationale is unchanged; only the obtain-the-value mechanism is corrected.

**Before (the third sentence of the §6.1 `stream_position` paragraph):**
> The sequence is independent of the events table itself rather than declared as the column's default; the reason is that the application code reads the next sequence value before the insert (using `nextval()`) so that the value can be included in event metadata exposed to downstream consumers.

**After:**
> The sequence is independent of the events table itself rather than declared as the column's default; the append statement calls `nextval()` inline and obtains each assigned position from the same statement via `RETURNING`, rather than in a separate read before the insert. The position stays explicit — produced by the statement, not a column default — and the `RETURNING` clause is the seam through which a future change-notification path will expose positions to downstream consumers; the realized `EventStore.Append` contract returns only an error, so the value is not yet propagated to the caller.

Rationale: align an authoritative document with the realized single-statement append; the "read before insert" mechanism §6.1 described is not what the adapter does, and a forward implementer reading §6.1 would otherwise build the wrong shape.

---

## Closing

Once the single C1 edit is applied, the planning corpus is logically at v1.3. This changelog is the authoritative record of the delta from v1.2; the Database Schema carries the C1 correction, and B1 is recorded here without a corpus edit by design. All items are Sprint-4 realization divergences; the v1.1 and v1.2 changelogs and the per-sprint retrospectives remain authoritative for their respective deltas.
