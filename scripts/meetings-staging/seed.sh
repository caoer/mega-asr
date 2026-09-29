#!/usr/bin/env bash
# seed.sh — fill a private staging page with a slice of the production meetings
# page, so the React app's gates run on real records without touching production.
#
#   seed.sh                 publish a new private staging page, mint its links, seed it
#   seed.sh <staging-slug>  (re)seed an existing staging page
#
# MEETINGS_PROD_SLUG names the production meetings page (required).
#
# Production is read, never written. The slice: the `labels` and `config` records, the newest
# 20 live records, every failed one and every tombstone; their text files
# (segments, transcript, feishu-transcript); and the audio of the three records
# with the smallest media over 1 MB. Copied files get new ids on the staging page,
# and each record's `files[].file` and `feishu.transcript_file` are re-keyed to
# them; an audio file not copied is dropped from the record's `files`. A reseed
# reuses files already copied (their `meta.src` names the production id) and
# overwrites the seeded records, so it also undoes a gate's writes.
#
# Needs the owner's ucc identity (UCC_USERNAME / UCC_AUTH_TOKEN) and the
# ccc-pages tools (page-publish, page-data, page-file, page-token), jq.
set -euo pipefail
shopt -s inherit_errexit

PROD=${MEETINGS_PROD_SLUG:?seed.sh: set MEETINGS_PROD_SLUG to the production meetings page slug}
LIVE_N=${SEED_LIVE_N:-20}
AUDIO_N=${SEED_AUDIO_N:-3}
PATH="$PATH:${UCC_HOME:-$HOME/.local/share/ucc}/bin/skills-bin"
for t in page-publish page-data page-file page-token jq; do command -v "$t" >/dev/null || { echo "seed.sh: $t not found" >&2; exit 2; }; done

# retry <cmd…> — the pages host drops a connection now and then; three tries.
retry() {
  local i
  for i in 1 2 3; do "$@" && return 0; echo "seed.sh: try $i failed: $*" >&2; sleep $((i * 2)); done
  return 1
}

work=$(mktemp -d "${TMPDIR:-/tmp}/meetings-seed.XXXXXX")
trap 'rm -rf "$work"' EXIT

# listing <cmd…> — every row of a paged page-data/page-file listing, one JSON array.
listing() {
  local after="" out="$work/rows.json" page="$work/page.json"
  echo '[]' >"$out"
  while :; do
    if [ -n "$after" ]; then retry "$@" --after "$after" >"$page"; else retry "$@" >"$page"; fi
    jq -s '.[0] + .[1].keys' "$out" "$page" >"$out.n" && mv "$out.n" "$out"
    after=$(jq -r '.cursor // empty' "$page")
    [ -n "$after" ] || break
  done
  cat "$out"
}

STAGING=${1:-}
if [ -z "$STAGING" ]; then
  mkdir -p "$work/meetings-staging"
  echo '<!doctype html><title>Meetings staging</title><p>Meetings staging — publish the app build here.' >"$work/meetings-staging/index.html"
  STAGING=$(page-publish "$work/meetings-staging" --private --data on --no-bundle --title "Meetings staging" \
    --description "Staging for the React meetings app; seeded from production by mega-asr scripts/meetings-staging/seed.sh" | jq -r .slug)
  echo "staging page: $STAGING"
  for role in viewer contributor editor; do page-token mint "$STAGING" --role "$role" --label "staging $role"; done
fi

echo "reading $PROD …" >&2
listing page-data list "$PROD" --prefix rec. --values >"$work/prod-recs.json"
listing page-file list "$PROD" >"$work/prod-files.json"
listing page-file list "$STAGING" >"$work/staging-files.json"
retry page-data get "$PROD" labels --data-only >"$work/labels.json"

# The slice, and which of its records bring their audio.
jq --argjson n "$LIVE_N" --argjson a "$AUDIO_N" '
  [.[].value.data] as $all
  | ($all | map(select(.state != "deleted" and .state != "failed")) | sort_by(.started // "") | reverse | .[:$n]) as $live
  | ($live + ($all | map(select(.state == "failed" or .state == "deleted")))) as $recs
  | ($live
     | map({id, bytes: ([.files[]? | select(.role == "media" and .file != null) | .bytes // 0] | max // 0)})
     | map(select(.bytes > 1000000)) | sort_by(.bytes) | .[:$a] | map(.id)) as $audio
  | {recs: $recs, audio: $audio}' "$work/prod-recs.json" >"$work/slice.json"
echo "slice: $(jq '.recs | length' "$work/slice.json") records, audio of $(jq -c .audio "$work/slice.json")" >&2

retry page-data put "$STAGING" labels --schema labels@1 --data-file "$work/labels.json" --notify none >/dev/null
# The optional page configuration (meetings-config@1): wiki links, ingest wiki, zone.
if page-data get "$PROD" config --data-only >"$work/config.json" 2>/dev/null; then
  retry page-data put "$STAGING" config --schema meetings-config@1 --data-file "$work/config.json" --notify none >/dev/null
fi

# copy <prod-file-id> <rec-id> <role> → prints the staging file id (reused when already copied).
copy() {
  local src=$1 rec=$2 role=$3 have name
  have=$(jq -r --arg s "$src" '[.[] | select(.value.data.meta.src == $s) | .value.data.id][0] // empty' "$work/staging-files.json")
  if [ -n "$have" ]; then echo "$have"; return; fi
  name=$(jq -r --arg k "f.$src" '[.[] | select(.key == $k) | .value.data.name][0] // "file"' "$work/prod-files.json")
  retry page-file get "$PROD" "$src" -o "$work/blob" >/dev/null
  retry page-file upload "$STAGING" "$work/blob" --name "$name" \
    --meta "$(jq -cn --arg rec "$rec" --arg role "$role" --arg src "$src" '{rec: $rec, role: $role, src: $src}')" | jq -r .id
  rm -f "$work/blob"
}

while read -r rec <&3; do
  id=$(jq -r .id <<<"$rec")
  audio=$(jq --arg id "$id" '.audio | index($id) != null' "$work/slice.json")
  map='{}'
  while read -r f <&4; do
    [ -n "$f" ] || continue
    src=$(jq -r .file <<<"$f"); role=$(jq -r .role <<<"$f")
    case "$role" in
      segments | transcript | feishu-transcript) ;;
      media) [ "$audio" = true ] || continue ;;
      *) continue ;;
    esac
    new=$(copy "$src" "$id" "$role")
    [ -n "$new" ] || { echo "seed.sh: copying $role $src of $id gave no id" >&2; exit 1; }
    map=$(jq -c --arg s "$src" --arg n "$new" '. + {($s): $n}' <<<"$map")
  done 4< <(jq -c '.files[]? | select(.file != null)' <<<"$rec")
  jq --argjson m "$map" '
    .files |= (if . == null then null else map(if .file == null then . elif $m[.file] then .file = $m[.file] else empty end) end)
    | if .files == null then del(.files) else . end
    | if .feishu.transcript_file then (if $m[.feishu.transcript_file] then .feishu.transcript_file = $m[.feishu.transcript_file] else del(.feishu.transcript_file) end) else . end
  ' <<<"$rec" >"$work/rec.json"
  retry page-data put "$STAGING" "rec.$id" --schema meeting@1 --data-file "$work/rec.json" >/dev/null
  echo "rec.$id  files: $(jq -r '[.files[]?.role] | join(",")' "$work/rec.json")" >&2
done 3< <(jq -c '.recs[]' "$work/slice.json")

echo "seeded $STAGING: $(page-data list "$STAGING" --prefix rec. | jq '.keys | length') rec.* keys" >&2
