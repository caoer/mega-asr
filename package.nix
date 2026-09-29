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
{
  lib,
  stdenv,
  buildGoModule,
  gitMinimal,
  perl,
  rev ? "dirty",
  asrRoot ? null,
}:
let
  # each app's constant Mach-O UUID (LC_UUID): see ldflags
  uuid = {
    megavoice = "cbf22ecfc978401fac4cf709df9460a4";
    megameet = "b9e677eb301d4c1487f1ef0ff6d53de9";
  };
in
buildGoModule {
  pname = "megavoice";
  version = "0-${rev}";

  src = lib.fileset.toSource {
    root = ./.;
    fileset = lib.fileset.unions [
      ./go.mod
      ./go.sum
      ./cmd
      ./internal
      ./packaging
      ./scripts
    ];
  };

  vendorHash = "sha256-gGLbUlMM/yqkBEad95K7SC0GpDDVWkGh971YIHKXesg=";
  subPackages = [
    "cmd/megavoice"
    "cmd/megameet"
  ];
  env.CGO_ENABLED = "0";
  # register-feishu's test files minutes in a scratch git wiki
  nativeCheckInputs = [ gitMinimal ];
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
  ++ [ "-X github.com/caoer/mega-asr/internal/app.Build=${rev}" ];

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
    sh scripts/bundle.sh $out/bin/megavoice $out/libexec/megavoice/MegaVoice.app
    sh scripts/bundle.sh $out/bin/megameet $out/libexec/megavoice/MegaMeet.app packaging/megameet.plist
    install -Dm755 scripts/install-app.sh $out/libexec/megavoice/install-app.sh
  '';

  meta = {
    description = "Tap a key, speak, and the transcript lands where you were typing";
    homepage = "https://github.com/caoer/mega-asr";
    mainProgram = "megavoice";
    platforms = lib.platforms.darwin ++ lib.platforms.linux; # Linux: megameet's server side (drain, pull, process) and megavoice's mic server
  };
}
