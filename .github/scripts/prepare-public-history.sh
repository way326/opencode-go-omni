#!/usr/bin/env bash
set -euo pipefail

: "${OLD_EMAIL:?set OLD_EMAIL to the personal email being removed}"
: "${NOREPLY_EMAIL:?set NOREPLY_EMAIL to the GitHub-provided noreply address}"

if [[ "${PUBLIC_HISTORY_REWRITE_CONFIRM:-}" != "YES" ]]; then
  echo "run this only in a disposable mirror clone and set PUBLIC_HISTORY_REWRITE_CONFIRM=YES" >&2
  exit 1
fi

if ! command -v git-filter-repo >/dev/null 2>&1; then
  echo "git-filter-repo is required (version 2.47.0 or newer)" >&2
  exit 1
fi

python_bin=""
version=""
for candidate in python3 python; do
  if command -v "${candidate}" >/dev/null 2>&1; then
    candidate_version="$("${candidate}" -c 'from importlib.metadata import version; print(version("git-filter-repo"))' 2>/dev/null || true)"
    if [[ -n "${candidate_version}" ]]; then
      python_bin="${candidate}"
      version="${candidate_version}"
      break
    fi
  fi
done
if [[ -z "${version}" ]] || ! "${python_bin}" -c 'import sys; v=tuple(int(n) for n in sys.argv[1].split(".")); raise SystemExit(0 if v >= (2, 47, 0) else 1)' "${version}"; then
  echo "git-filter-repo version 2.47.0 or newer is required" >&2
  exit 1
fi
echo "git-filter-repo ${version}"

if [[ "$(git rev-parse --is-bare-repository)" != "true" && -n "$(git status --porcelain)" ]]; then
  echo "refusing to rewrite a dirty worktree" >&2
  exit 1
fi

current_remote="$(git remote get-url origin 2>/dev/null || true)"
if [[ -n "${current_remote}" ]]; then
  echo "origin=present (URL redacted)"
else
  echo "origin=none"
fi

mailmap="$(mktemp)"
cleanup() {
  rm -f "${mailmap}"
}
trap cleanup EXIT

printf 'way326 <%s> <%s>\n' "${NOREPLY_EMAIL}" "${OLD_EMAIL}" > "${mailmap}"

mapfile -t tags < <(git tag --list)
if (( ${#tags[@]} > 0 )); then
  git tag --delete "${tags[@]}" >/dev/null
fi

# The public repository does not carry internal audit/design documents. Old
# root-level names are included because they remain in Git history after moves.
git filter-repo \
  --force \
  --mailmap "${mailmap}" \
  --invert-paths \
  --path docs \
  --path VALIDATION.md \
  --path TOOL_IMAGE_FIX_PLAN.md \
  --path PLUGIN_ONLY_499_REFACTOR_PLAN.md \
  --path-glob 'OpenCode-Go-Omni-*.md'

if git log --all --format='%ae%n%ce' | grep -F --quiet "${OLD_EMAIL}"; then
  echo "rewrite failed: old email remains in commit metadata" >&2
  exit 1
fi

if [[ -n "$(git for-each-ref refs/tags --format='%(refname)')" ]]; then
  echo "rewrite failed: tags must be recreated with a noreply tagger identity" >&2
  exit 1
fi

if git log --all --name-only --pretty=format: | grep -Eq '^(docs/|VALIDATION\.md$|TOOL_IMAGE_FIX_PLAN\.md$|PLUGIN_ONLY_499_REFACTOR_PLAN\.md$|OpenCode-Go-Omni-)'; then
  echo "rewrite failed: internal documentation remains in history" >&2
  exit 1
fi

local_path_pattern='[A-Za-z]:[/\\]|/(Users|home)/[^/]+/'
if git log --all --format='%B' | grep -Ei "${local_path_pattern}" >/dev/null; then
  echo "rewrite failed: local absolute paths remain in commit messages" >&2
  exit 1
fi

if git rev-list --objects --all \
  | git cat-file --batch-check='%(objectname) %(objecttype)' \
  | awk '$2 == "blob" {print $1}' \
  | sort -u \
  | while IFS= read -r object; do git cat-file blob "${object}"; done \
  | grep -Ei "${local_path_pattern}" >/dev/null; then
  echo "rewrite failed: local absolute paths remain in file history" >&2
  exit 1
fi

echo "history sanitized; review git log/refs, then add the new public remote and push explicitly"
