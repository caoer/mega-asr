#!/usr/bin/env bash
# pages/meetings-hook/install.sh <slug> — install or refresh the meetings
# page's host hook and pin it. Run it on the host that publishes the page,
# from a mega-asr checkout, as the page owner. Safe to re-run; run it after every
# publish of the page (docs/megameet.md § Publish the meetings page).
#
#   1. agent/ from this directory into the page's site ($UCC_HOME/data/
#      ccc-pages/sites/<slug>, cloned when absent, born when the page has no
#      bundle), committed and pushed as the site's bundle
#   2. the page's inbox secret minted when absent (the page's doorbell uses
#      the grant lane, which needs the inbox to exist; the secret itself is
#      not kept)
#   3. the tick host's checkout of the site cloned or pulled to that commit,
#      and the hook dry-run there
#   4. handler.hook pinned to that commit (only when it differs)
#   5. control created with admit true when absent (an existing control is
#      left as it is: a paused page stays paused)
#
# TICK_HOST (required) is the ssh name of the host whose ccc-pages tick runs
# page hooks. It must list the slug in its page-endpoints.md; step 3 says
# when it does not.
set -euo pipefail

slug=${1:?usage: install.sh <slug>}
tick=${TICK_HOST:?TICK_HOST: the ssh name of the host that runs page hooks}
here=$(cd "$(dirname "$0")" && pwd)
home=${UCC_HOME:-$HOME/.local/share/ucc}
site=$home/data/ccc-pages/sites/$slug
rev=$(git -C "$here" rev-parse --short HEAD)

say() { printf 'install: %s\n' "$*" >&2; }

# 1. The site: pulled, cloned from its bundle, or — a page published with
# --no-bundle has none — born from agent/ alone.
if [ -d "$site/.git" ]; then
  page-site pull "$slug" >/dev/null
elif page-site info "$slug" >/dev/null 2>&1; then
  page-site clone "$slug" >/dev/null
else
  tmp=$(mktemp -d)
  rsync -a "$here/agent" "$tmp/"
  page-site adopt "$tmp" --slug "$slug" >/dev/null
  rm -rf "$tmp"
  say "site $slug born"
fi
rsync -a --delete "$here/agent/" "$site/agent/"
git -C "$site" add -A agent
if git -C "$site" diff --cached --quiet; then
  say "agent/ unchanged in the site"
else
  git -C "$site" commit -qm "agent: the meetings hook from mega-asr $rev"
  say "agent/ committed from mega-asr $rev"
fi
page-site push "$slug" >/dev/null
sha=$(git -C "$site" rev-parse HEAD)
say "site $slug at $sha"

# 2. The inbox.
if page-inbox secret "$slug" --status | jq -e '.exists == true' >/dev/null; then
  say "inbox exists"
else
  page-inbox secret "$slug" >/dev/null
  say "inbox created"
fi

# 3. The tick host's checkout, and a dry run there.
ssh "$tick" bash -s -- "$slug" "$sha" <<'REMOTE'
set -euo pipefail
slug=$1 sha=$2
. "$HOME/.local/share/ucc/user-env.sh"
export PATH="$HOME/.local/share/ucc/bin/skills-bin:$PATH"
site=$HOME/.local/share/ucc/data/ccc-pages/sites/$slug
if [ -d "$site/.git" ]; then page-site pull "$slug" >/dev/null; else page-site clone "$slug" >/dev/null; fi
head=$(git -C "$site" rev-parse HEAD)
[ "$head" = "$sha" ] || { echo "install: $(hostname): the site is at $head, not $sha" >&2; exit 1; }
grep -q "^### $slug\$" "$HOME/.local/share/ucc/config/page-endpoints.md" ||
  echo "install: $(hostname) does not list $slug in page-endpoints.md: add it to that host's page-endpoints.md" >&2
page-inbox hook dry-run "$slug" --dir "$site" >&2
REMOTE

# 4. The pin.
pinned=$(page-data get "$slug" handler.hook --data-only 2>/dev/null | jq -r '.sha // empty' || true)
if [ "$pinned" = "$sha" ]; then
  say "handler.hook already pins $sha"
else
  page-inbox hook activate "$slug" --sha "$sha" --timeout 120 >/dev/null
  say "handler.hook pinned to $sha (was ${pinned:-none})"
fi

# 5. Admission.
if page-data get "$slug" control --data-only >/dev/null 2>&1; then
  say "control exists: $(page-data get "$slug" control --data-only | jq -c '{admit, note}')"
else
  page-data put "$slug" control --schema control@1 --notify none \
    --data "{\"admit\":true,\"stop_run\":null,\"note\":\"\",\"updated_at\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"}" >/dev/null
  say "control created, admit true"
fi
page-inbox hook status "$slug"
