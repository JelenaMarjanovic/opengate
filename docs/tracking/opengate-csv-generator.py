"""
OpenGate Implementation Plan → CSV converter.

Parses the Implementation Plan markdown document and produces two CSV files:
  1. opengate-stories.csv  - one row per user story with all metadata
  2. opengate-epics.csv    - one row per epic with goal and totals

The CSV format is intentionally universal: each row uses double-quoted fields
with embedded newlines preserved. Most backlog tools (ClickUp, Linear, Plane,
Notion) accept this format via their respective CSV import flows.

Usage:
    python3 generate_csv.py <implementation_plan.md> <output_dir>
"""

import csv
import re
import sys
from pathlib import Path


# ---------------------------------------------------------------------------
# Regular expressions
# ---------------------------------------------------------------------------

# Matches the header of each user story. The story ID is captured along with
# the title. Example header line:
#     **US-01.01: Initialize Go module and hexagonal project layout**
STORY_HEADER_RE = re.compile(r"^\*\*(US-\d{2}\.\d{2}): (.+?)\*\*$", re.MULTILINE)

# Matches an epic section header. The epic number and the title after the
# colon are captured. Example:
#     ## 8. Epic E1: Project Bootstrap and Foundations
EPIC_HEADER_RE = re.compile(
    r"^## \d+\. Epic (E\d+): (.+?)$", re.MULTILINE
)

# The labels that introduce each field inside a story body. Kept as a single
# constant because the field pattern needs them twice: once to match a field,
# and once in the lookahead that decides where that field's value ends.
FIELD_LABELS = (
    "Format|Description|Acceptance Criteria|Story Points|Dependencies|"
    "Technical Notes|INVEST"
)

# Matches each labeled field within a story body. The pattern uses a lookahead
# to stop at the next labeled field or at the trailing horizontal rule. This
# is the key to robust extraction because story field contents can be multi-
# paragraph (especially Description and Acceptance Criteria).
#
# The emphasis delimiter is captured rather than hard-coded: the plan writes
# fields as `_Description:_`, but earlier revisions used `*Description:*`, and
# both are valid Markdown emphasis. The `(?P=em)` backreference requires the
# closing delimiter to match the opening one, so a stray `_Description:*` is
# not accepted. Hard-coding one form is what silently emptied every content
# column when the corpus was reformatted: the story headers use `**` and kept
# parsing, so the generator still exited 0 while producing hollow rows.
#
# The lookahead names the labels explicitly instead of accepting any emphasized
# capital letter. Field values contain emphasized prose of their own, and a
# looser lookahead would truncate a Description at its first italicized word.
FIELD_RE = re.compile(
    r"(?P<em>[*_])(?P<label>" + FIELD_LABELS + r"):(?P=em)"
    r"\s*(?P<value>.+?)"
    r"(?=\n\n[*_](?:" + FIELD_LABELS + r"):|\n\n---|\n\*\*US-|\Z)",
    re.DOTALL,
)

# Matches the sprint plan table rows. Used to assign each story to its sprint.
# Example row:
#     | S1 | 1 | E1 | US-01.01, US-01.02, US-01.03, US-01.04 | 12 |
SPRINT_ROW_RE = re.compile(
    r"^\|\s*(S\d+)\s*\|\s*\d+\s*\|\s*[^|]*\|\s*(.+?)\s*\|\s*\d+\s*\|$",
    re.MULTILINE,
)

# Matches a story ID anywhere in text. Used to extract dependencies and to
# expand the sprint plan's story lists (which use shorthand like
# "US-13.02 — US-13.07" for ranges).
STORY_ID_RE = re.compile(r"US-\d{2}\.\d{2}")


# ---------------------------------------------------------------------------
# Story extraction
# ---------------------------------------------------------------------------


def find_epic_for_offset(epic_headers, offset):
    """
    Given a story's character offset in the document and the list of epic
    header positions, return the (epic_id, epic_name) that owns the story.

    The function relies on the document being laid out so that each epic's
    stories appear after its header but before the next epic's header. This
    is true for the Implementation Plan as written.
    """
    current = None
    for epic_offset, epic_id, epic_name in epic_headers:
        if epic_offset < offset:
            current = (epic_id, epic_name)
        else:
            break
    return current if current else ("?", "Unknown")


def parse_story_body(body_text):
    """
    Extract the labeled fields from a single story's body text.

    Returns a dict with keys: format, description, acceptance_criteria,
    story_points, dependencies, technical_notes, invest. Missing fields map
    to empty strings.
    """
    fields = {}
    for match in FIELD_RE.finditer(body_text):
        label = match.group("label").lower().replace(" ", "_")
        # Acceptance Criteria becomes acceptance_criteria; others are
        # single-word labels and unchanged by the replace.
        value = match.group("value").strip()
        fields[label] = value
    return fields


