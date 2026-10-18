package store

import (
	"strings"
	"unicode"
)

// Icon keys the apps draw for a device. The server picks one so every app
// shows the same picture for the same device.
const (
	IconRoku          = "roku"
	IconShield        = "shield"
	IconGoogleTV      = "google-tv"
	IconFireTV        = "fire-tv"
	IconAndroidTV     = "android-tv"
	IconAppleTV       = "apple-tv"
	IconTV            = "tv"
	IconIPhone        = "iphone"
	IconIPad          = "ipad"
	IconAndroidPhone  = "android-phone"
	IconAndroidTablet = "android-tablet"
	IconWebChrome     = "web-chrome"
	IconWebFirefox    = "web-firefox"
	IconWebSafari     = "web-safari"
	IconWebEdge       = "web-edge"
	IconWeb           = "web"
)

// ClientIcon picks the icon for a device from what its app reported.
func ClientIcon(c Client) string {
	platform := strings.ToLower(c.Platform)
	brand := strings.ToLower(c.Brand)
	model := strings.ToLower(c.Model)
	tablet := strings.EqualFold(c.FormFactor, "tablet")
	switch {
	// Roku TVs made by TCL, Hisense and others are still Rokus; their maker
	// shows in the label.
	case platform == "roku":
		return IconRoku
	case brand == "nvidia" || strings.Contains(model, "shield"):
		return IconShield
	case strings.Contains(model, "chromecast") || (brand == "google" && platform == "android-tv"):
		return IconGoogleTV
	// Fire tablets are Amazon too (models "KF..."), and stay tablets.
	case strings.HasPrefix(model, "aft") || (brand == "amazon" && !tablet && !strings.HasPrefix(model, "kf")):
		return IconFireTV
	case platform == "android-tv":
		return IconAndroidTV
	case platform == "tvos":
		return IconAppleTV
	case platform == "ios":
		if tablet || strings.Contains(model, "ipad") {
			return IconIPad
		}
		return IconIPhone
	case platform == "android":
		if tablet {
			return IconAndroidTablet
		}
		return IconAndroidPhone
	case platform == "web":
		return browserIcon(c.Model)
	case strings.EqualFold(c.FormFactor, "tv"):
		return IconTV
	}
	// Apps that say nothing: perhaps a browser's user agent as the name.
	if b, _ := BrowserFromUserAgent(c.Name); b != "" {
		return browserIcon(b)
	}
	return IconWeb
}

func browserIcon(model string) string {
	b := strings.ToLower(model)
	if guess, _ := BrowserFromUserAgent(model); guess != "" {
		b = strings.ToLower(guess)
	}
	switch {
	case strings.HasPrefix(b, "edge"):
		return IconWebEdge
	case strings.HasPrefix(b, "firefox"):
		return IconWebFirefox
	case strings.HasPrefix(b, "chrome"):
		return IconWebChrome
	case strings.HasPrefix(b, "safari"):
		return IconWebSafari
	}
	return IconWeb
}

// BrowserFromUserAgent names the browser and system in a user agent (or in
// a short "Chrome on Windows" the web app sends), or "" when it isn't one.
func BrowserFromUserAgent(ua string) (browser, osName string) {
	if before, after, ok := strings.Cut(ua, " on "); ok && !strings.Contains(ua, "/") {
		if b, _ := knownBrowser(before); b != "" {
			return b, strings.TrimSpace(after)
		}
	}
	if !strings.Contains(ua, "Mozilla/") && !strings.Contains(ua, "AppleWebKit") {
		if b, _ := knownBrowser(ua); b != "" && !strings.Contains(ua, " ") {
			return b, ""
		}
		return "", ""
	}
	browser, _ = knownBrowser(ua)
	if browser == "" {
		browser = "Browser"
	}
	switch {
	case strings.Contains(ua, "iPad"):
		osName = "iPad"
	case strings.Contains(ua, "iPhone"):
		osName = "iPhone"
	case strings.Contains(ua, "Android"):
		osName = "Android"
	case strings.Contains(ua, "Windows"):
		osName = "Windows"
	case strings.Contains(ua, "CrOS"):
		osName = "ChromeOS"
	case strings.Contains(ua, "Mac OS X") || strings.Contains(ua, "Macintosh"):
		osName = "Mac"
	case strings.Contains(ua, "Linux"):
		osName = "Linux"
	}
	return browser, osName
}

// knownBrowser finds a browser name in text. Order matters: Edge's agent
// also says Chrome, and Chrome's also says Safari.
func knownBrowser(s string) (string, bool) {
	l := strings.ToLower(s)
	switch {
	case strings.Contains(l, "edg"):
		return "Edge", true
	case strings.Contains(l, "firefox") || strings.Contains(l, "fxios"):
		return "Firefox", true
	case strings.Contains(l, "chrome") || strings.Contains(l, "crios") || strings.Contains(l, "chromium"):
		return "Chrome", true
	case strings.Contains(l, "safari"):
		return "Safari", true
	}
	return "", false
}

// ClientLabel is a short description of the device for the owner, such as
// "Roku · TCL 55S455" or "Samsung SM-F956U1 · Android 15". The device's own
// name is shown separately.
func ClientLabel(c Client) string {
	platform := strings.ToLower(c.Platform)
	switch platform {
	case "roku":
		model := strings.TrimSpace(c.Model)
		if m, ok := cutPrefixFold(model, "roku "); ok {
			model = m
		}
		parts := []string{}
		if c.Brand != "" && !strings.EqualFold(c.Brand, "roku") {
			parts = append(parts, c.Brand)
		}
		if model != "" {
			parts = append(parts, model)
		}
		if len(parts) == 0 {
			return "Roku"
		}
		return "Roku · " + strings.Join(parts, " ")
	case "web":
		label := c.Model
		if label == "" {
			label = "Web browser"
		}
		if c.OS != "" && !strings.Contains(strings.ToLower(label), strings.ToLower(c.OS)) {
			label += " · " + c.OS
		}
		return label
	}
	device := strings.TrimSpace(c.Model)
	brand := capitalize(strings.TrimSpace(c.Brand))
	if brand != "" {
		if _, ok := cutPrefixFold(device, brand); !ok {
			device = strings.TrimSpace(brand + " " + device)
		}
	}
	parts := []string{}
	if device != "" {
		parts = append(parts, device)
	}
	if c.OS != "" {
		parts = append(parts, c.OS)
	}
	if len(parts) == 0 {
		// A session from an app that said nothing: a user agent, perhaps.
		if b, osName := BrowserFromUserAgent(c.Name); b != "" {
			if osName != "" {
				return b + " on " + osName
			}
			return b
		}
		return ""
	}
	return strings.Join(parts, " · ")
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):], true
	}
	return s, false
}

// capitalize turns "samsung" into "Samsung" and leaves "NVIDIA" and "TCL" be.
func capitalize(s string) string {
	if s == "" || strings.ToLower(s) != s {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}
