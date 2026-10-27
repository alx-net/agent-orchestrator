#!/usr/bin/env bash
# One entry point for the local fork dev loop. Run `scripts/dev.sh help`.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
frontend="${repo_root}/frontend"
backend="${repo_root}/backend"
node_major="$(tr -dc '0-9' < "${repo_root}/.nvmrc")"

say() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
die() { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

# Node 26/npm 12 refuses the lockfile's git dependency and blocks native
# install scripts, leaving a broken tree, so pin the CI major via nvm.
use_node() {
  if [[ "$(node -p 'process.versions.node.split(".")[0]' 2>/dev/null || true)" == "${node_major}" ]]; then
    return
  fi
  local nvm_sh="${NVM_DIR:-${HOME}/.nvm}/nvm.sh"
  [[ -s "${nvm_sh}" ]] || die "Node ${node_major} is required (found $(node -v 2>/dev/null || echo none)); install nvm or Node ${node_major}"
  set +u
  # shellcheck disable=SC1090
  source "${nvm_sh}" --no-use
  nvm use "${node_major}" >/dev/null 2>&1 || nvm install "${node_major}" >/dev/null
  set -u
}

check_go() {
  command -v go >/dev/null || die "Go is required (see backend/go.mod)"
  local want have
  want="$(awk '/^go /{print $2; exit}' "${backend}/go.mod")"
  have="$(go env GOVERSION | sed 's/^go//')"
  if [[ "$(printf '%s\n%s\n' "${want}" "${have}" | sort -V | head -1)" != "${want}" ]]; then
    die "Go ${want}+ required, found ${have}"
  fi
}

# npm ci only when the lockfile or Node major changed since the last install.
npm_install() {
  local dir="$1" stamp="$1/node_modules/.ao-install-stamp" want
  want="$(cat "${dir}/package-lock.json" | shasum | cut -d' ' -f1)-node${node_major}"
  if [[ "${FORCE_INSTALL:-}" != "1" && -f "${stamp}" && "$(cat "${stamp}")" == "${want}" ]]; then
    return
  fi
  say "npm ci in ${dir#"${repo_root}"}/"
  (cd "${dir}" && npm ci --no-audit --no-fund --loglevel=error)
  printf '%s' "${want}" > "${stamp}"
}

ensure_deps() {
  check_go
  use_node
  npm_install "${repo_root}"
  # frontend links ../packages/product-ui (file:), which resolves its own deps.
  npm_install "${repo_root}/packages/product-ui"
  npm_install "${frontend}"
}

cmd_setup() {
  ensure_deps
  say "warming Go build cache"
  (cd "${backend}" && go build ./...)
  say "ready ($(node -v), $(go env GOVERSION))"
}

# Each workspace gets its own dev profile, run file, data dir, and daemon port
# so several checkouts can run the dev app side by side.
isolated_env() {
  local name="${CONDUCTOR_WORKSPACE_NAME:-$(basename "${repo_root}")}"
  local base="${HOME}/.ao/dev/workspaces/${name}"
  export AO_DEV_ELECTRON_DIR="${base}/electron"
  export AO_RUN_FILE="${base}/running.json"
  export AO_DATA_DIR="${base}/data"
  if [[ -n "${CONDUCTOR_PORT:-}" ]]; then export AO_PORT="${CONDUCTOR_PORT}"; else unset AO_PORT; fi
  say "isolated dev data: ${base} (port ${AO_PORT:-3002})"
}

cmd_app() {
  ensure_deps
  if [[ "${1:-}" == "--real" ]]; then
    unset AO_RUN_FILE AO_PORT
    export AO_DATA_DIR="${AO_DATA_DIR:-${HOME}/.ao/data}"
    say "REAL data: ${AO_DATA_DIR} (quit the installed app first; only one daemon may own it)"
  else
    isolated_env
  fi
  cd "${frontend}" && exec npm run dev
}

cmd_web() {
  ensure_deps
  cd "${frontend}" && exec npm run dev:web
}

changed_go_packages() {
  local base
  base="$(git -C "${repo_root}" merge-base HEAD origin/main 2>/dev/null || echo HEAD)"
  { git -C "${repo_root}" diff --name-only "${base}" -- backend; git -C "${repo_root}" ls-files --others --exclude-standard -- backend; } |
    { grep '\.go$' || true; } | xargs -n1 dirname 2>/dev/null | sort -u |
    while read -r d; do [[ -d "${repo_root}/${d}" ]] && echo "./${d#backend/}"; done
}

# Packaging (and Forge) rebuild better-sqlite3 in place for Electron's ABI,
# which breaks the Node-hosted vitest run until it is rebuilt for Node.
ensure_node_native() {
  (cd "${frontend}" && node -e "new (require('better-sqlite3'))(':memory:')" >/dev/null 2>&1) && return
  say "rebuilding better-sqlite3 for Node $(node -v)"
  (cd "${frontend}" && npm rebuild better-sqlite3 --loglevel=error >/dev/null)
}

# Fast gate for the edit loop: build everything, vet/test only what changed.
cmd_check() {
  check_go
  use_node
  local pkgs
  pkgs="$(changed_go_packages | tr '\n' ' ')"
  say "go build ./..."
  (cd "${backend}" && go build ./...)
  if [[ -n "${pkgs// }" ]]; then
    say "go vet + test ${pkgs}"
    # shellcheck disable=SC2086
    (cd "${backend}" && go vet ${pkgs} && go test ${pkgs})
  fi
  say "frontend typecheck"
  (cd "${frontend}" && npm run --silent typecheck)
  local base files
  base="$(git -C "${repo_root}" merge-base HEAD origin/main 2>/dev/null || echo HEAD)"
  files="$({ git -C "${repo_root}" diff --name-only "${base}" -- frontend/src; git -C "${repo_root}" ls-files --others --exclude-standard -- frontend/src; } |
    { grep -E '\.(ts|tsx|css)$' || true; } | sed 's#^frontend/##' | sort -u |
    while read -r f; do [[ -f "${frontend}/${f}" ]] && echo "${f}"; done | tr '\n' ' ')"
  if [[ -n "${files// }" ]]; then
    ensure_node_native
    say "vitest related to changed frontend files"
    # shellcheck disable=SC2086
    (cd "${frontend}" && npx vitest related --run --config vite.renderer.config.ts --passWithNoTests ${files})
  fi
  say "check passed"
}

