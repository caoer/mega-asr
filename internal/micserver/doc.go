// Package micserver serves this machine's mic to paired Macs: a TLS server
// with a self-signed certificate, a PIN-authenticated pairing endpoint that
// hands a paired Mac a bearer token, and a WebSocket stream of 20 ms int16
// mono frames per connection.
//
// # Pairing
//
// The box shows an 8-digit PIN; the user types it on the Mac and nothing
// else. The PIN authenticates both sides through a PAKE, and the PAKE
// confirms the certificate the Mac then pins. What it must hold: someone in
// the path who does not know the PIN cannot pair, cannot learn the token,
// and cannot get the Mac to pin a certificate other than the box's; someone
// guessing online gets at most PINTries tries per PIN.
//
// The construction, with why each input is there:
//
//  1. The Mac opens TLS to the box without verifying it and reads the leaf
//     certificate's SHA-256, fp. Unverified on purpose: step 2 decides
//     whether fp is the box's. Every later connection of the pairing must
//     present the same fp (audio.PinnedTLS).
//
//  2. Both run CPace (pake.go) with the PIN as the password. The context
//     binds the client name (so nobody can pair the Mac under another name),
//     a fixed server role, and ad = "megavoice-pair-v1 cert " + fp, where
//     the Mac's fp is the certificate it saw and the box's is its own. A
//     relay that terminates TLS with its own certificate and forwards the
//     PAKE leaves the two sides with different fp, hence different keys: it
//     can only run its own PAKE, one PIN guess per try. The box's name is
//     not in the context, since the Mac learns it only from the answer;
//     step 5's confirmation binds it.
//
//  3. POST /pair {name, msgA} → {id, msgB}. The box counts a try here, under
//     its mutex, before it answers: anything keyed by the session lets the
//     caller test one guess, so every PAKE run spends a try whether or not a
//     confirmation follows. PINTries runs end the PIN. One pairing is
//     pending at a time: a new run displaces the pending one, whose
//     confirmation is then refused without spending a try. Any host that
//     reaches the port can so spend the tries or displace a pairing; every
//     run is logged with its address, and `mic pair` issues a fresh PIN.
//
//  4. POST /pair/confirm {id, confirm_mac}. confirm_mac is an HMAC, under a
//     key derived from the session key for this purpose, over both PAKE
//     messages. The box checks it in constant time: a mismatch (wrong PIN,
//     or a relay's different fp) is a spent try; a match consumes the PIN.
//     Only then does the box create the token (32 bytes from crypto/rand,
//     kept as its SHA-256).
//
//  5. The box answers {box, sealed, confirm_box}: the token sealed with
//     AES-256-GCM under a derived key (the client name as associated data),
//     and an HMAC under another derived key over both PAKE messages, the
//     box's name, the client name and the sealed token. The Mac checks
//     confirm_box before it opens the token, so it takes a name and a token
//     only from the peer that proved the same PIN and fp; it then pins fp.
//
// The token rides inside TLS and is sealed besides, so it stays secret even
// if the TLS peer is not the box. Derived keys come from HKDF-SHA256 over
// the CPace key with distinct labels; HMAC inputs are length-prefixed.
//
// # Streams
//
// GET /stream with Authorization: Bearer <token> is a WebSocket that sends
// the audio of one new Source as FrameSamples frames. The Mac ends it by
// closing; a revoke closes it with StatusPolicyViolation; a source error
// closes it with StatusInternalError and the error as the reason. Every
// pairing, connect and disconnect is logged with the name and address.
package micserver
