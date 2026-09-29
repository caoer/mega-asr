# funasr-root — an engine root for megavoice and megameet on Linux, the
# directory `[asr.funasr] root` names:
#   bin/llama-funasr-cli   FunASR v1.3.30's llama.cpp runtime with
#                          runtime/funasr-cli-serve.patch (build-funasr-cli.sh
#                          documents the flags); CPU, AVX2 on x86_64
#   models/                Fun-ASR-Nano (SAN-M encoder f16, Qwen3-0.6B q8_0) and
#                          FSMN-VAD, pinned by Hugging Face revision and sha256 —
#                          the same bytes as the repo's annexed models/
#
#   nix build .#funasr-root -o ~/.local/share/mega-asr/funasr-root
{
  lib,
  stdenv,
  fetchFromGitHub,
  fetchurl,
  cmake,
  runCommand,
}:
let
  funasr = fetchFromGitHub {
    owner = "modelscope";
    repo = "FunASR";
    rev = "16cd165ac3946cc8c08bf845331f91fefec8e1a9"; # v1.3.30
    hash = "sha256-utcPHH4mMKyvGNgK4XgxIbhFmnDsdRpETqc+R+5VvQc=";
  };
  # runtime/llama.cpp/CMakeLists.txt FetchContent's pin, supplied up front
  # because the build sandbox has no network
  llama = fetchFromGitHub {
    owner = "ggml-org";
    repo = "llama.cpp";
    rev = "8086439a4cea94c71a5dfb8fe4ad1546aebd640f";
    hash = "sha256-MW3NehJRfConxNrOLsAmeWVTtQF36z8EApohCcZ9zEE=";
  };

  cli = stdenv.mkDerivation {
    pname = "llama-funasr-cli";
    version = "1.3.30-serve";
    src = funasr;
    patches = [ ./funasr-cli-serve.patch ];
    nativeBuildInputs = [ cmake ];
    cmakeDir = "../runtime/llama.cpp";
    cmakeFlags = [
      "-DFETCHCONTENT_SOURCE_DIR_LLAMA=${llama}"
      "-DFETCHCONTENT_FULLY_DISCONNECTED=ON"
      "-DGGML_NATIVE=OFF"
    ]
    # SOURCE_DATE_EPOCH turns ggml's x86 extensions off with its native
    # default; AVX2 is the baseline FunASR's own Linux release targets.
    ++ lib.optionals stdenv.hostPlatform.isx86_64 [
      "-DGGML_SSE42=ON"
      "-DGGML_AVX=ON"
      "-DGGML_AVX2=ON"
      "-DGGML_BMI2=ON"
      "-DGGML_FMA=ON"
      "-DGGML_F16C=ON"
    ];
    buildFlags = [ "llama-funasr-cli" ];
    installPhase = ''
      runHook preInstall
      install -Dm755 bin/llama-funasr-cli $out/bin/llama-funasr-cli
      runHook postInstall
    '';
    meta = {
      description = "Fun-ASR-Nano on llama.cpp with resident --serve, hotwords and the decode loop guard";
      homepage = "https://github.com/caoer/mega-asr";
      license = lib.licenses.mit;
      platforms = lib.platforms.linux;
      mainProgram = "llama-funasr-cli";
    };
  };

  hf =
    repo: rev: file: sha256:
    fetchurl {
      url = "https://huggingface.co/${repo}/resolve/${rev}/${file}";
      inherit sha256;
    };
  nano = "46e849502a867080d66d351b8dfb1018b607e509";
  models = {
    "funasr-encoder-f16.gguf" =
      hf "FunAudioLLM/Fun-ASR-Nano-GGUF" nano "funasr-encoder-f16.gguf"
        "f92f91d01a24fbed6c863495b2ee8c6a6788144a02858b75743f0946668de8a2";
    "qwen3-0.6b-q8_0.gguf" =
      hf "FunAudioLLM/Fun-ASR-Nano-GGUF" nano "qwen3-0.6b-q8_0.gguf"
        "819f385dc0e035dccc3d9e7edaf6b7b044b8ba7ace63cbcbf84c7e397eecbf27";
    "fsmn-vad.gguf" =
      hf "FunAudioLLM/fsmn-vad-GGUF" "6840bae4c5c92ee8c04faaf4db23dd0105098d7f" "fsmn-vad.gguf"
        "1270f2559c495f4e7b6e739541151027d360761a3fda43fc147034f5719f5479";
  };
in
runCommand "funasr-root" { passthru = { inherit cli models; }; } ''
  mkdir -p $out/bin $out/models
  ln -s ${cli}/bin/llama-funasr-cli $out/bin/llama-funasr-cli
  ${lib.concatStrings (lib.mapAttrsToList (n: f: "ln -s ${f} $out/models/${n}\n") models)}
''
