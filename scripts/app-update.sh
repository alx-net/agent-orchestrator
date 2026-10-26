#!/usr/bin/env bash
# Build a ref of the fork and install it as the everyday app.
#
#   ao-app-update            # origin/main
#   ao-app-update my-branch  # origin/my-branch (or any commit-ish)
#
# Builds in a dedicated checkout (~/.ao/build-src), never in a workspace, so
# work in progress is untouched. `scripts/dev.sh link-updater` puts this script
# on PATH as `ao-app-update`; the link points into the build checkout, so the
# updater updates itself along with the app.
set -euo pipefail

main() {
  local ref="${1:-main}"
  local self repo src target
  self="$(cd "$(dirname "$(readlink -f "${BASH_SOURCE[0]}")")" && pwd)"
  repo="${AO_APP_REPO:-$(cd "${self}" && cd "$(git rev-parse --git-common-dir)/.." && pwd)}"
  src="${AO_APP_SRC:-${HOME}/.ao/build-src}"

  printf '\033[1;34m==>\033[0m fetching %s\n' "$(git -C "${repo}" remote get-url origin)"
  git -C "${repo}" fetch --quiet origin
  if git -C "${repo}" rev-parse --verify --quiet "origin/${ref}^{commit}" >/dev/null; then
    target="origin/${ref}"
  else
    target="${ref}"
  fi
  if [[ ! -e "${src}/.git" ]]; then
    git -C "${repo}" worktree add --quiet --detach "${src}" "${target}"
  else
    if [[ -n "$(git -C "${src}" status --porcelain --untracked-files=no)" ]]; then
      printf 'error: %s has local edits; commit or discard them first\n' "${src}" >&2
      exit 1
    fi
    git -C "${src}" checkout --quiet --detach "${target}"
  fi
  printf '\033[1;34m==>\033[0m building %s (%s)\n' "${target}" "$(git -C "${src}" log -1 --format='%h %s')"
  AO_REOPEN=1 bash "${src}/scripts/dev.sh" install
}

# Parsed in full before running: the checkout above may rewrite this file.
main "$@"
exit
