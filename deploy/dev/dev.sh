#!/usr/bin/env bash
# Runs the three long-lived processes `make dev` needs - the chaos-smtp mail
# sink, the sendplane server and the console dev server - in the foreground,
# and stops all three together on Ctrl-C (or SIGTERM). `make dev` calls this
# after the databases are up and migrated; it assumes deploy/dev/config.yaml
# already validates and web/node_modules already exists (both of which
# `make dev` arranges before running this script).
#
# Each process is also runnable on its own, in its own terminal, without this
# script: `make dev-smtp`, `make dev-server`, `make dev-web`.
set -uo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root_dir" || exit 1

pnpm_bin="${PNPM:-pnpm}"

pids=()
stopping=0

# Ctrl-C on the terminal already delivers SIGINT to this whole process group
# (dev.sh and the jobs it backgrounds below all share one, since none of them
# call setsid). This trap covers the other ways `make dev` gets stopped -
# `make` itself receiving SIGTERM, or dev.sh being killed directly - by
# explicitly signalling every pid it started and waiting for them to exit,
# rather than assuming they already got the signal.
cleanup() {
  if [ "$stopping" = 1 ]; then
    return
  fi
  stopping=1
  trap - INT TERM EXIT
  echo ""
  echo "dev.sh: stopping chaos-smtp, sendplane and the console dev server..."
  for pid in "${pids[@]}"; do
    kill -TERM "$pid" 2>/dev/null || true
  done
  wait 2>/dev/null
  echo "dev.sh: stopped"
}
trap cleanup INT TERM EXIT

go run ./cmd/chaos-smtp --listen 127.0.0.1:12525 --stats-listen :12590 \
  --tempfail=0 --permfail=0 --drop=0 --keep-messages=-1 --keep-bodies &
pids+=("$!")

go run ./cmd/sendplane --config deploy/dev/config.yaml \
  --roles control,sender,bounce --listen :8080 &
pids+=("$!")

(cd web && "$pnpm_bin" --filter @sendplane/console dev) &
pids+=("$!")

# wait -n (any single job) so a process that crashes early brings the others
# down with it instead of `make dev` sitting there with a dead sender.
if wait -n "${pids[@]}" 2>/dev/null; then
  status=0
else
  status=$?
fi
if [ "$stopping" = 0 ]; then
  echo "dev.sh: one of chaos-smtp/sendplane/console exited (status $status); stopping the rest"
fi
cleanup
exit "$status"
