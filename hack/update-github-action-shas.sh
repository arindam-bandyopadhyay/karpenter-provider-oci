#!/usr/bin/env bash
# Karpenter Provider OCI
#
# Copyright (c) 2026 Oracle and/or its affiliates.
# Licensed under the Universal Permissive License v 1.0 as shown at https://oss.oracle.com/licenses/upl/

set -euo pipefail

GIT_BIN="${GIT_BIN:-git}"

DEFAULT_WORKFLOW_FILES=(".github/workflows/build.yaml" ".github/workflows/release.yaml")

ACTION_RE='^[[:space:]]*(-[[:space:]]*)?uses:[[:space:]]*[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+@'
PINNED_RE='^[[:space:]]*(-[[:space:]]*)?uses:[[:space:]]*[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+@[0-9a-f]{40}[[:space:]]*#[[:space:]]*v[0-9]+(\.[0-9]+){0,2}[[:space:]]*$'
UPDATE_RE='^([[:space:]]*(-[[:space:]]*)?uses:[[:space:]]*)([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)@([^[:space:]#]+)[[:space:]]*#[[:space:]]*(v[0-9]+(\.[0-9]+){0,2})[[:space:]]*$'

latest_action_pin() {
  local repo="$1" url="https://github.com/${1}.git"
  local tag sha

  tag="$("${GIT_BIN}" ls-remote --tags --refs --sort='v:refname' "${url}" 'refs/tags/v*' |
    awk -F/ '$NF ~ /^v[0-9]+([.][0-9]+){0,2}$/ { tag=$NF } END { if (tag) print tag; else exit 1 }')" || {
    echo "Error: no stable v* tag found for ${repo}" >&2
    return 1
  }

  sha="$("${GIT_BIN}" ls-remote --tags "${url}" "refs/tags/${tag}" "refs/tags/${tag}^{}" |
    awk -v tag="${tag}" '$2 == "refs/tags/" tag "^{}" { peeled=$1 } $2 == "refs/tags/" tag { direct=$1 } END { print peeled ? peeled : direct }')" || {
    echo "Error: failed to resolve ${repo}@${tag}" >&2
    return 1
  }

  [[ "${sha}" =~ ^[0-9a-f]{40}$ ]] || {
    echo "Error: failed to resolve a commit SHA for ${repo}@${tag}" >&2
    return 1
  }

  printf '%s %s\n' "${sha}" "${tag}"
}

validate_workflow_file() {
  local file="$1"
  local line

  while IFS= read -r line || [[ -n "${line}" ]]; do
    [[ ! "${line}" =~ ${ACTION_RE} || "${line}" =~ ${PINNED_RE} ]] && continue
    echo "Error: ${file} contains an external GitHub action that is not SHA-pinned with a # v... comment:" >&2
    echo "${line}" >&2
    return 1
  done < "${file}"
}

update_workflow_file() {
  local file="$1"
  local tmp line

  tmp="$(mktemp)"
  while IFS= read -r line || [[ -n "${line}" ]]; do
    if [[ "${line}" =~ ${UPDATE_RE} ]]; then
      local prefix="${BASH_REMATCH[1]}"
      local repo="${BASH_REMATCH[3]}"
      local current_ref="${BASH_REMATCH[4]}"
      local pin latest_sha latest_tag

      pin="$(latest_action_pin "${repo}")" || {
        rm -f "${tmp}"
        return 1
      }
      read -r latest_sha latest_tag <<< "${pin}"
      if [[ "${current_ref}" != "${latest_sha}" ]]; then
        echo "Updated ${file}: ${repo}@${current_ref} -> ${latest_sha} # ${latest_tag}" >&2
      fi
      printf '%s%s@%s # %s\n' "${prefix}" "${repo}" "${latest_sha}" "${latest_tag}" >> "${tmp}"
    elif [[ "${line}" =~ ${ACTION_RE} ]]; then
      echo "Error: ${file} contains an external GitHub action that is missing a # v... comment:" >&2
      echo "${line}" >&2
      rm -f "${tmp}"
      return 1
    else
      printf '%s\n' "${line}" >> "${tmp}"
    fi
  done < "${file}"

  mv "${tmp}" "${file}"
  validate_workflow_file "${file}"
}

main() {
  local files=("$@")
  local file

  [[ ${#files[@]} -gt 0 ]] || files=("${DEFAULT_WORKFLOW_FILES[@]}")

  for file in "${files[@]}"; do
    [[ -f "${file}" ]] || {
      echo "Error: workflow file not found: ${file}" >&2
      exit 1
    }

    update_workflow_file "${file}"
  done
}

main "$@"
