{
  description = "mega-asr — local ASR (FunASR GGUF on llama.cpp) and megavoice, the voice-input app";

  inputs.nixpkgs.url = "github:nixos/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      rev = self.shortRev or self.dirtyShortRev or "dirty";
      linux = f: nixpkgs.lib.genAttrs [ "x86_64-linux" "aarch64-linux" ] (s: f nixpkgs.legacyPackages.${s});
    in
    {
      packages = {
        aarch64-darwin =
          let
            pkgs = nixpkgs.legacyPackages.aarch64-darwin;
          in
          rec {
            megavoice = pkgs.callPackage ./package.nix { inherit rev; };
            default = megavoice;
          };
      }
      // linux (pkgs: rec {
        funasr-root = pkgs.callPackage ./runtime/funasr-root.nix { };
        megameet = pkgs.callPackage ./package.nix { inherit rev; };
        # the same build: `megavoice mic serve`, the paired mic server
        megavoice = megameet;
        default = funasr-root;
      });
    };
}
