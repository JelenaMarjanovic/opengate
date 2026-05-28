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

# Matches each labeled field within a story body. The pattern uses a lookahead
# to stop at the next labeled field or at the trailing horizontal rule. This
# is the key to robust extraction because story field contents can be multi-
# paragraph (especially Description and Acceptance Criteria).
FIELD_RE = re.compile(
    r"\*(Format|Description|Acceptance Criteria|Story Points|Dependencies|Technical Notes|INVEST):\*"
    r"\s*(.+?)"
    r"(?=\n\n\*[A-Z]|\n\n---|\n\*\*US-)",
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
        label = match.group(1).lower().replace(" ", "_")
        # Acceptance Criteria becomes acceptance_criteria; others are
        # single-word labels and unchanged by the replace.
        value = match.group(2).strip()
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
    """
    epic_positions = []
    for em in EPIC_HEADER_RE.finditer(plan_text):
        epic_positions.append((em.start(), em.group(1), em.group(2).strip()))

    sprint_map = parse_sprint_assignments(plan_text)

    stories = []
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
    return stories


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

    stories = extract_stories(plan_text)
    epics = parse_epics(plan_text)

    write_stories_csv(stories, output_dir / "opengate-stories.csv")
    write_epics_csv(epics, output_dir / "opengate-epics.csv")

    # Sanity check output: each story has a sprint and an epic, totals match.
    missing_sprints = [s["id"] for s in stories if not s["sprint"]]
    missing_epics = [s["id"] for s in stories if s["epic_id"] == "?"]
    total_points = sum(s["story_points"] for s in stories)
    epic_points_sum = sum(e["total_story_points"] for e in epics)

    print(f"Parsed {len(stories)} stories across {len(epics)} epics.")
    print(f"Total story points (stories): {total_points}")
    print(f"Total story points (epic totals): {epic_points_sum}")
    if missing_sprints:
        print(f"WARNING: {len(missing_sprints)} stories without sprint assignment:")
        for sid in missing_sprints:
            print(f"  - {sid}")
    if missing_epics:
        print(f"WARNING: {len(missing_epics)} stories without epic mapping:")
        for sid in missing_epics:
            print(f"  - {sid}")


if __name__ == "__main__":
    main()