def split_format_clause(format_text):
    """
    Split the "Format" field "As ROLE, I want CAPABILITY, so that BENEFIT."
    into its three constituent parts. Returns (role, want, so_that) tuples.

    The split tolerates minor variations such as "As a" vs "As an" vs "As the"
    and trailing punctuation.
    """
    # Strip the trailing period and any leading "As" prefix.
    text = format_text.strip().rstrip(".")
    # Pattern: "As <role>, I want <capability>, so that <benefit>"
    match = re.match(
        r"^As (?:a |an |the )?(.+?), I want (.+?), so that (.+)$",
        text,
        re.IGNORECASE,
    )
    if match:
        return match.group(1).strip(), match.group(2).strip(), match.group(3).strip()
    # Fall back to placing the whole string in the "want" slot if the pattern
    # does not match; this should not happen with a well-formed Implementation
    # Plan but is defensive against typos.
    return "", text, ""


def parse_dependencies(deps_text):
    """
    Parse the "Dependencies" field into a comma-separated list of story IDs.

    Handles the special values "none" and "(all other epics complete)" by
    returning an empty string and a sentinel marker respectively.
    """
    text = deps_text.strip().lower()
    if text == "none":
        return ""
    if "all other epics" in text:
        return "ALL_EPICS_COMPLETE"
    # Extract every story ID present in the dependencies text. This is more
    # robust than splitting on commas because of inconsistent formatting.
    ids = STORY_ID_RE.findall(deps_text)
    return ", ".join(ids)


def parse_story_points(points_text):
    """
    Extract the integer story point value, discarding any parenthetical
    annotation such as "(reference story)".
    """
    match = re.search(r"\d+", points_text)
    return int(match.group(0)) if match else 0


# ---------------------------------------------------------------------------
# Sprint mapping
# ---------------------------------------------------------------------------


def parse_sprint_assignments(plan_text):
    """
    Read the sprint plan table and produce a {story_id: sprint_id} mapping.

    The table cell can include ranges with em-dash shorthand like
    "US-13.02 — US-13.07", which this function expands by walking the numeric
    range of the suffix.
    """
    mapping = {}
    for match in SPRINT_ROW_RE.finditer(plan_text):
        sprint_id = match.group(1)
        cell = match.group(2)

        # First, handle em-dash ranges. Each range expands to every story ID
        # in the inclusive interval. The epic number is identical on both
        # endpoints; only the story suffix varies.
        range_pattern = re.compile(r"(US-\d{2})\.(\d{2})\s*[—-]\s*(US-\d{2})\.(\d{2})")
        cell_expanded = cell
        for r in range_pattern.finditer(cell):
            start_epic, start_num, end_epic, end_num = r.groups()
            if start_epic == end_epic:
                expanded = [
                    f"{start_epic}.{n:02d}"
                    for n in range(int(start_num), int(end_num) + 1)
                ]
                cell_expanded += " " + " ".join(expanded)

        for sid in STORY_ID_RE.findall(cell_expanded):
            mapping[sid] = sprint_id
    return mapping


# ---------------------------------------------------------------------------
# Epic extraction
# ---------------------------------------------------------------------------


def parse_epics(plan_text):
    """
    Extract the epic catalog as a list of dicts containing the epic ID,
    name, goal paragraph, business value paragraph, and the running point
    total displayed at the end of each epic section.
    """
    epics = []
    matches = list(EPIC_HEADER_RE.finditer(plan_text))
    for i, match in enumerate(matches):
        epic_id = match.group(1)
        epic_name = match.group(2).strip()

        # The epic section runs from the end of the header line until the
        # next epic header (or until the end of the file for the last epic).
        start = match.end()
        end = matches[i + 1].start() if i + 1 < len(matches) else len(plan_text)
        section = plan_text[start:end]

        # The goal is the paragraph following "### Epic goal".
        goal_match = re.search(
            r"### Epic goal\s*\n\n(.+?)(?=\n\n###|\n\n\*\*)", section, re.DOTALL
        )
        goal = goal_match.group(1).strip() if goal_match else ""

        # The business value follows "### Business value" similarly.
        value_match = re.search(
            r"### Business value\s*\n\n(.+?)(?=\n\n###|\n\n\*\*)", section, re.DOTALL
        )
        business_value = value_match.group(1).strip() if value_match else ""

        # The total points are reported in the form "**Epic E1 total: 17 story points.**"
        total_match = re.search(
            r"\*\*Epic E\d+ total: (\d+) story points\.\*\*", section
        )
        total_points = int(total_match.group(1)) if total_match else 0

        epics.append(
            {
                "id": epic_id,
                "name": epic_name,
                "goal": goal,
                "business_value": business_value,
                "total_story_points": total_points,
            }
        )
    return epics