# The full CI-equivalent suites; slower, run before merging.
cmd_check_all() {
  cmd_setup
  (cd "${repo_root}" && npm run lint)
  ensure_node_native
  (cd "${frontend}" && npm run typecheck && npm test)
}

# Package this checkout and install it as the everyday app. The updater is
# pointed at the fork, so upstream releases never replace a custom build.
cmd_install() {
  cmd_setup
  local release_repo dest app
  release_repo="${AO_RELEASE_REPO:-$(gh repo view --json nameWithOwner -q .nameWithOwner 2>/dev/null || true)}"
  [[ -n "${release_repo}" ]] || die "set AO_RELEASE_REPO=owner/repo (your fork)"
  dest="${AO_INSTALL_DIR:-/Applications}"
  say "packaging (updater feed: ${release_repo})"
  # Shared cache: the bundled tmux source build takes minutes per checkout.
  (cd "${frontend}" && AO_BUILD_CACHE_DIR="${AO_BUILD_CACHE_DIR:-${HOME}/.ao/build-cache}" \
    AO_RELEASE_REPO="${release_repo}" AO_LOCAL_BUILD=1 npm run package)
  app="$(find "${frontend}/out" -maxdepth 2 -name '*.app' -type d -newer "${frontend}/package.json" | head -1)"
  [[ -n "${app}" ]] || die "no packaged .app found under frontend/out"
  # The main process of a running copy; it is restarted after the new files land.
  local running_pid
  running_pid="$(pgrep -f "${dest}/$(basename "${app}")/Contents/MacOS/[^/]*$" | head -1 || true)"
  say "installing to ${dest}"
  rm -rf "${dest:?}/$(basename "${app}")"
  ditto "${app}" "${dest}/$(basename "${app}")"
  # Local builds have no Developer ID; an ad-hoc seal keeps Apple Silicon happy.
  codesign --force --deep --sign - "${dest}/$(basename "${app}")" >/dev/null 2>&1 || true
  # Packaging rebuilt better-sqlite3 for Electron; put the Node build back so
  # tests in this checkout keep working.
  ensure_node_native
  say "installed ${dest}/$(basename "${app}") ($(git -C "${repo_root}" log -1 --format='%h %s'))"
  local marker="${HOME}/.ao/run/app-restart-signal"
  if [[ -n "${running_pid}" ]] && kill -0 "${running_pid}" 2>/dev/null; then
    # A plain quit only hides AO to the menu bar on macOS. A build that wrote
    # the marker handles SIGUSR2 by relaunching into the files just installed;
    # to an older build the signal would be fatal, so it is never sent there.
    if [[ -f "${marker}" && "$(cat "${marker}")" == "${running_pid}" ]]; then
      say "restarting $(basename "${app}" .app) into the new build"
      kill -USR2 "${running_pid}" 2>/dev/null || true
      for _ in $(seq 1 60); do kill -0 "${running_pid}" 2>/dev/null || break; sleep 1; done
    fi
    if kill -0 "${running_pid}" 2>/dev/null; then
      printf '\033[1;33m==>\033[0m %s is still running the old version.\n' "$(basename "${app}" .app)"
      printf '    Answer its unsent-message prompt if one is open, or use the menu-bar icon: Quit AO Completely, then open it again.\n'
    fi
  elif [[ "${AO_REOPEN:-}" == "1" ]]; then
    open "${dest}/$(basename "${app}")"
  fi
}

