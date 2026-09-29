//go:build darwin

package mac

import (
	"sync"

	"github.com/ebitengine/purego/objc"
)

// Item is one menu row. A row with Sep is a separator; one with Sub opens a
// submenu; one with Do is clickable when Enabled.
type Item struct {
	Title   string
	Enabled bool
	Checked bool
	Mono    bool   // monospaced digits and bars: a meter row
	Color   string // "", "red", "orange", "label" (full contrast on a disabled row)
	Indent  int
	Sep     bool
	Key     string // key equivalent with ⌘ ("q"), while the menu is open
	Do      func() // runs on the main thread
	Sub     []Item
}

// Bar is the status item's look: an SF Symbol, its tint ("" for the menu
// bar's own colour, "red", "orange") and a title beside it.
type Bar struct {
	Symbol, Tint, Title string
}

// StatusItem is an item in the menu bar with a menu. OnOpen runs on the main
// thread just before the menu shows, OnClose after it closes. Set and SetBar
// may be called from any goroutine.
type StatusItem struct {
	OnOpen, OnClose func()

	// main thread only
	item, menu objc.ID
	shape      string // the rendered items' structure; a change rebuilds the menu
	acts       []func()
	bar        Bar
}

var (
	targetOnce sync.Once
	target     objc.ID
	statusMu   sync.Mutex
	statusBy   = map[objc.ID]*StatusItem{} // menu → its item
)

// menuTarget is the one Objective-C object every menu row and menu calls:
// act: runs the row's func by its tag, the delegate methods the item's
// OnOpen/OnClose.
func menuTarget() objc.ID {
	targetOnce.Do(func() {
		byMenu := func(menu objc.ID) *StatusItem {
			statusMu.Lock()
			defer statusMu.Unlock()
			return statusBy[menu]
		}
		cls, err := objc.RegisterClass("MegaVoiceMenuTarget", objc.GetClass("NSObject"),
			[]*objc.Protocol{objc.GetProtocol("NSMenuDelegate")}, nil,
			[]objc.MethodDef{
				{Cmd: sel("act:"), Fn: func(_ objc.ID, _ objc.SEL, sender objc.ID) {
					menu := send(sender, "menu")
					for p := send(menu, "supermenu"); p != 0; p = send(p, "supermenu") {
						menu = p
					}
					s := byMenu(menu)
					tag := sendInt(sender, "tag")
					if s != nil && tag > 0 && tag <= len(s.acts) && s.acts[tag-1] != nil {
						s.acts[tag-1]()
					}
				}},
				{Cmd: sel("menuWillOpen:"), Fn: func(_ objc.ID, _ objc.SEL, menu objc.ID) {
					if s := byMenu(menu); s != nil && s.OnOpen != nil {
						s.OnOpen()
					}
				}},
				{Cmd: sel("menuDidClose:"), Fn: func(_ objc.ID, _ objc.SEL, menu objc.ID) {
					if s := byMenu(menu); s != nil && s.OnClose != nil {
						s.OnClose()
					}
				}},
			})
		if err != nil {
			panic("mac: " + err.Error())
		}
		target = send(send(objc.ID(cls), "alloc"), "init")
	})
	return target
}

// NewStatusItem puts an item with an empty menu in the menu bar.
func NewStatusItem(onOpen, onClose func()) *StatusItem {
	s := &StatusItem{OnOpen: onOpen, OnClose: onClose}
	DoSync(func() {
		bar := send(class("NSStatusBar"), "systemStatusBar")
		s.item = send(send(bar, "statusItemWithLength:", -1.0), "retain") // NSVariableStatusItemLength
		s.menu = send(send(class("NSMenu"), "alloc"), "initWithTitle:", nsString("MegaVoice"))
		send(s.menu, "setAutoenablesItems:", false)
		send(s.menu, "setDelegate:", menuTarget())
		send(s.item, "setMenu:", s.menu)
		statusMu.Lock()
		statusBy[s.menu] = s
		statusMu.Unlock()
	})
	return s
}

