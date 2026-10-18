#!/usr/bin/env python3
"""Generate the next release section of CHANGELOG.md.

The section is the text in "## [Unreleased]" of CHANGELOG.md (written by hand
before the changelog was generated; normally empty), plus one line per
squash-merged commit since the last tag (git-cliff groups them, see
cliff.toml), plus the notes in changes/*.md. A commit line is dropped when the
Unreleased text already names its pull request.

  changelog.py preview          print the section for the commits since the last tag
  changelog.py release VERSION  write it into CHANGELOG.md as VERSION, delete changes/*.md
  changelog.py notes TAG        print the section of TAG (for the GitHub release body)

Set GIT_CLIFF to the git-cliff command (default: git-cliff).
"""

import datetime
import json
import os
import pathlib
import re
import shlex
import subprocess
import sys

REPO_URL = "https://github.com/telekom/BOOTy"
CHANGELOG = pathlib.Path("CHANGELOG.md")
CHANGES = pathlib.Path("changes")
# Section order. A section not in this list stays after the section before it.
ORDER = [
    "Highlights",
    "Breaking changes",
    "Behaviour changes in all modes",
    "Features",
    "Security",
    "Fixes",
    "Performance",
    "CI, build and tests",
    "Docs",
    "Dependencies",
    "Maintenance",
    "Reverts",
    "Upgrade notes for CAPRF",
]
# PR references in the hand-written text: "#123" or a ".../pull/123" link.
PR_REF = re.compile(r"(?:#|/pull/)(\d+)\b")
# The PR number that GitHub appends to a squash-merge title.
TITLE_PR = re.compile(r"\(#(\d+)\)$")


def git(*args):
    return subprocess.run(["git", *args], check=True, capture_output=True, text=True).stdout.strip()


def last_tag(ref):
    return git("describe", "--tags", "--abbrev=0", "--match", "v[0-9]*", ref)


def reverted(rng):
    """Commits in rng that a later commit in rng reverts, plus those reverts."""
    shas = []
    for commit in git("log", "--format=%H%n%B%x00", rng).split("\0"):
        sha, _, body = commit.strip().partition("\n")
        m = re.search(r"This reverts commit ([0-9a-f]{40})", body)
        if m:
            shas += [sha, m.group(1)]
    return shas


def commit_entries(rng):
    """(section, line, PR number or None) for each commit in rng, oldest first."""
    cmd = shlex.split(os.environ.get("GIT_CLIFF", "git-cliff")) + ["--context", rng]
    for sha in reverted(rng):
        cmd += ["--skip-commit", sha]
    out = subprocess.run(cmd, check=True, capture_output=True, text=True).stdout
    entries = []
    for release in json.loads(out):
        for c in release["commits"]:
            group = "Breaking changes" if c.get("breaking") else c["group"]
            message = c["message"].splitlines()[0].strip()
            pr = TITLE_PR.search(message)
            entries.append((group, entry_line(c.get("scope"), message), pr and pr.group(1)))
    return entries


def entry_line(scope, message):
    message = TITLE_PR.sub( rf"([#\1]({REPO_URL}/pull/\1))", message)
    if scope and not scope.startswith("deps"):
        message = f"{scope}: {message}"
    return f"- {message}"


def read_fragments(directory=CHANGES):
    """(section, text) for each changes/*.md except README.md."""
    frags = []
    for path in sorted(directory.glob("*.md")):
        if path.name == "README.md":
            continue
        m = re.match(r"---\n(.*?)\n---\n(.*)", path.read_text(), re.S)
        section = re.search(r"^section:\s*(.+?)\s*$", m.group(1), re.M) if m else None
        if not m or not section or not m.group(2).strip():
            sys.exit(f"{path}: needs front matter with 'section: <heading>' and a text")
        frags.append((section.group(1).strip("\"'"), m.group(2).strip()))
    return frags


def split_sections(body):
    """Split markdown into the text before the first ### and [heading, text] pairs."""
    parts = re.split(r"^### (.+)\n", body, flags=re.M)
    return parts[0].strip(), [[h.strip(), t.strip()] for h, t in zip(parts[1::2], parts[2::2])]


def render(frozen, entries, fragments):
    """Merge commit lines and fragments into the frozen Unreleased text."""
    intro, sections = split_sections(frozen)
    known = set(PR_REF.findall(frozen))
    added = [(s, line) for s, line, pr in entries if pr not in known]
    for i, (heading, text) in enumerate(added + fragments):
        for sec in sections:
            if sec[0] == heading:
                sec[1] = f"{sec[1]}\n{text}".strip()
                break
        else:
            if i >= len(added) and heading not in ORDER:
                print(f"warning: changes/ fragment makes a new section {heading!r}", file=sys.stderr)
            sections.append([heading, text])
    key, keys = -1, []
    for heading, _ in sections:
        key = ORDER.index(heading) if heading in ORDER else key
        keys.append(key)
    ordered = [s for _, s in sorted(zip(keys, sections), key=lambda p: p[0])]
    out = [intro] if intro else []
    out += [f"### {h}\n\n{t}" for h, t in ordered]
    return "\n\n".join(out) or "No changes."


def find_section(text, version):
    """(start, end) of the body of "## [version]" in text, or None."""
    m = re.search(rf"^## \[{re.escape(version)}\][^\n]*\n", text, re.M)
    if not m:
        return None
    nxt = re.search(r"^## \[", text[m.end():], re.M)
    end = m.end() + nxt.start() if nxt else len(text)
    return m.end(), end


def unreleased(text):
    """(start, end) of the body of "## [Unreleased]"; exits if there is none."""
    span = find_section(text, "Unreleased")
    if span is None:
        sys.exit("CHANGELOG.md has no '## [Unreleased]' section")
    return span


def generate(rng):
    text = CHANGELOG.read_text()
    start, end = unreleased(text)
    return render(text[start:end].strip(), commit_entries(rng), read_fragments())


def release(version, body, text, today):
    """CHANGELOG text with body as section version and an empty Unreleased."""
    if find_section(text, version):
        sys.exit(f"CHANGELOG.md already has a '## [{version}]' section")
    start, end = unreleased(text)
    text = text[:start] + f"\n## [{version}] - {today}\n\n{body}\n\n" + text[end:]
    return re.sub(
        r"^\[Unreleased\]: .*$",
        f"[Unreleased]: {REPO_URL}/compare/v{version}...HEAD\n"
        f"[{version}]: {REPO_URL}/releases/tag/v{version}",
        text,
        count=1,
        flags=re.M,
    )


def main(argv):
    cmd = argv[1] if len(argv) > 1 else "preview"
    if cmd == "preview":
        print(generate(f"{last_tag('HEAD')}..HEAD"))
    elif cmd == "release" and len(argv) == 3:
        version = argv[2].removeprefix("v")
        body = generate(f"{last_tag('HEAD')}..HEAD")
        today = datetime.datetime.now(datetime.timezone.utc).date().isoformat()
        CHANGELOG.write_text(release(version, body, CHANGELOG.read_text(), today))
        for path in CHANGES.glob("*.md"):
            if path.name != "README.md":
                path.unlink()
    elif cmd == "notes" and len(argv) == 3:
        tag = argv[2]
        text = CHANGELOG.read_text()
        span = find_section(text, tag.removeprefix("v"))
        # A tag cut without the release commit gets its section generated.
        print(text[span[0]:span[1]].strip() if span else generate(f"{last_tag(tag + '^')}..{tag}"))
    else:
        sys.exit(__doc__)


if __name__ == "__main__":
    main(sys.argv)
