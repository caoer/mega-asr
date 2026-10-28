// Package remote lets a paired iPhone drive this Mac's takes: start one,
// stop it to paste, stop it to paste and press Return, cancel it, and see
// the Mac's state. The watch reaches it through the phone.
//
// # Discovery
//
// The Mac registers ServiceType ("_megavoice-ctl._tcp") in local. through
// mDNSResponder, under the Mac's computer name, on remote.listen's port
// (default 7867), with TXT "v=1" and "fp=<the first 16 hex digits of the
// certificate's SHA-256>". The fp lets a phone match a service it browses to
// a Mac it paired without connecting; it is no secret.
//
// # Transport
//
// TLS 1.3 with a self-signed P-256 certificate (state dir: key.pem 0600,
// cert.pem, phones.toml 0600). The phone pins the certificate: the SHA-256 of
// the leaf's DER, lowercase hex, as the pairing QR gave it; it checks no
// host name. Over TLS, one WebSocket at GET /ctl (any path is answered the
// same, for a client that connects to a browsed service and cannot name
// one); JSON objects use text frames, and phone audio uses binary frames
// containing kind | payload. The direct read limit is MaxFrame (16 KiB).
//
// # Pairing
//
// The Mac's pairing page (menu › iPhone 遥控…, the loopback Takes listener)
// shows a QR code of a URL:
//
//	megavoice://pair?v=1&name=<Mac name>&port=<port>&fp=<64 hex>&s=<secret>&ip=<IPv4>,<IPv4>
//
// s is 16 random bytes, base64url without padding; it is live PairLife (2
// min) and pairs one phone, then it is gone. ip lists the Mac's LAN IPv4
// addresses for a phone that cannot browse. The phone pins fp, connects, and
// sends as its first message
//
//	{"type":"pair","secret":"<s>","device":"<the phone's name>"}
//
// and gets {"type":"paired","mac":"<Mac name>","token":"<token>"}: 32 random
// bytes, base64url, which the Mac keeps only as its SHA-256. A device name
// already paired is paired again: its old token is refused from then on. A
// wrong or spent secret gets {"type":"refused","code":"pair","reason":…} and
// the connection closes. A device name is 1 to 64 characters with no control
// character; it names the phone in the Mac's list, so a phone sends one that
// stays the same and differs from its owner's other phones (e.g. "iPhone
// 3fa2", a suffix kept with the token).
//
// # Session
//
// A paired phone's first message is
//
//	{"type":"hello","token":"<token>","device":"<the phone's name>"}
//
// answered by {"type":"hello","mac":"<Mac name>","state":<State>}, or by a
// refusal with code "token" (unknown or revoked: pair again) and a close. A
// connection that sends neither within HelloWait is closed. After a hello or
// a pairing, the phone sends commands:
//
//	{"type":"cmd","id":"<unique>","action":"start|stop|cancel|state",
//	 "end":"paste|send","via":"iphone|watch|keyboard","take":"<id>","sent_at":<unix ms>}
//
// In Mac mode, via="keyboard" identifies start, stop and cancel from the
// iPhone keyboard, recording with the Mac's own input.
//
// Every command is answered once, by id:
//
//	{"type":"reply","id":"<id>","ok":true|false,"code":"…","reason":"…","state":<State>}
//
// A command names the state it wants; none toggles. start starts a take
// (target: what has focus on the Mac then) and is refused with code
// "recording" while one records, "stale" when sent_at is more than Fresh (6
// s) before the Mac's clock, "exiting" while the Mac restarts or quits, and
// "start_failed" when the take could not start. stop ends the recording take
// to paste (end "paste", the default) or to paste and press Return (end
// "send"); cancel ends it unpasted. stop and cancel name the take they end,
// the State's take the phone pressed on, and are refused with "not_recording"
// when no take records and "other_take" when another one does: a stop sent
// again after a dropped connection, or raced by the Mac's own key or another
// phone, never ends a take it was not meant for. state only answers. A repeated id gets the first
// answer again, with the current state, and acts no second time. reason is
// Chinese, for the phone to show as it is.
//
// The Mac pushes, to every connected phone:
//
//	{"type":"state","state":<State>}                       on every change
//	{"type":"take","take":"<id>","final":"…","why":"…"}    a take's final state
//
// State is {"phase":"idle|recording|transcribing|delivering","take":"<id of
// the recording take, or empty>","elapsed_ms":<how long it has recorded>,
// "owner":"mac|stream"}. owner is who started the take that defines the
// phase: "mac" for the Mac's own input, whoever asked for it, "stream" for a
// take a phone keyboard streams. It is the recording take's; with none
// recording, "mac" while a take of the Mac's own is on its way, else
// "stream". It is omitted when idle and by an older Mac.
// final is one of sent, pasted, cancelled, empty, undelivered; why says why
// a take is undelivered. No text of a Mac-input take goes to the phone.
//
// # Phone keyboard streams (U5 contract)
//
// A keyboard start adds source="stream", stream="<16 hex digits>" and
// format="pcm16le-16k-mono", with via="keyboard". The Mac registers the
// stream before replying; its take id is returned in reply.take, and also
// State.take while it still records. Repeating the command id retains reply.take.
// Ordered binary records may follow start without waiting for that reply.
// Direct binary frames and relay plaintext use the same layout:
//
//	0x02 | stream id (8 bytes BE) | seq (u32 BE, from 0) | flags | PCM
//
// Only flags bit 0 (last) is defined. PCM is 1,600 int16 LE mono samples
// at 16 kHz, except a last record may contain 0..1,600 samples. A full
// plaintext record is at most 3,214 bytes. Three seconds without a record,
// an invalid record, a sequence gap or a repeat cuts the source with an error.
// stop adds take, stream and last_seq; that record must arrive within one second.
// Stream stops accept end="paste" (or absent), and never send Mac Enter.
// They use via="keyboard" on the originating session. Ordinary iPhone/watch
// stops, including a one-shot watch request, refuse a keyboard-stream take
// with other_take. They still stop a named Mac-input take on that connection.
// A cancel of a keyboard-stream take reaches it only via "keyboard" from the
// phone that streams it, on the originating session or a later one of the
// same phone (a phone that lost its connection cancels from the next); any
// other is refused with other_take. The Mac's own Return, Esc, tap key, menu
// and control socket leave it recording; its quit and restart stop it.
// The last flag also closes the source. A 300 ms pre-roll spans three records.
//
// The completed text is {"type":"text","take":"<Mac take>","text":"..."},
// sent only to the session that supplied the audio. No text is pasted on
// the Mac. Its take final still broadcasts to every phone. Session delivery
// failure leaves the Mac take undelivered; the phone keeps its own audio.
// A source error also leaves it undelivered with that error as why. A session
// with no registered stream drops audio. Text delivery waits for the session
// writer, not a phone adoption acknowledgement, and precedes the take final.
// Its whole JSON message, including now_ms and escaping, fits MaxTextMessage
// (16,359 bytes), reserving the relay's kind byte and 16-byte AEAD tag.
//
// Version 2 fields ride every path (relay/PROTOCOL.md § 8): every message
// the Mac sends carries now_ms, its clock in unix ms; State carries epoch
// (8 random bytes per Mac process, compared for equality) and seq (the
// version of phase, take and owner); hello and every reply carry recent, the last
// 3 takes as {take, final, why, at_ms}, final "" while a take is open. A
// state command is answered and not remembered.
//
// WebSocket status 1008 means this phone's token is no good: a revoke closes
// its connections with 1008 and reason "revoked", and a "token" refusal
// closes with 1008. Only a "token" refusal and a 1008 close make a phone stop
// and pair again. Every other refusal and close is one to connect again
// after: "busy" (16 direct connections open) with 1013, a pairing or a
// malformed first message with 1000, no first message within HelloWait or a
// phone that did not keep up with 1013, the Mac stopping with 1001.
//
// # Cloud engine key
//
// A phone session (direct after pair or hello, or relay) may ask for a cloud
// engine's key, so the phone can stream to the engine itself:
//
//	{"type":"asr_key","id":"<unique>","engine":"doubao"}
//
// answered once, by id, with the key the Mac's config names
// ([asr.doubao] key_file, read at the request), its endpoint and resource:
//
//	{"type":"asr_key","id":"<id>","ok":true,"engine":"doubao","key":"…","url":"wss://…/api/v3/plan/sauc/…","resource_id":"…"}
//
// or with ok false, code "no_key" (no key, or the Mac gives none) or
// "bad_request", and a Chinese reason. The phone keeps the key in its
// Keychain and refuses a url outside the plan. The key rides only this
// session's TLS or sealed channel: it is never logged and never kept in the
// command replies remembered for repeated ids. A watch's one-shot request
// cannot ask for it.
//
// # Relay
//
// With [remote.relay] on, the Mac also holds one link to its room on the
// Mega Voice relay (internal/relay/client), and a paired phone and its watch
// reach it from any network. After a direct pair or authenticated hello the
// Mac offers {"type":"relay","url","room","mac_kem"}; the phone answers
// {"type":"relay_key","role":"phone","sig"}, once per direct connection.
// A hello registers an empty key slot or confirms the same key; a pairing
// may replace a registered key. The watch uses role watch under the same
// direct rule, or registers on a relay session. Each accepted key is answered
// {"type":"relay_ok","role"}, any refusal by a bad_request reply.
// Over the relay a phone's session runs as a direct one does, sealed end to
// end: its hello carries no token and the Mac knows it by its signed key; a
// session the Mac ends hears {"type":"bye","code","reason"} last, and a
// revoke sends a token refusal before bye 1008. At most 4 relay sessions run
// at once, counted apart from the 16 direct connections, so the relay cannot
// take the LAN path's slots; one more is refused "busy". A watch's request
// carries one command and gets its reply.
//
// # What it holds
//
// Nobody can make this Mac start, stop or type without a token, and only a
// phone that showed a live secret gets one. The secret reaches the phone
// through the camera, from the Mac's screen, and the phone sends it only to
// the certificate the same QR named, so someone on the network who cannot
// see the screen can neither pair nor learn a token. Someone on the network
// can find the service, connect, and keep the port busy; they cannot read or
// replay what a paired phone sends (TLS).
package remote