// SetBar changes the menu bar look.
func (s *StatusItem) SetBar(b Bar) {
	now(func() {
		if b == s.bar {
			return
		}
		s.bar = b
		btn := send(s.item, "button")
		img := send(class("NSImage"), "imageWithSystemSymbolName:accessibilityDescription:", nsString(b.Symbol), nsString("MegaVoice"))
		if c := color(b.Tint); img != 0 && c != 0 {
			// A coloured symbol is a non-template image drawn in its palette:
			// the menu bar keeps its colour.
			cfg := send(class("NSImageSymbolConfiguration"), "configurationWithPaletteColors:", send(class("NSArray"), "arrayWithObject:", c))
			img = send(img, "imageWithSymbolConfiguration:", cfg)
			send(img, "setTemplate:", false)
		} else if img != 0 {
			send(img, "setTemplate:", true)
		}
		send(btn, "setImage:", img)
		send(btn, "setImagePosition:", 2) // NSImageLeft
		title := b.Title
		if title != "" {
			title = " " + title
		}
		send(btn, "setAttributedTitle:", attributed(title, true, color(b.Tint)))
	})
}

func color(name string) objc.ID {
	switch name {
	case "red":
		return send(class("NSColor"), "systemRedColor")
	case "orange":
		return send(class("NSColor"), "systemOrangeColor")
	case "label":
		return send(class("NSColor"), "labelColor")
	}
	return 0
}

// attributed is s in the menu font, monospaced digits when mono, in fg when
// not 0.
func attributed(s string, mono bool, fg objc.ID) objc.ID {
	size := objc.Send[float64](class("NSFont"), sel("systemFontSize"))
	font := send(class("NSFont"), "menuFontOfSize:", 0.0)
	if mono {
		font = send(class("NSFont"), "monospacedDigitSystemFontOfSize:weight:", size, 0.0)
	}
	attrs := send(class("NSMutableDictionary"), "dictionary")
	send(attrs, "setObject:forKey:", font, nsString("NSFont")) // NSFontAttributeName
	if fg != 0 {
		send(attrs, "setObject:forKey:", fg, nsString("NSColor")) // NSForegroundColorAttributeName
	}
	return send(send(send(class("NSAttributedString"), "alloc"), "initWithString:attributes:", nsString(s), attrs), "autorelease")
}

// Set renders items. Rows keep their NSMenuItems while the structure (the
// count, separators and submenus) holds, so an open menu updates in place;
// otherwise the menu is rebuilt.
func (s *StatusItem) Set(items []Item) {
	now(func() {
		var acts []func()
		if sh := shape(items); sh != s.shape {
			send(s.menu, "removeAllItems")
			build(s.menu, items, &acts)
			s.shape = sh
		} else {
			update(s.menu, items, &acts)
		}
		s.acts = acts
	})
}

// now runs fn at once on the main thread (a menu about to open renders
// before it shows), else queues it there.
func now(fn func()) {
	if onMain() {
		fn()
		return
	}
	Do(fn)
}

func shape(items []Item) string {
	b := make([]byte, 0, len(items)+2)
	for _, it := range items {
		switch {
		case it.Sep:
			b = append(b, '-')
		case it.Sub != nil:
			b = append(b, '[')
			b = append(b, shape(it.Sub)...)
			b = append(b, ']')
		default:
			b = append(b, 'i')
		}
	}
	return string(b)
}

func build(menu objc.ID, items []Item, acts *[]func()) {
	for _, it := range items {
		if it.Sep {
			send(menu, "addItem:", send(class("NSMenuItem"), "separatorItem"))
			continue
		}
		mi := send(send(send(class("NSMenuItem"), "alloc"), "initWithTitle:action:keyEquivalent:", nsString(it.Title), sel("act:"), nsString("")), "autorelease")
		send(mi, "setTarget:", menuTarget())
		if it.Sub != nil {
			sub := send(send(send(class("NSMenu"), "alloc"), "initWithTitle:", nsString(it.Title)), "autorelease")
			send(sub, "setAutoenablesItems:", false)
			build(sub, it.Sub, acts)
			send(mi, "setSubmenu:", sub)
		}
		send(menu, "addItem:", mi)
		apply(mi, it, acts)
	}
}

func update(menu objc.ID, items []Item, acts *[]func()) {
	for i, it := range items {
		if it.Sep {
			continue
		}
		mi := send(menu, "itemAtIndex:", i)
		if it.Sub != nil {
			update(send(mi, "submenu"), it.Sub, acts)
		}
		apply(mi, it, acts)
	}
}

// apply sets a row's content; a clickable row gets the next tag.
func apply(mi objc.ID, it Item, acts *[]func()) {
	if it.Mono || it.Color != "" {
		send(mi, "setAttributedTitle:", attributed(it.Title, it.Mono, color(it.Color)))
	} else {
		send(mi, "setAttributedTitle:", objc.ID(0))
		send(mi, "setTitle:", nsString(it.Title))
	}
	send(mi, "setEnabled:", it.Enabled)
	state := 0
	if it.Checked {
		state = 1
	}
	send(mi, "setState:", state)
	send(mi, "setIndentationLevel:", it.Indent)
	send(mi, "setKeyEquivalent:", nsString(it.Key))
	tag := 0
	if it.Do != nil && it.Sub == nil {
		*acts = append(*acts, it.Do)
		tag = len(*acts)
	}
	send(mi, "setTag:", tag)
}

