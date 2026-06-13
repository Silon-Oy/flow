#!/usr/bin/env bash
#
# deploy.sh — bring the production Flow stack up to origin/main.
#
# Run this ON the production host (the machine whose deploy/pgdata holds the
# live Postgres data). It fast-forwards main, rebuilds changed images, and
# recreates changed containers. Database migrations run automatically when
# flowd boots (store.Migrate); Postgres data persists in deploy/pgdata.
#
# Prerequisites on the host:
#   - deploy/.env present with real secrets (POSTGRES_PASSWORD, FLOW_GITHUB_TOKEN,
#     FLOW_BROKER_TOKEN, … — gitignored, never committed)
#   - Docker running
#
# Safe by design: refuses to deploy a dirty working tree, never resets the repo,
# never auto-loads a personal docker-compose.override.yml (uses explicit -f).
#
# The whole body lives in main() so bash parses it into memory before running;
# the `git switch main` below then cannot corrupt an in-flight read of this file.
set -euo pipefail

main() {
  local REPO_ROOT
  REPO_ROOT="$(git -C "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)" rev-parse --show-toplevel)"
  cd "$REPO_ROOT"

  local COMPOSE=(docker compose --env-file deploy/.env -f deploy/docker-compose.yml)

  # --- Preflight -----------------------------------------------------------
  if [ ! -f deploy/.env ]; then
    echo "deploy: deploy/.env missing — cannot resolve secrets. Aborting." >&2
    exit 1
  fi

  # Refuse on a dirty tree (gitignored runtime dirs like deploy/work, deploy/pgdata
  # do not show in --porcelain, so a clean prod checkout passes).
  if [ -n "$(git status --porcelain)" ]; then
    echo "deploy: working tree not clean — commit or stash first, refusing to deploy." >&2
    git status -sb >&2
    exit 1
  fi

  local ORIG_BRANCH
  ORIG_BRANCH="$(git rev-parse --abbrev-ref HEAD)"

  # --- Fast-forward main ---------------------------------------------------
  git fetch origin
  [ "$ORIG_BRANCH" != "main" ] && git switch main
  local PRE_PULL AFTER
  PRE_PULL="$(git rev-parse HEAD)"
  git pull --ff-only origin main
  AFTER="$(git rev-parse HEAD)"

  if [ "$PRE_PULL" = "$AFTER" ]; then
    echo "deploy: main already at $AFTER — rebuild/restart is a no-op for unchanged images."
  else
    echo "deploy: main $PRE_PULL -> $AFTER"
  fi

  # --- Compose up ----------------------------------------------------------
  # FLOW_WORK_DIR: host absolute path of the orchestrator runner work dir. Required
  # by the compose file (no default); it is the gitignored deploy/work checkout.
  export FLOW_WORK_DIR="$REPO_ROOT/deploy/work"
  mkdir -p "$FLOW_WORK_DIR"

  "${COMPOSE[@]}" up -d --build

  # Return to the branch the operator was on (non-destructive; tree was clean).
  [ "$ORIG_BRANCH" != "main" ] && git switch "$ORIG_BRANCH" >/dev/null 2>&1 || true

  echo
  echo "deploy: stack status —"
  "${COMPOSE[@]}" ps
}

main "$@"
