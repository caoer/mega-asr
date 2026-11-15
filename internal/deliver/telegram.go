package deliver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"

	"github.com/caoer/mega-asr/internal/refine"
	"github.com/caoer/mega-asr/internal/session"
	"github.com/caoer/mega-asr/internal/store"
)

// TelegramTarget is the keypad's locked target: a Telegram chat, a forum
// topic in it, and the labels the keypad shows for them. Deliver sets
// MessageID when the send command prints it.
type TelegramTarget struct {
	Chat       string // chat id, or @username
	Topic      int64  // forum topic id; 0 or 1 posts to General
	Group      string // the chat's label
	TopicLabel string // the topic's label; "" for General

	MessageID int64 // the sent message's id; 0 until a send printed one
}

// Record is the target as a take's record gives it: kind telegram, the
// group's label as the app and the topic's label as the title.
func (t *TelegramTarget) Record() store.Target {
	return store.Target{Kind: "telegram", App: t.Group, Title: t.TopicLabel}
}

func (t *TelegramTarget) String() string {
	s := "Telegram " + t.Group
	if t.TopicLabel != "" {
		s += " / " + t.TopicLabel
	}
	return s
}

// TelegramWait is a send command's deadline when Telegram.Wait is 0.
const TelegramWait = 20 * time.Second

// Telegram delivers a take's text to a Telegram chat by running an outside
// command: Argv, with {chat}, {topic} and {text} replaced in every element,
// the text also on its stdin and CCC_TELEGRAM_AGENT_BLOCK=off in its
// environment. {text} is safe only as a whole argv element of a command
// that takes it as data; a command that hands its argv to a shell (ssh,
// sh -c) reads the text from stdin. Exit 0 is sent, and `message <id>` on its stdout is the
// message's id; any other exit, or no exit by the deadline, is a failure
// whose reason is stderr's first line.
type Telegram struct {
	Argv   []string
	Stream *TelegramStream
	OnSent func(*TelegramTarget)
	Wait   time.Duration // the deadline; 0 is TelegramWait
	// Copy puts the text on the clipboard before the send, so a failed send
	// leaves it there as a failed paste does; nil copies nothing.
	Copy func(text string) error
}

// telegramChat is a chat the send command can be given inside a shell
// command line: a numeric id or an @username.
var telegramChat = regexp.MustCompile(`^(-?[0-9]+|@[A-Za-z0-9_]{1,64})$`)

// messageID is the send command's report of the message it sent.
var messageID = regexp.MustCompile(`message (\d+)`)

// TelegramLimit is how long a message Telegram takes; a longer text is
// refused (MESSAGE_TOO_LONG). It is counted in UTF-16 code units, as
// Telegram measures text: a part that fits in units fits in code points too,
// so an emoji or a rare CJK character never takes a part past it.
const TelegramLimit = 4096

// units is the length of s as Telegram counts it, in UTF-16 code units.
func units(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}

// markCut is where a both copy's second text starts.
const markCut = "\n\n" + refine.MarkOriginal

// partSent is a send cut after k of n messages went out: the take may have
// reached the chat in part (session.ErrMaybeSent). err is why message k+1
// did not go out.
type partSent struct {
	k, n int
	err  error
}

func (e *partSent) Error() string {
	return fmt.Sprintf("telegram: %d of %d messages sent, then %s", e.k, e.n, strings.TrimPrefix(e.err.Error(), "telegram: "))
}

func (e *partSent) Is(target error) bool { return target == session.ErrMaybeSent }
func (e *partSent) Unwrap() error        { return e.err }

// refused reports whether the message that did not go out was refused, so
// none of it reached the chat, rather than timed out.
func (e *partSent) refused() bool { return !errors.Is(e.err, session.ErrMaybeSent) }

// Deliver copies the text, then sends it to t. An error says why the text
// did not reach the chat; it is on the clipboard either way.
func (g Telegram) Deliver(t *TelegramTarget, text string) error {
	return g.DeliverClip(t, text, text)
}

// DeliverClip copies clip, then sends text to t: in one message, or, past
// TelegramLimit, in as many as it takes, in order (telegramParts). An error
// says why the text did not reach the chat; one after a part went out says
// how many did and is session.ErrMaybeSent (partSent).
func (g Telegram) DeliverClip(t *TelegramTarget, text, clip string) error {
	if g.Copy != nil {
		if err := g.Copy(clip); err != nil {
			return err
		}
	}
	if len(g.Argv) == 0 {
		return errors.New("telegram: no send command; set [keypad] via or send")
	}
	if !telegramChat.MatchString(t.Chat) {
		return fmt.Errorf("telegram: chat %q is not an id or @username", t.Chat)
	}
	parts := telegramParts(text)
	for k, part := range parts {
		if err := g.send(t, part); err != nil {
			if k == 0 {
				return err
			}
			return &partSent{k: k, n: len(parts), err: err}
		}
	}
	if g.OnSent != nil && t.MessageID > 0 {
		g.OnSent(t)
	}
	return nil
}

