#!/bin/sh
# A stand-in for `llama-funasr-cli --serve` (runtime/funasr-cli-serve.patch)
# for resident_test.go: the same stdin/stdout/stderr protocol, a reply that
# echoes the request. Each start appends a line to $FAKE_DIR/starts.
# FAKE_MODE: ok (default) | noready | die-once | hang-once — a *-once mode
# misbehaves on the first request it ever sees ($FAKE_DIR/tripped marks it).
echo start >> "$FAKE_DIR/starts"
echo "llama_model_load: loading" >&2
if [ "${FAKE_MODE:-ok}" = noready ]; then
	sleep 30
	exit 1
fi
echo "[ready] 0.01s" >&2
tab=$(printf '\t')
while IFS= read -r line; do
	wav=${line%%"$tab"*}
	hw=${line#*"$tab"}
	opt=
	case $hw in *"$tab"*) opt=${hw#*"$tab"}; hw=${hw%%"$tab"*} ;; esac
	case ${FAKE_MODE:-ok} in
	die-once | hang-once)
		if [ ! -e "$FAKE_DIR/tripped" ]; then
			: > "$FAKE_DIR/tripped"
			[ "$FAKE_MODE" = die-once ] && exit 3
			sleep 30
		fi ;;
	esac
	if [ ! -s "$wav" ]; then
		echo "failed to read audio" >&2
		echo
		echo "[done] 0.00s $wav" >&2
		continue
	fi
	echo "[hotwords] 1 of 1 terms, prefix 9 tokens, Chinese segments only" >&2
	echo "[stage] vad 0.01s enc 0.01s llm 0.01s audio 1.0s" >&2
	echo "  $(basename "$wav") /sil hw=[$hw] opt=$opt; args=[$*]"
	echo "[done] 0.03s $wav" >&2
done