# ---------------------------------------------------------------------------
# Main extraction pipeline
# ---------------------------------------------------------------------------


def extract_stories(plan_text):
    """
    Walk the document, find every story header, and build a list of fully
    populated story records ready for CSV emission.

    Returns (stories, raw_fields) where raw_fields maps a story ID to the
    unprocessed {label: value} dict that FIELD_RE produced for it. The raw
    dict is returned alongside rather than folded into the story record
    because validation needs to distinguish "the label was absent from the
    plan" from "the label was present and its value is legitimately empty".
    The Dependencies field is the case that forces the distinction: US-01.01
    reads `_Dependencies:_ none`, which is present in the plan but renders as
    an empty CSV column.
    """
    epic_positions = []
    for em in EPIC_HEADER_RE.finditer(plan_text):
        epic_positions.append((em.start(), em.group(1), em.group(2).strip()))

    sprint_map = parse_sprint_assignments(plan_text)

    stories = []
    raw_fields = {}
    story_matches = list(STORY_HEADER_RE.finditer(plan_text))
    for i, match in enumerate(story_matches):
        story_id = match.group(1)
        title = match.group(2).strip()
        offset = match.start()
        epic_id, epic_name = find_epic_for_offset(epic_positions, offset)

        # The story body runs from the end of its header line until the
        # next story header or until the end of the document.
        body_start = match.end()
        body_end = (
            story_matches[i + 1].start()
            if i + 1 < len(story_matches)
            else len(plan_text)
        )
        body = plan_text[body_start:body_end]

        fields = parse_story_body(body)
        raw_fields[story_id] = fields
        role, want, so_that = split_format_clause(fields.get("format", ""))

        stories.append(
            {
                "id": story_id,
                "title": title,
                "epic_id": epic_id,
                "epic_name": epic_name,
                "sprint": sprint_map.get(story_id, ""),
                "type": "Story",
                "status": "To Do",
                "priority": "Medium",
                "story_points": parse_story_points(fields.get("story_points", "0")),
                "role": role,
                "want": want,
                "so_that": so_that,
                "description": fields.get("description", ""),
                "acceptance_criteria": fields.get("acceptance_criteria", ""),
                "dependencies": parse_dependencies(fields.get("dependencies", "")),
                "technical_notes": fields.get("technical_notes", ""),
                "invest": fields.get("invest", ""),
                "labels": f"{epic_id},{sprint_map.get(story_id, '')}",
            }
        )
    return stories, raw_fields


# ---------------------------------------------------------------------------
# Validation
# ---------------------------------------------------------------------------

# The seven labels every story block in the plan carries. Absence of any one of
# them means the field pattern stopped matching the corpus, not that a story is
# genuinely incomplete — the plan's own review process does not let a story
# through without them.
REQUIRED_STORY_FIELDS = (
    "format",
    "description",
    "acceptance_criteria",
    "story_points",
    "dependencies",
    "technical_notes",
    "invest",
)


