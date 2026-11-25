#!/usr/bin/env bash
set -euo pipefail

repo="${1:?usage: $0 OWNER/REPO}"
api_version="X-GitHub-Api-Version: 2026-03-10"

visibility="$(gh api -H "${api_version}" "repos/${repo}" --jq '.visibility')"
if [[ "${visibility}" != "public" ]]; then
  echo "refusing to apply public-repository settings to visibility=${visibility}" >&2
  exit 1
fi

default_branch="$(gh api -H "${api_version}" "repos/${repo}" --jq '.default_branch')"
if [[ "${default_branch}" != "main" ]]; then
  echo "refusing to protect unexpected default branch=${default_branch}" >&2
  exit 1
fi

gh api -X PUT -H "${api_version}" "repos/${repo}/actions/permissions" --input - <<'JSON'
{
  "enabled": true,
  "allowed_actions": "all",
  "sha_pinning_required": true
}
JSON

gh api -X PATCH -H "${api_version}" "repos/${repo}" --input - <<'JSON'
{
  "security_and_analysis": {
    "secret_scanning": {"status": "enabled"},
    "secret_scanning_push_protection": {"status": "enabled"}
  }
}
JSON

gh api -X PUT -H "${api_version}" "repos/${repo}/vulnerability-alerts"
gh api -X PUT -H "${api_version}" "repos/${repo}/automated-security-fixes"
gh api -X PUT -H "${api_version}" "repos/${repo}/private-vulnerability-reporting"
gh api -X PUT -H "${api_version}" "repos/${repo}/immutable-releases"

gh api -X PUT -H "${api_version}" "repos/${repo}/branches/${default_branch}/protection" --input - <<'JSON'
{
  "required_status_checks": {
    "strict": true,
    "contexts": ["Test"]
  },
  "enforce_admins": true,
  "required_pull_request_reviews": null,
  "restrictions": null,
  "required_linear_history": true,
  "allow_force_pushes": false,
  "allow_deletions": false,
  "required_conversation_resolution": true
}
JSON

assert_value() {
  local label="$1" route="$2" filter="$3" expected="$4" actual endpoint="repos/${repo}"
  if [[ -n "${route}" ]]; then
    endpoint+="/${route}"
  fi
  actual="$(gh api -H "${api_version}" "${endpoint}" --jq "${filter}")"
  if [[ "${actual}" != "${expected}" ]]; then
    echo "verification failed: ${label}" >&2
    exit 1
  fi
  echo "verified: ${label}=${actual}"
}

assert_value "visibility" "" '.visibility' "public"
assert_value "default_branch" "" '.default_branch' "main"
assert_value "secret_scanning" "" '.security_and_analysis.secret_scanning.status // "disabled"' "enabled"
assert_value "push_protection" "" '.security_and_analysis.secret_scanning_push_protection.status // "disabled"' "enabled"
assert_value "actions_enabled" "actions/permissions" '.enabled' "true"
assert_value "allowed_actions" "actions/permissions" '.allowed_actions' "all"
assert_value "actions_sha_pinning_required" "actions/permissions" '.sha_pinning_required' "true"
assert_value "dependabot_security_updates" "automated-security-fixes" '.enabled' "true"
assert_value "private_vulnerability_reporting" "private-vulnerability-reporting" '.enabled' "true"
assert_value "immutable_releases" "immutable-releases" '.enabled' "true"

if ! gh api -H "${api_version}" "repos/${repo}/vulnerability-alerts" >/dev/null; then
  echo "verification failed: vulnerability alerts" >&2
  exit 1
fi
echo "verified: vulnerability_alerts=enabled"

assert_value "status_check_test" "branches/${default_branch}/protection" \
  '[.required_status_checks.contexts[]? | select(. == "Test")] | length' "1"
assert_value "enforce_admins" "branches/${default_branch}/protection" '.enforce_admins.enabled' "true"
assert_value "linear_history" "branches/${default_branch}/protection" '.required_linear_history.enabled' "true"
assert_value "force_push_denied" "branches/${default_branch}/protection" '.allow_force_pushes.enabled' "false"
assert_value "deletion_denied" "branches/${default_branch}/protection" '.allow_deletions.enabled' "false"
assert_value "conversation_resolution" "branches/${default_branch}/protection" '.required_conversation_resolution.enabled' "true"
