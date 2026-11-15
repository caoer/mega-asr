package refine

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// Front is the application in front as a take starts.
type Front struct {
	Bundle string // its bundle id
	Name   string
	Title  string // its front window's title
	PID    int32
	Window uint32 // its front window, as the machine numbers it
	// Secure says what is typed there is secret: the keyboard is in a
	// password field, or the system's secure keyboard input is on.
	Secure bool
}

// Shot is the picture of a window, held until Release.
type Shot interface {
	Size() (w, h int)
	// Text is the window's text, read on this machine.
	Text() (string, error)
	// JPEG is the picture, its long edge at most edge pixels.
	JPEG(edge int) (data []byte, w, h int, err error)
	Release()
}

// Display is the machine's side of the screen source.
type Display interface {
	// Access reports the Screen Recording grant, without asking.
	Access() bool
	// Request asks for the grant: the first time the system prompts and
	// lists the app in its settings. It reports the grant as it is now.
	Request() bool
	Front() (Front, error)
	// Capture takes the picture of f's front window.
	Capture(f Front) (Shot, error)
}

// ScreenSource is the model step's screen source: the window in front of
// the speaker, as its text or as a picture.
type ScreenSource struct {
	Mode    string   // ScreenOCR or ScreenImage
	Exclude []string // bundle ids of applications whose windows are never captured
	Edge    int      // the picture's long edge in pixels; 0 is ScreenEdge
	Display Display
}

// The forms of the screen source (refine.context.screen).
const (
	ScreenOCR   = "ocr"   // the window's text
	ScreenImage = "image" // the window's picture
)

// ScreenEdge is the long edge of the picture sent, in pixels.
const ScreenEdge = 1568

// maxScreenText is the most of a window's text that is sent, in characters:
// the end of it, where a terminal and a chat hold what is newest.
const maxScreenText = 12000

// Why a take's screen was not sent (Screen.Withheld).
const (
	WithheldPermission = "permission" // the Screen Recording grant is off
	WithheldSecure     = "secure"     // the keyboard is in a password field
	WithheldExcluded   = "excluded"   // the application is on the exclude list
	WithheldEmpty      = "empty"      // the window holds no text
	WithheldLate       = "late"       // the capture was not ready when the step ran
	WithheldError      = "error"      // no window, or the capture failed
)

// Screen is what the screen source got for one take.
type Screen struct {
	Mode  string
	App   string // the application in front, by bundle id
	Name  string
	Title string
	Text  string // ScreenOCR: the window's text as sent
	Image *Image // ScreenImage: the picture as sent
	W, H  int    // the capture, in pixels
	// What each part took: the picture, reading its text, making the JPEG.
	Capture, Recognize, Encode time.Duration
	Withheld                   string // why nothing of the screen is sent; "" when Text or Image is
	Err                        error
}

// Take captures the window in front. Nothing is captured without the
// grant, from an excluded application, or while what is typed is secret.
func (s ScreenSource) Take() *Screen {
	sc := &Screen{Mode: s.Mode}
	withhold := func(why string, err error) *Screen {
		sc.Withheld, sc.Err = why, err
		return sc
	}
	d := s.Display
	if !d.Access() && !d.Request() {
		return withhold(WithheldPermission, nil)
	}
	f, err := d.Front()
	if err != nil {
		return withhold(WithheldError, err)
	}
	sc.App, sc.Name = f.Bundle, f.Name
	if slices.Contains(s.Exclude, f.Bundle) {
		return withhold(WithheldExcluded, nil)
	}
	if f.Secure {
		return withhold(WithheldSecure, nil)
	}
	sc.Title = f.Title
	t0 := time.Now()
	shot, err := d.Capture(f)
	sc.Capture = time.Since(t0)
	if err != nil {
		return withhold(WithheldError, err)
	}
	defer shot.Release()
	sc.W, sc.H = shot.Size()
	t0 = time.Now()
	if s.Mode == ScreenImage {
		edge := s.Edge
		if edge == 0 {
			edge = ScreenEdge
		}
		data, w, h, err := shot.JPEG(edge)
		sc.Encode = time.Since(t0)
		if err != nil {
			return withhold(WithheldError, err)
		}
		sc.Image = &Image{MediaType: "image/jpeg", Data: data, W: w, H: h}
		return sc
	}
	text, err := shot.Text()
	sc.Recognize = time.Since(t0)
	if err != nil {
		return withhold(WithheldError, err)
	}
	if text = strings.TrimSpace(text); text == "" {
		return withhold(WithheldEmpty, nil)
	}
	if r := []rune(text); len(r) > maxScreenText {
		text = string(r[len(r)-maxScreenText:])
	}
	sc.Text = text
	return sc
}

// Context is the part of a call's <context> the screen gives: the window's
// text under a line naming the window, or for a picture that line alone;
// "" when the screen is withheld.
func (sc *Screen) Context() string {
	if sc == nil || sc.Withheld != "" {
		return ""
	}
	window := sc.Name
	if sc.Title != "" {
		window = fmt.Sprintf("%s, %q", sc.Name, sc.Title)
	}
	if sc.Image != nil {
		return "The image is the window in front of the speaker: " + window + "."
	}
	return "Text of the window in front of the speaker (" + window + "), as read off the screen:\n" + sc.Text
}
