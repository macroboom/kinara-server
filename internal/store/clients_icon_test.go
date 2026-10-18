package store

import "testing"

func TestClientIconAndLabel(t *testing.T) {
	const chromeUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0 Safari/537.36"
	const edgeUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0 Safari/537.36 Edg/129.0"
	const safariUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_5) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Safari/605.1.15"
	const firefoxUA = "Mozilla/5.0 (X11; Linux x86_64; rv:131.0) Gecko/20100101 Firefox/131.0"
	cases := []struct {
		name  string
		c     Client
		icon  string
		label string
	}{
		{"roku tv by tcl", Client{Platform: "roku", Brand: "TCL", Model: "55S455"}, IconRoku, "Roku · TCL 55S455"},
		{"roku tv by hisense", Client{Platform: "roku", Brand: "Hisense", Model: "50R6"}, IconRoku, "Roku · Hisense 50R6"},
		{"roku player", Client{Platform: "roku", Brand: "Roku", Model: "Roku Ultra 4802X"}, IconRoku, "Roku · Ultra 4802X"},
		{"shield by brand", Client{Platform: "android-tv", Brand: "NVIDIA", Model: "SHIELD Android TV", OS: "Android 11"}, IconShield, "NVIDIA SHIELD Android TV · Android 11"},
		{"shield by model", Client{Platform: "android-tv", Model: "SHIELD Android TV"}, IconShield, "SHIELD Android TV"},
		{"chromecast with google tv", Client{Platform: "android-tv", Brand: "Google", Model: "Chromecast", OS: "Android 12"}, IconGoogleTV, "Google Chromecast · Android 12"},
		{"google tv streamer", Client{Platform: "android-tv", Brand: "google", Model: "Google TV Streamer"}, IconGoogleTV, "Google TV Streamer"},
		{"fire tv by model", Client{Platform: "android-tv", Brand: "Amazon", Model: "AFTMM", OS: "Fire OS 7"}, IconFireTV, "Amazon AFTMM · Fire OS 7"},
		{"fire tv as android", Client{Platform: "android", Model: "AFTKA"}, IconFireTV, "AFTKA"},
		{"fire tablet", Client{Platform: "android", Brand: "Amazon", Model: "KFTRWI", FormFactor: "tablet"}, IconAndroidTablet, "Amazon KFTRWI"},
		{"other android tv", Client{Platform: "android-tv", Brand: "Sony", Model: "BRAVIA 4K"}, IconAndroidTV, "Sony BRAVIA 4K"},
		{"apple tv", Client{Platform: "tvos", Brand: "Apple", Model: "Apple TV 4K", OS: "tvOS 18"}, IconAppleTV, "Apple TV 4K · tvOS 18"},
		{"iphone", Client{Platform: "ios", Brand: "Apple", Model: "iPhone 16", OS: "iOS 18"}, IconIPhone, "Apple iPhone 16 · iOS 18"},
		{"ipad", Client{Platform: "ios", Brand: "Apple", Model: "iPad Air", OS: "iPadOS 18"}, IconIPad, "Apple iPad Air · iPadOS 18"},
		{"samsung phone", Client{Platform: "android", Brand: "samsung", Model: "SM-F956U1", OS: "Android 15"}, IconAndroidPhone, "Samsung SM-F956U1 · Android 15"},
		{"android tablet", Client{Platform: "android", Brand: "samsung", Model: "SM-X710", FormFactor: "tablet", OS: "Android 14"}, IconAndroidTablet, "Samsung SM-X710 · Android 14"},
		{"web chrome", Client{Platform: "web", Model: "Chrome on Windows"}, IconWebChrome, "Chrome on Windows"},
		{"web edge", Client{Platform: "web", Model: "Edge on Windows"}, IconWebEdge, "Edge on Windows"},
		{"web safari", Client{Platform: "web", Model: "Safari on Mac"}, IconWebSafari, "Safari on Mac"},
		{"web firefox", Client{Platform: "web", Model: "Firefox on Linux"}, IconWebFirefox, "Firefox on Linux"},
		{"web unknown", Client{Platform: "web", Model: "Browser"}, IconWeb, "Browser"},
		{"web user agent chrome", Client{Platform: "web", Model: chromeUA}, IconWebChrome, chromeUA},
		{"old session chrome", Client{Name: chromeUA}, IconWebChrome, "Chrome on Windows"},
		{"old session edge", Client{Name: edgeUA}, IconWebEdge, "Edge on Windows"},
		{"old session safari", Client{Name: safariUA}, IconWebSafari, "Safari on Mac"},
		{"old session firefox", Client{Name: firefoxUA}, IconWebFirefox, "Firefox on Linux"},
		{"old session named", Client{Name: "Living room TV"}, IconWeb, ""},
		{"unknown tv", Client{FormFactor: "tv"}, IconTV, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClientIcon(tc.c); got != tc.icon {
				t.Errorf("icon = %q, want %q", got, tc.icon)
			}
			if got := ClientLabel(tc.c); got != tc.label {
				t.Errorf("label = %q, want %q", got, tc.label)
			}
		})
	}
}

func TestSessionOnlyClient(t *testing.T) {
	c := sessionOnlyClient("s-abc", "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1")
	if c.Platform != "web" || c.Model != "Safari on iPhone" || c.Name != "Safari on iPhone" || c.Icon != IconWebSafari {
		t.Fatalf("%+v", c)
	}
	if got := SessionOfDerivedClient(DerivedClientID("0123456789abcdef")); got != "0123456789ab" {
		t.Fatalf("derived = %q", got)
	}
	if SessionOfDerivedClient("phone-install-1") != "" || !ValidClientID("c2f1a9b0") || ValidClientID("short") || ValidClientID("has space!") {
		t.Fatal("client ID checks")
	}
}
