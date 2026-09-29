#!/bin/sh
# build-funasr-cli.sh [workdir] — rebuild bin/llama-funasr-cli: FunASR v1.3.30's
# llama.cpp runtime with runtime/funasr-cli-serve.patch, which adds
#   --hotwords 'a, b, c'  the Fun-ASR-Nano hotword prompt (热词列表), capped at
#                         512 prompt tokens by dropping trailing terms
#   --hotwords-cjk        decode each VAD segment plainly first and again with
#                         the hotwords only when the text has CJK ideographs
#   --serve               load the models once; one request per stdin line
#                         "wav[\thotwords[\twhole]]", one reply line per
#                         request; whole decodes the file as one window
#                         without the VAD (the VAD segments it after all when
#                         that window yields no speech); [ready], [done] and
#                         [stage] timings on stderr
#   --gpu N               offload N Qwen3 layers to Metal (default 0: CPU)
#   --enc-gpu             run the SAN-M encoder on Metal; on the CPU when there
#                         is no GPU backend or it lacks an op ([enc] on stderr)
#   --vad-gpu             run FSMN-VAD on the encoder's GPU backend, with the
#                         same CPU fallback ([vad] on stderr); the VAD model is
#                         loaded once per process either way
#   --threads N           encoder CPU threads (default 8)
#   --llm-threads N       Qwen3 CPU threads, prompt and decode (default llama.cpp's 4)
# On macOS the main thread runs at user-interactive QoS, so the host work
# between GPU calls keeps its CPU on a loaded Mac.
# Every decode stops at 16 + 12 tokens per second of its window, or on a loop
# (a p-gram repeated max(4, 12/p) times), cut back to one copy ([loop] on
# stderr); v1.3.30 decodes to 512 tokens. With --hotwords-cjk, a hotword pass
# that loses the speech (no speech, or under half the plain text) is dropped.
# VAD segment texts are joined with one space between two sides that are not
# CJK (v1.3.30 glues them: "a." + "b" gives "a.b"), nothing next to CJK. Otherwise,
# without these flags, the output is byte-identical to the v1.3.30 release
# binary.
# macOS arm64: needs git, cmake (nix shell nixpkgs#cmake) and Xcode's clang; the
# binary replaces the repo's bin/llama-funasr-cli.
# Linux: needs git, cmake and gcc; the binary lands in OUTDIR (default
# WORKDIR/bin), built for this host's CPU (-march=native). On NixOS,
# `nix build .#funasr-root` builds a whole engine root instead (runtime/funasr-root.nix).
#
#   build-funasr-cli.sh [WORKDIR [OUTDIR]]
set -eu
here=$(cd "$(dirname "$0")/.." && pwd)
work=${1:-$(mktemp -d)}
cc=
if [ "$(uname -s)" = Darwin ]; then
  out=${2:-$here/bin}
  cc="-DCMAKE_C_COMPILER=/usr/bin/clang -DCMAKE_CXX_COMPILER=/usr/bin/clang++"
else
  out=${2:-$work/bin}
fi
git clone -q --depth 1 --branch v1.3.30 https://github.com/modelscope/FunASR.git "$work/FunASR"
git -C "$work/FunASR" apply "$here/runtime/funasr-cli-serve.patch"
cd "$work/FunASR/runtime/llama.cpp"
# shellcheck disable=SC2086
cmake -B build -DCMAKE_BUILD_TYPE=Release $cc
cmake --build build -j --target llama-funasr-cli
mkdir -p "$out"
cp build/bin/llama-funasr-cli "$out/llama-funasr-cli"
echo "built $out/llama-funasr-cli"
