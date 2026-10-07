#!/usr/bin/env python3
"""Prepare and verify the versioned files included in a release commit."""

import argparse
import datetime
import json
import os
import re
import sys
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
IMAGE = "ghcr.io/open-mcp-ai/termcp"
VERSION = re.compile(r"v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\Z")
UNRELEASED = re.compile(r"^## Unreleased\s*$", re.MULTILINE)
RELEASE = re.compile(r"^## v(\d+\.\d+\.\d+) — (\d{4}-\d{2}-\d{2})\s*$", re.MULTILINE)
RELEASE_BRANCH = re.compile(r"^release/v(\d+\.\d+\.\d+)$")
RELEASE_TITLE = re.compile(r"^release: v(\d+\.\d+\.\d+)$", re.IGNORECASE)


def normalized_version(value):
    match = VERSION.fullmatch(value)
    if not match:
        raise ValueError(f"invalid release version {value!r}; use vMAJOR.MINOR.PATCH")
    return ".".join(match.groups())


def changelog_parts(text):
    unreleased = UNRELEASED.search(text)
    release = RELEASE.search(text)
    if not release or (unreleased and unreleased.end() >= release.start()):
        raise ValueError("CHANGELOG.md needs a dated release heading after any Unreleased section")
    next_heading = re.search(r"^## ", text[release.end():], re.MULTILINE)
    release_end = release.end() + next_heading.start() if next_heading else len(text)
    unreleased_body = text[unreleased.end():release.start()] if unreleased else ""
    return unreleased, release, unreleased_body, text[release.end():release_end]


def validate(text, manifest, expected=None):
    _, release, unreleased_body, release_body = changelog_parts(text)
    version = release.group(1)
    try:
        datetime.date.fromisoformat(release.group(2))
    except ValueError as exc:
        raise ValueError(f"CHANGELOG.md v{version} has an invalid date") from exc
    if manifest.get("version") != version:
        raise ValueError(f"server.json version {manifest.get('version')!r} != changelog v{version}")
    packages = manifest.get("packages")
    if not isinstance(packages, list) or not packages:
        raise ValueError("server.json needs at least one package")
    identifier = packages[0].get("identifier") if isinstance(packages[0], dict) else None
    if identifier != f"{IMAGE}:{version}":
        raise ValueError(f"server.json package identifier {identifier!r} != {IMAGE}:{version}")
    if not re.search(r"^\s*-\s+\S", release_body, re.MULTILINE):
        raise ValueError(f"CHANGELOG.md v{version} needs at least one release note")
    if expected is not None:
        expected = normalized_version(expected)
        if version != expected:
            raise ValueError(f"release v{expected} requested, but files describe v{version}")
        if unreleased_body.strip():
            raise ValueError("move Unreleased notes into the release section before tagging")
    return version


def expected_from_pr():
    branch = os.environ.get("RELEASE_PR_HEAD_REF", "")
    title = os.environ.get("RELEASE_PR_TITLE", "")
    branch_match = RELEASE_BRANCH.fullmatch(branch)
    title_match = RELEASE_TITLE.fullmatch(title)
    if branch.startswith("release/") and not branch_match:
        raise ValueError(f"release branch {branch!r} must be release/vMAJOR.MINOR.PATCH")
    if title.lower().startswith("release:") and not title_match:
        raise ValueError(f"release PR title {title!r} must be 'release: vMAJOR.MINOR.PATCH'")
    branch_version = branch_match.group(1) if branch_match else None
    title_version = title_match.group(1) if title_match else None
    if branch_version and title_version and branch_version != title_version:
        raise ValueError(f"release PR branch v{branch_version} and title v{title_version} disagree")
    return branch_version or title_version


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=ROOT, help=argparse.SUPPRESS)
    commands = parser.add_subparsers(dest="command", required=True)
    check = commands.add_parser("check", help="verify server.json and CHANGELOG.md")
    check.add_argument("--expected", help="release tag, e.g. v0.2.5")
    prepare = commands.add_parser("prepare", help="move Unreleased notes into a dated release")
    prepare.add_argument("version", help="release tag, e.g. v0.2.5")
    prepare.add_argument("--date", default=datetime.datetime.now(datetime.timezone.utc).date().isoformat())
    args = parser.parse_args()

    changelog_path = args.root / "CHANGELOG.md"
    manifest_path = args.root / "server.json"
    text = changelog_path.read_text(encoding="utf-8")
    manifest = json.loads(manifest_path.read_text(encoding="utf-8"))

    if args.command == "prepare":
        version = normalized_version(args.version)
        try:
            datetime.date.fromisoformat(args.date)
        except ValueError as exc:
            raise ValueError("--date must be YYYY-MM-DD") from exc
        unreleased, release, unreleased_body, _ = changelog_parts(text)
        if unreleased is None:
            raise ValueError("add an Unreleased section with notes before preparing a release")
        if not re.search(r"^\s*-\s+\S", unreleased_body, re.MULTILINE):
            raise ValueError("Unreleased has no notes to move into the new release")
        current = tuple(map(int, release.group(1).split(".")))
        if tuple(map(int, version.split("."))) <= current:
            raise ValueError(f"v{version} must be newer than v{release.group(1)}")
        text = (
            text[:unreleased.end()]
            + f"\n\n## v{version} — {args.date}\n\n"
            + unreleased_body.strip()
            + "\n\n"
            + text[release.start():]
        )
        manifest["version"] = version
        manifest["packages"][0]["identifier"] = f"{IMAGE}:{version}"
        validate(text, manifest, version)
        changelog_path.write_text(text, encoding="utf-8")
        manifest_path.write_text(json.dumps(manifest, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
        print(f"prepared v{version}; review the release notes before merging")
        return

    expected = args.expected or expected_from_pr()
    version = validate(text, manifest, expected)
    print(f"release metadata consistent: v{version}" + (" (release ready)" if expected else ""))


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, KeyError, json.JSONDecodeError) as exc:
        print(f"release metadata: {exc}", file=sys.stderr)
        sys.exit(1)