# Put `ao-app-update` on PATH, pointing at the build checkout's copy so the
# updater updates itself with the app.
cmd_link_updater() {
  local bin="${AO_BIN_DIR:-${HOME}/.local/bin}" src="${AO_APP_SRC:-${HOME}/.ao/build-src}"
  mkdir -p "${bin}"
  ln -sfn "${src}/scripts/app-update.sh" "${bin}/ao-app-update"
  say "linked ${bin}/ao-app-update -> ${src}/scripts/app-update.sh"
}

cmd_cli() {
  check_go
  bash "${repo_root}/scripts/daemon-build.sh"
}

# Fast-forward the fork's main from upstream and update local refs.
cmd_sync() {
  gh repo sync "$(gh repo view --json nameWithOwner -q .nameWithOwner)" --branch main
  git -C "${repo_root}" fetch --quiet origin
  git -C "${repo_root}" fetch --quiet upstream
  say "origin/main is now $(git -C "${repo_root}" rev-parse --short origin/main)"
}

cmd_help() {
  cat <<EOF
usage: scripts/dev.sh <command>

  setup       check Go/Node, install deps only when lockfiles changed
  app         run the Electron dev app on isolated per-workspace data
  app --real  run the dev app against ~/.ao/data (quit the installed app first)
  web         renderer only in a browser (no Electron APIs)
  check       fast gate: go build, vet+test changed Go pkgs, typecheck, related vitest
  check-all   full lint + test suites (CI equivalent, slower)
  install     package this checkout and install it into /Applications
  update [ref] build origin/<ref> (default main) in ~/.ao/build-src and install it
  link-updater put `ao-app-update` on PATH
  cli         build the ao CLI from source and link it onto PATH
  sync        fast-forward the fork's main from upstream
EOF
}

case "${1:-help}" in
  setup) cmd_setup ;;
  app) shift; cmd_app "$@" ;;
  web) cmd_web ;;
  check) cmd_check ;;
  check-all) cmd_check_all ;;
  install) cmd_install ;;
  update) shift; exec bash "${repo_root}/scripts/app-update.sh" "$@" ;;
  link-updater) cmd_link_updater ;;
  cli) cmd_cli ;;
  sync) cmd_sync ;;
  help|-h|--help) cmd_help ;;
  *) cmd_help; exit 2 ;;
esac
