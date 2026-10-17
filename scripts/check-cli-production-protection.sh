#!/usr/bin/env bash
set -euo pipefail

# GitHub creates an unprotected environment if its name is missing or misspelled.
# Check the live policy before any public CLI release or image promotion.
[[ "${GITHUB_REPOSITORY:-}" == layervai/qurl-integrations &&
   "${GITHUB_REF:-}" == refs/heads/main && -n "${GH_TOKEN:-}" ]] || {
  echo "::error::CLI publication requires the canonical main workflow" >&2
  exit 1
}

environment=cli-production
path="repos/$GITHUB_REPOSITORY/environments/$environment"
protection=$(gh api "$path")
jq -e '
  .name == "cli-production" and .can_admins_bypass == false and
  .deployment_branch_policy == {
    protected_branches:false, custom_branch_policies:true
  } and
  [.protection_rules[] | select(.type == "required_reviewers") |
    [.reviewers[] | {type, id:.reviewer.id}]] == [
      [{type:"User", id:178750268}]
    ]
' <<<"$protection" >/dev/null || {
  echo "::error::CLI production requires Justin's approval with administrator bypass disabled" >&2
  exit 1
}

policies=$(gh api "$path/deployment-branch-policies?per_page=100")
jq -e '
  .total_count == 1 and
  [.branch_policies[] | {name,type}] == [{name:"main",type:"branch"}]
' <<<"$policies" >/dev/null || {
  echo "::error::CLI production must allow only the main branch" >&2
  exit 1
}