def validate(stories, raw_fields, epics):
    """
    Check the extraction for the signatures of a parser that has silently
    stopped matching the corpus. Returns a list of human-readable problems;
    an empty list means the extraction looks sound.

    This exists because the generator's failure mode is not a crash. When the
    plan was reformatted from `*Description:*` to `_Description:_`, the story
    headers and the sprint table kept parsing, so the script still reported
    "55 stories across 14 epics" and exited 0 while emitting rows whose every
    content column was empty.

    Why that has to be fatal rather than a warning: the drift guard's
    remediation path is "run `make tracking` and commit the result". Any
    failure mode in which regeneration produces garbage is therefore a failure
    mode the guard actively converts into a committed defect — the check goes
    red with a large diff, a reader concludes the corpus moved, regenerates,
    commits the hollow output, and the check goes green. The guard would not
    merely miss the fault; it would instruct someone to commit it. So the
    generator must refuse to emit an artifact it can tell is wrong.
    """
    problems = []

    # A corpus that yields nothing at all is the most basic form of the fault:
    # a renamed heading convention, or simply the wrong file passed in.
    if not stories:
        problems.append("no stories parsed - the story header pattern matched nothing")
    if not epics:
        problems.append("no epics parsed - the epic header pattern matched nothing")

    for story in stories:
        sid = story["id"]
        fields = raw_fields.get(sid, {})

        # Field-level emptiness is the general form of the fault. The totals
        # check below only catches it when the Story Points label is among the
        # casualties; descriptions going empty while points still parse would
        # slip past a totals check entirely.
        absent = [f for f in REQUIRED_STORY_FIELDS if not fields.get(f, "").strip()]
        if absent:
            problems.append(f"{sid}: no value parsed for {', '.join(absent)}")

        # Derived fields. These catch a Format clause that no longer matches
        # "As ROLE, I want CAPABILITY, so that BENEFIT" even though the label
        # itself was found, and a Story Points value carrying no digit.
        if not story["role"] or not story["so_that"]:
            problems.append(f"{sid}: Format clause did not split into role/want/benefit")
        if story["story_points"] <= 0:
            problems.append(f"{sid}: story points parsed as {story['story_points']}")
        if not story["sprint"]:
            problems.append(f"{sid}: no sprint assignment found in the sprint plan table")
        if story["epic_id"] == "?":
            problems.append(f"{sid}: could not be mapped to an epic")

    for epic in epics:
        if not epic["goal"]:
            problems.append(f"{epic['id']}: no epic goal parsed")
        if not epic["business_value"]:
            problems.append(f"{epic['id']}: no business value parsed")
        if epic["total_story_points"] <= 0:
            problems.append(f"{epic['id']}: epic total parsed as 0")

    # Cross-check the two independent readings of the same number: the sum of
    # the per-story estimates against the sum of the totals each epic section
    # states in prose. They are written by hand in different places, so an
    # agreement is meaningful evidence that both parsed correctly.
    story_total = sum(s["story_points"] for s in stories)
    epic_total = sum(e["total_story_points"] for e in epics)
    if story_total != epic_total:
        problems.append(
            f"story points sum to {story_total} but the epic totals sum to "
            f"{epic_total}; the two readings of the same number disagree"
        )

    return problems


def write_stories_csv(stories, path):
    """Write the stories list to CSV with universal quoting."""
    fieldnames = [
        "id",
        "title",
        "type",
        "epic_id",
        "epic_name",
        "sprint",
        "status",
        "priority",
        "story_points",
        "role",
        "want",
        "so_that",
        "description",
        "acceptance_criteria",
        "dependencies",
        "technical_notes",
        "invest",
        "labels",
    ]
    with open(path, "w", newline="", encoding="utf-8") as f:
        writer = csv.DictWriter(
            f, fieldnames=fieldnames, quoting=csv.QUOTE_ALL
        )
        writer.writeheader()
        for story in stories:
            writer.writerow(story)


def write_epics_csv(epics, path):
    """Write the epics list to CSV with universal quoting."""
    fieldnames = [
        "id",
        "name",
        "type",
        "status",
        "goal",
        "business_value",
        "total_story_points",
    ]
    with open(path, "w", newline="", encoding="utf-8") as f:
        writer = csv.DictWriter(
            f, fieldnames=fieldnames, quoting=csv.QUOTE_ALL
        )
        writer.writeheader()
        for epic in epics:
            row = {**epic, "type": "Epic", "status": "To Do"}
            writer.writerow(row)


def main():
    if len(sys.argv) != 3:
        print("Usage: python3 generate_csv.py <plan_path> <output_dir>", file=sys.stderr)
        sys.exit(1)

    plan_path = Path(sys.argv[1])
    output_dir = Path(sys.argv[2])
    output_dir.mkdir(parents=True, exist_ok=True)

    plan_text = plan_path.read_text(encoding="utf-8")

    stories, raw_fields = extract_stories(plan_text)
    epics = parse_epics(plan_text)

    # Validate BEFORE writing anything. Order matters: these CSVs are committed
    # and regenerated in place, so validating after the write would leave the
    # corrupt artifact on disk for someone to commit — which is exactly the
    # outcome the check exists to prevent. On failure the previously committed
    # files are left untouched and the caller sees a non-zero exit.
    problems = validate(stories, raw_fields, epics)
    if problems:
        print(
            f"ERROR: refusing to write CSVs; the extraction from {plan_path} "
            f"looks wrong ({len(problems)} problems).",
            file=sys.stderr,
        )
        print(
            "This usually means the plan's formatting changed and a pattern in "
            "this script no longer matches it. Fix the parser rather than "
            "committing the output.",
            file=sys.stderr,
        )
        for problem in problems:
            print(f"  - {problem}", file=sys.stderr)
        sys.exit(1)

    write_stories_csv(stories, output_dir / "opengate-stories.csv")
    write_epics_csv(epics, output_dir / "opengate-epics.csv")

    total_points = sum(s["story_points"] for s in stories)
    print(f"Parsed {len(stories)} stories across {len(epics)} epics.")
    print(f"Total story points: {total_points} (matches the sum of epic totals).")


if __name__ == "__main__":
    main()
