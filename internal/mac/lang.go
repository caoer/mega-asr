//go:build darwin

package mac

// PreferredLanguage is the first of the user's preferred languages
// (System Settings › General › Language & Region), a tag such as en-US or
// zh-Hans-CN; "" when there is none.
func PreferredLanguage() string {
	var tag string
	WithPool(func() {
		langs := send(class("NSLocale"), "preferredLanguages")
		if langs == 0 || sendInt(langs, "count") == 0 {
			return
		}
		tag = goString(send(langs, "firstObject"))
	})
	return tag
}
