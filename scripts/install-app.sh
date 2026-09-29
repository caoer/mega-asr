#!/bin/sh
# install-app.sh <Name.app> — install the bundle at a stable path under a
# stable signature, so its TCC grants (Accessibility, Microphone, System Audio
# Recording) survive rebuilds: TCC keys a grant on the app's designated
# requirement (bundle id + signing identity), not on the file. The bundle id
# is also the launchd label of the app's agent (app.0xdao.megavoice,
# app.0xdao.megameet).
#
#   dest      ~/Applications/<Name.app>
#   identity  $MEGAVOICE_SIGN_IDENTITY, default the keychain's Apple Development
#             identity, else "MegaVoice Signing" (bin/megavoice-signing-cert)
#   stamp     the source path and the executable's checksum. The bundle
#             carries it in Contents/Resources/source; ~/Applications/
#             .<Name.app>.restarted holds it once the agent accepted the
#             restart onto that bundle. With both equal to this run's stamp
#             the run is a no-op; a bundle already installed is not copied
#             again, and only the restart is asked for.
#   restart   megavoice's agent: `megavoice restart --when-idle`, which the
#             agent accepts at once and carries out once nothing has been in
#             flight and no key was pressed for 3 s. An agent that refuses
#             (exit 75: one without --when-idle, busy) is asked again every
#             2 s for $MEGAVOICE_RESTART_WAIT seconds (default 60), then the
#             run fails without the restart stamp, so the next run asks again.
#             An agent that is not running (exit 69) is started with
#             `launchctl kickstart -k`. Any other app's agent is restarted
#             with `kickstart -k`. An agent launchd does not know is left
#             alone.
#
# Run as the user who owns the identity. Exit 1: nothing to sign with,
# signing failed (the installed app is left as it was), or the agent refused
# the restart for the whole wait.
set -eu
[ $# -eq 1 ] || { echo "usage: install-app.sh <Name.app>" >&2; exit 2; }
src=${1%/}
name=$(basename "$src")
dest=$HOME/Applications/$name
plist() { /usr/libexec/PlistBuddy -c "Print :$1" "$src/Contents/Info.plist"; }
bundle_id=$(plist CFBundleIdentifier)
exe=$(plist CFBundleExecutable)
label=$bundle_id

parent=$(dirname "$dest")
restarted=$parent/.$name.restarted
stamp="$src $(cksum <"$src/Contents/MacOS/$exe" | cut -d' ' -f1)"
installed=false
if [ "$(cat "$dest/Contents/Resources/source" 2>/dev/null)" = "$stamp" ] &&
	codesign --verify --strict "$dest" 2>/dev/null; then
	installed=true
	[ "$(cat "$restarted" 2>/dev/null)" = "$stamp" ] && exit 0
fi

# restart asks the agent to run the installed bundle; it fails when the
# agent refused for the whole wait.
restart() {
	uid=$(id -u)
	launchctl print "gui/$uid/$label" >/dev/null 2>&1 || return 0
	if [ "$exe" != megavoice ]; then
		launchctl kickstart -k "gui/$uid/$label"
		echo "install-app: restarted $label"
		return 0
	fi
	wait=${MEGAVOICE_RESTART_WAIT:-60}
	start=$(date +%s)
	told=false
	while :; do
		rc=0
		why=$("$dest/Contents/MacOS/$exe" restart --when-idle 2>&1 >/dev/null) || rc=$?
		case $rc in
		0)
			echo "install-app: $label accepted the restart; it restarts once nothing is in flight and no key was pressed for 3 s"
			return 0
			;;
		69)
			launchctl kickstart -k "gui/$uid/$label"
			echo "install-app: $label was not running; started it"
			return 0
			;;
		esac
		if [ $(($(date +%s) - start)) -ge "$wait" ]; then
			echo "install-app: $label refused the restart for ${wait} s (${why:-exit $rc}); it runs the build before this one until the next run" >&2
			return 1
		fi
		if ! $told; then
			echo "install-app: $label refused the restart (${why:-exit $rc}); asking again every 2 s for up to ${wait} s"
			told=true
		fi
		sleep 2
	done
}

if $installed; then
	restart
	printf '%s\n' "$stamp" >"$restarted"
	exit 0
fi

identity=${MEGAVOICE_SIGN_IDENTITY:-}
ids=$(security find-identity -v -p codesigning)
[ -n "$identity" ] || identity=$(printf '%s\n' "$ids" | sed -n 's/.*"\(Apple Development: [^"]*\)".*/\1/p' | head -1)
[ -n "$identity" ] || identity=$(printf '%s\n' "$ids" | sed -n 's/.*"\(MegaVoice Signing\)".*/\1/p' | head -1)
if [ -z "$identity" ]; then
	echo "install-app: no code-signing identity (Apple Development or MegaVoice Signing)" >&2
	exit 1
fi

mkdir -p "$parent"
stage=$(mktemp -d "$parent/.${name%.app}.XXXXXX")
trap 'rm -rf "$stage"' EXIT
cp -R "$src" "$stage/$name"
chmod -R u+w "$stage/$name"
mkdir -p "$stage/$name/Contents/Resources"
printf '%s\n' "$stamp" >"$stage/$name/Contents/Resources/source"
codesign --force --sign "$identity" --identifier "$bundle_id" "$stage/$name"
codesign --verify --strict "$stage/$name"

# Swap by rename: the running agent keeps executing its old, now unlinked
# image. Overwriting a running signed executable in place gets it SIGKILLed.
[ -e "$dest" ] && mv "$dest" "$stage/old"
mv "$stage/$name" "$dest"
echo "install-app: $dest ← $src (signed: $identity)"

restart
printf '%s\n' "$stamp" >"$restarted"