// send sends one message of at most TelegramLimit code points, through the
// stream when it takes the send, else by the send command, each under its
// own deadline; it sets t.MessageID when the send gave one, else leaves it 0,
// never an earlier message's.
func (g Telegram) send(t *TelegramTarget, text string) error {
	t.MessageID = 0
	wait := g.Wait
	if wait <= 0 {
		wait = TelegramWait
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	if g.Stream != nil {
		if id, used, err := g.Stream.send(ctx, t, text); used {
			if err == nil {
				t.MessageID = id
			}
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("telegram: send budget expired before fallback: %w", err)
	}
	topic := t.Topic
	if topic <= 0 {
		topic = 1
	}
	fill := strings.NewReplacer("{chat}", t.Chat, "{topic}", strconv.FormatInt(topic, 10), "{text}", text)
	argv := make([]string, len(g.Argv))
	for i, a := range g.Argv {
		argv[i] = fill.Replace(a)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), "CCC_TELEGRAM_AGENT_BLOCK=off")
	cmd.Stdin = strings.NewReader(text)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = time.Second // a child the kill missed cannot hold the pipes open
	if err := sendErr(cmd.Run(), ctx.Err() != nil, wait, stderr.String()); err != nil {
		return err
	}
	if m := messageID.FindStringSubmatch(stdout.String()); m != nil {
		t.MessageID, _ = strconv.ParseInt(m[1], 10, 64)
	}
	return nil
}

// telegramParts is text as messages of at most TelegramLimit units: text
// itself when it fits; a both copy whose first text fits, cut where its
// second text starts; else cut at the last paragraph break within the
// limit, else the last line break, else after the last 。, else the last
// space, else at the limit. The whitespace at a cut is dropped; every other
// character goes, in order.
func telegramParts(text string) []string {
	var parts []string
	for units(text) > TelegramLimit {
		end, n := 0, 0
		for i, r := range text {
			if n += utf16.RuneLen(r); n > TelegramLimit {
				end = i
				break
			}
		}
		head, cut := text[:end], end
		if i := strings.Index(text, markCut); i > 0 && i <= end {
			cut = i
		} else {
			for _, sep := range []string{"\n\n", "\n", "。", " "} {
				if i := strings.LastIndex(head, sep); i > 0 {
					cut = i
					if sep == "。" {
						cut += len(sep)
					}
					break
				}
			}
		}
		if part := strings.TrimRightFunc(text[:cut], unicode.IsSpace); part != "" {
			parts = append(parts, part)
		}
		text = strings.TrimLeftFunc(text[cut:], unicode.IsSpace)
	}
	if strings.TrimSpace(text) != "" || len(parts) == 0 {
		parts = append(parts, text)
	}
	return parts
}

// sendErr is a send's failure from its command's error, whether the
// deadline passed and its stderr: nil for an exit 0, even at the deadline;
// a timeout may have posted all the same (session.ErrMaybeSent).
func sendErr(err error, late bool, wait time.Duration, stderr string) error {
	switch {
	case err == nil:
		return nil
	case late:
		return fmt.Errorf("telegram: timeout, no answer in %s; %w", wait, session.ErrMaybeSent)
	}
	if why := firstLine(stderr); why != "" {
		return fmt.Errorf("telegram: %s", why)
	}
	return fmt.Errorf("telegram: %v", err)
}

// firstLine is s's first line that is not blank, trimmed.
func firstLine(s string) string {
	for l := range strings.Lines(s) {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

// TelegramCommand is the send command for sender user (the account
// ccc-telegram on via is logged in to) or
// bot, run by ssh on via, whose ccc-telegram holds the live session; the text
// goes over stdin, never into the remote shell's line, and reaches
// ccc-telegram as an argument after "--", so a text like "-h" is sent as text
// (needs a ccc-telegram on via that reads "--", 1.12.174 or later; an older one refuses it).
func TelegramCommand(sender, via string) (Telegram, error) {
	var send string
	switch sender {
	case "", "user":
		send = `ccc-telegram sendAsUser --topic {topic} -- {chat} "$(cat)"`
	case "bot":
		send = `ccc-telegram sendAsBot --plain --chat {chat} --topic {topic} -- "$(cat)"`
	default:
		return Telegram{}, fmt.Errorf("keypad sender %q: want user or bot", sender)
	}
	if via == "" {
		return Telegram{}, errors.New("keypad via: name the host that sends")
	}
	g := Telegram{Argv: []string{"ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", via,
		"CCC_TELEGRAM_AGENT_BLOCK=off " + send}}
	if sender == "" || sender == "user" {
		g.Stream = &TelegramStream{Argv: []string{"ssh", "-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "ServerAliveInterval=10", "-o", "ServerAliveCountMax=2", via,
			"CCC_TELEGRAM_AGENT_BLOCK=off ccc-telegram sender"}}
	}
	return g, nil
}