// Field is one text field of a Prompt.
type Field struct{ Label, Placeholder, Value string }

// Prompt asks for fields in a modal alert brought to the front, with ok as
// the default button; it returns the values, or false when cancelled. Call
// it on the main thread.
func Prompt(title, info, ok string, fields []Field) ([]string, bool) {
	app := send(class("NSApplication"), "sharedApplication")
	send(app, "activateIgnoringOtherApps:", true)
	alert := send(send(send(class("NSAlert"), "alloc"), "init"), "autorelease")
	send(alert, "setMessageText:", nsString(title))
	send(alert, "setInformativeText:", nsString(info))
	send(alert, "addButtonWithTitle:", nsString(ok))
	send(alert, "addButtonWithTitle:", nsString("Cancel"))
	const w, rowH = 300.0, 52.0
	view := send(send(send(class("NSView"), "alloc"), "initWithFrame:", CGRect{0, 0, w, rowH * float64(len(fields))}), "autorelease")
	tf := make([]objc.ID, len(fields))
	for i, f := range fields {
		y := rowH * float64(len(fields)-1-i)
		label := send(class("NSTextField"), "labelWithString:", nsString(f.Label))
		send(label, "setFrame:", CGRect{0, y + 28, w, 18})
		send(view, "addSubview:", label)
		t := send(class("NSTextField"), "textFieldWithString:", nsString(f.Value))
		send(t, "setPlaceholderString:", nsString(f.Placeholder))
		send(t, "setFrame:", CGRect{0, y + 4, w, 22})
		send(view, "addSubview:", t)
		tf[i] = t
	}
	for i := 0; i+1 < len(tf); i++ {
		send(tf[i], "setNextKeyView:", tf[i+1])
	}
	send(alert, "setAccessoryView:", view)
	send(alert, "layout")
	if len(tf) > 0 {
		send(send(alert, "window"), "setInitialFirstResponder:", tf[0])
	}
	var resp int
	modal(func() { resp = sendInt(alert, "runModal") })
	if resp != 1000 { // NSAlertFirstButtonReturn
		return nil, false
	}
	out := make([]string, len(tf))
	for i, t := range tf {
		out[i] = goString(send(t, "stringValue"))
	}
	return out, true
}

// Alert shows a message with an OK button, brought to the front. Call it on
// the main thread from a menu action or Later, never from a Do block.
func Alert(title, info string) {
	send(send(class("NSApplication"), "sharedApplication"), "activateIgnoringOtherApps:", true)
	alert := send(send(send(class("NSAlert"), "alloc"), "init"), "autorelease")
	send(alert, "setMessageText:", nsString(title))
	send(alert, "setInformativeText:", nsString(info))
	modal(func() { send(alert, "runModal") })
}

// Choose asks a question with one button per choice, brought to the front,
// and returns the index of the one clicked; -1 when it ended another way.
// Return clicks the first, Esc the last. Call it on the main thread from a
// menu action or Later, never from a Do block.
func Choose(title, info string, buttons ...string) int {
	send(send(class("NSApplication"), "sharedApplication"), "activateIgnoringOtherApps:", true)
	alert := send(send(send(class("NSAlert"), "alloc"), "init"), "autorelease")
	send(alert, "setMessageText:", nsString(title))
	send(alert, "setInformativeText:", nsString(info))
	var last objc.ID
	for _, b := range buttons {
		last = send(alert, "addButtonWithTitle:", nsString(b))
	}
	if len(buttons) > 1 {
		send(last, "setKeyEquivalent:", nsString("\x1b"))
	}
	var resp int
	modal(func() { resp = sendInt(alert, "runModal") })
	return choiceIndex(resp, len(buttons))
}

// Open opens a URL, or a file path in its default app.
func Open(target string) {
	Do(func() {
		var u objc.ID
		if len(target) > 0 && target[0] == '/' {
			u = send(class("NSURL"), "fileURLWithPath:", nsString(target))
		} else {
			u = send(class("NSURL"), "URLWithString:", nsString(target))
		}
		if u != 0 {
			send(send(class("NSWorkspace"), "sharedWorkspace"), "openURL:", u)
		}
	})
}
