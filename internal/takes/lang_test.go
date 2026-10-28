package takes

import (
	"net/http"
	"strings"
	"testing"

	"github.com/caoer/mega-asr/internal/l10n"
)

// lang.js sets the pages' language to the app's: Chinese for a dev build
// and a Chinese Mac, English otherwise.
func TestLangJS(t *testing.T) {
	defer l10n.Set(l10n.Current())
	h := New(t.TempDir(), nil, func() []Engine { return nil })
	for l, want := range map[l10n.Lang]string{l10n.Orig: `"zh-CN"`, l10n.ZH: `"zh-CN"`, l10n.EN: `"en"`} {
		l10n.Set(l)
		w := request(h, http.MethodGet, "/takes/lang.js", nil)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "document.documentElement.lang = "+want) {
			t.Errorf("lang %v: %d %q", l, w.Code, w.Body.String())
		}
	}
}
