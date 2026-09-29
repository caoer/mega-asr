# pages/meetings

The meetings page: a React app (Vite, Node 24, pnpm) served by ccc-pages as a private page with its record store on. The page's records and files are the megameet registry ([docs/megameet.md](../../docs/megameet.md)); the app holds no data, and nothing under this directory is ever a recording, a record or a snapshot.

`<slug>` below is your production page's slug. Every command runs as the page owner (`UCC_USERNAME` / `UCC_AUTH_TOKEN` in the shell) with the ccc-pages tools on `PATH`, and `TICK_HOST` set to the ssh name of the host whose ccc-pages tick runs page hooks (`install.sh` needs it).

## Configuration

The app ships no deployment values. It reads them from the page's optional `config` record (schema `meetings-config@1`); without it, wiki pages show unlinked and times render in the viewer's device zone.

```sh
page-data put <slug> config --schema meetings-config@1 --notify none --data '{
  "wikis": {
    "team-wiki": {
      "page": "https://git.example/team/team-wiki/src/branch/main/{path}",
      "commit": "https://git.example/team/team-wiki/src/commit/{commit}/{path}"
    }
  },
  "ingest_wiki": "team-wiki",
  "tz": "Europe/London"
}'
```

- `wikis`: web address templates by the wiki name the records and the `people.index` use. `{path}` is the repo-relative page path; `commit` adds `{commit}` and links a recording's wiki page as of its ingest commit.
- `ingest_wiki`: the wiki a recording's `wiki.page` lives in.
- `tz`: the zone a viewer sees until they choose one on the page.

## Build

```sh
cd pages/meetings
pnpm install --frozen-lockfile
pnpm typecheck && pnpm test && pnpm build     # → dist/
```

Without pnpm on the host: `nix shell nixpkgs#pnpm -c pnpm …`.

## Staging

Every change is gated on a private staging page before production. `scripts/meetings-staging/seed.sh` publishes a new staging page and mints its viewer and contributor links; `seed.sh <staging-slug>` reseeds an existing one, which also undoes a gate's writes. It copies a slice of production (labels, recent live records, failed ones, tombstones, their text files, three audios) and reads production only.

Publish the build to staging, then re-pin its hook:

```sh
page-publish pages/meetings/dist --slug <staging-slug> --private --no-bundle --label "<sha>"
bash pages/meetings-hook/install.sh <staging-slug>
```

Uploads, Delete and any other test write run on staging only: production's drain ingests whatever lands there into the wiki.

## Publish to production

```sh
page-publish pages/meetings/dist --slug <slug> --private --no-bundle --description Meetings --label "<sha>"
bash pages/meetings-hook/install.sh <slug>
```

Each publish is a new version; records and files stay. The `install.sh` re-run is part of every publish: a publish can move the page's site, and the script re-pins the label-answers hook (`handler.hook`) when it did and changes nothing when it did not.

## Rollback

```sh
page-publish --slug <slug> --versions      # active version and history
page-publish --slug <slug> --switch <n>    # serve version n again
bash pages/meetings-hook/install.sh <slug>
```

To stop the hook applying approved label questions: `page-inbox hook deactivate <slug>`.
