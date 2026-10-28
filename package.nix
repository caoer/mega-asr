# megavoice — the voice-input app (cmd/megavoice) and megameet, the meeting
# recorder and, on Linux, its server (cmd/megameet); cgo-free: purego drives AppKit/CoreGraphics and
# Core Audio. The package carries the unsigned MegaVoice.app and MegaMeet.app
# and install-app.sh under libexec/megavoice/ — not Applications/, so
# nix-darwin never links an unsigned copy of a bundle id into
# /Applications/Nix Apps. install-app.sh installs each to ~/Applications
# under a stable signature.
#
# asrRoot is the build's default for the config key asr.funasr.root: an
# install passes the root it built (bin/llama-funasr-cli + models/), so its
# service and its shell CLI resolve the same one without a config edit.
#
# channel, releaseVersion, feedURL and pageToken make the staging build
# scripts/release-mac.sh releases to friends: MegaVoice.app carries the
# engine CLI, takes its version from the tag, fetches its models at first
# launch and updates itself from the release page's feed. Its bundle ids
# carry the channel (app.0xdao.megavoice.staging, scripts/bundle.sh), so a
# staging app and a dev app on one Mac keep separate TCC grants. A dev
# install passes none of them: no channel, so it never asks a feed for
# anything and keeps app.0xdao.megavoice.
#
# appName is the name a person sees: the bundle's CFBundleName and
# CFBundleDisplayName and the app's own menus, alerts and prompts. A staging
# build is "Mega Voice"; a dev build defaults to "Mega Voice Dev", and an
# install that wants its own name passes it. The bundle file stays
# MegaVoice.app and the bundle id is unchanged, so a rename keeps every grant.
{
  lib,
  stdenv,
  buildGo127Module,
  gitMinimal,
  perl,
  rev ? "dirty",
  asrRoot ? null,
  channel ? "",
  releaseVersion ? "", # the tag, v0.MINOR.PATCH
  feedURL ? "",
  pageToken ? "",
  appName ? (if channel == "" then "Mega Voice Dev" else "Mega Voice"),
}:
assert channel == "" -> feedURL == "" && pageToken == "" && releaseVersion == "";
assert builtins.match "[A-Za-z0-9 -]+" appName != null;
assert
  channel != ""
  -> feedURL != "" && pageToken != "" && builtins.match "v0\\.[0-9]+\\.[0-9]+" releaseVersion != null;
let
  # each app's constant Mach-O UUID (LC_UUID): see ldflags
  uuid = {
    megavoice = "cbf22ecfc978401fac4cf709df9460a4";
    megameet = "b9e677eb301d4c1487f1ef0ff6d53de9";
  };
in
buildGo127Module {
  pname = "megavoice";
  version = if releaseVersion != "" then lib.removePrefix "v" releaseVersion else "0-${rev}";

  src = lib.fileset.toSource {
    root = ./.;
    fileset = lib.fileset.unions [
      ./go.mod
      ./go.sum
      ./cmd
      ./internal
      ./packaging
      ./scripts
      # the phone-language contract tests (cmd/megavoice, internal/remote) read the iOS side's reason table
      ./ios/MegaVoiceKit/Tests/MacRemoteTests/Resources/MacSessionReasons.json
    ];
  };

  vendorHash = "sha256-aMYuccXJ6uNBslvDbSGP/PkWietQ3VqzOZDRO2UxpFw=";
  subPackages = [
    "cmd/megavoice"
    "cmd/megameet"
  ];
  env.CGO_ENABLED = "0";
  # register-feishu's test files minutes in a scratch git wiki
  # scripts/bundle.sh runs perl, and the bundle tests run it on every platform
  nativeCheckInputs = [
    gitMinimal
    perl
  ];
  # LC_UUID: buildGoModule links with -buildid=, which leaves a darwin
  # binary without one, and macOS local network privacy tracks an app by
  # its main executable's UUID -- without it the user's Allow never applies
  # and every LAN connection is dropped (NECP, "no route to host"). Each app
  # gets a constant UUID, so a rebuild keeps the Allow the user gave.
  ldflags = [
    "-s"
    "-w"
  ]
  ++ lib.optionals stdenv.hostPlatform.isDarwin [
    "-B"
    "0x${uuid.megavoice}"
  ]
  ++ lib.optional (asrRoot != null) "-X github.com/caoer/mega-asr/internal/app.DefaultASRRoot=${asrRoot}"
  # the build id: `megavoice version`, `status` and each take's start line
  ++ [ "-X github.com/caoer/mega-asr/internal/app.Build=${rev}" ]
  ++ [ "-X 'github.com/caoer/mega-asr/internal/app.AppName=${appName}'" ]
  ++ lib.optionals (channel != "") (
    map (kv: "-X github.com/caoer/mega-asr/internal/app.${kv}") [
      "Channel=${channel}"
      "Version=${releaseVersion}"
      "FeedURL=${feedURL}"
      "PageToken=${pageToken}"
    ]
  );

  nativeBuildInputs = lib.optional stdenv.hostPlatform.isDarwin perl;

  # One link gives both binaries megavoice's UUID; megameet gets its own
  # before it is bundled, and the build fails if either is not as set.
  postInstall = lib.optionalString stdenv.hostPlatform.isDarwin ''
    perl -0777 -pi -e 'BEGIN { $f = pack("H*", "${uuid.megavoice}"); $t = pack("H*", "${uuid.megameet}") }
      $n = s/\Q$f\E/$t/g; die "megameet: megavoice UUID found $n times, not once\n" unless $n == 1' $out/bin/megameet
    for b in megavoice megameet; do
      want=$(case $b in megavoice) echo ${uuid.megavoice};; megameet) echo ${uuid.megameet};; esac)
      got=$(otool -l $out/bin/$b | awk '$1 == "uuid" { gsub("-", "", $2); print tolower($2) }')
      [ "$got" = "$want" ] || { echo "$b: LC_UUID '$got', want $want" >&2; exit 1; }
    done
    CHANNEL=${channel} VERSION=${releaseVersion} NAME=${lib.escapeShellArg appName} sh scripts/bundle.sh $out/bin/megavoice $out/libexec/megavoice/MegaVoice.app
  ''
  + lib.optionalString (stdenv.hostPlatform.isDarwin && channel != "") ''
    install -m755 ${./bin/llama-funasr-cli} $out/libexec/megavoice/MegaVoice.app/Contents/MacOS/llama-funasr-cli
  ''
  + lib.optionalString stdenv.hostPlatform.isDarwin ''
    CHANNEL=${channel} sh scripts/bundle.sh $out/bin/megameet $out/libexec/megavoice/MegaMeet.app packaging/megameet.plist
    install -Dm755 scripts/install-app.sh $out/libexec/megavoice/install-app.sh
  '';

  meta = {
    description = "Tap a key, speak, and the transcript lands where you were typing";
    homepage = "https://github.com/caoer/mega-asr";
    mainProgram = "megavoice";
    platforms = lib.platforms.darwin ++ lib.platforms.linux; # Linux: megameet's server side (drain, pull, process) and megavoice's mic server
  };
}
