package utils

import (
	"net/url"
	"strings"
	"testing"

	"kptv-proxy/work/config"
	"kptv-proxy/work/types"
)

func TestEscapeM3UAttribute(t *testing.T) {
	got := EscapeM3UAttribute(`a,b "quoted" \path` + "\nnext")
	want := `a,b \"quoted\" \\path\nnext`
	if got != want {
		t.Fatalf("EscapeM3UAttribute() = %q, want %q", got, want)
	}
}

func TestSanitizeM3UDisplayName(t *testing.T) {
	if got := SanitizeM3UDisplayName("Movie\r\n#EXTINF:-1"); got != "Movie  #EXTINF:-1" {
		t.Fatalf("SanitizeM3UDisplayName() = %q, want one-line title", got)
	}
}

func TestNormalizeContainerExtension(t *testing.T) {
	tests := map[string]string{
		"mp4":      "mp4",
		".MKV":     "mkv",
		"  webm  ": "webm",
		"":         "ts",
		".":        "ts",
		"mp4/evil": "ts",
		"mp4?x=y":  "ts",
		"\u00e9":   "ts",
	}
	for input, want := range tests {
		if got := NormalizeContainerExtension(input); got != want {
			t.Errorf("NormalizeContainerExtension(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestAppendURLPathEscapesSegmentsExactlyOnce(t *testing.T) {
	segment := "space +&=%?#/雪"
	got, err := AppendURLPath("http://provider/base", "movie", segment, "42.mkv")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Path != "/base/movie/"+segment+"/42.mkv" {
		t.Fatalf("decoded path = %q", parsed.Path)
	}
	if strings.Contains(parsed.EscapedPath(), "%2525") || !strings.Contains(parsed.EscapedPath(), "%2F") {
		t.Fatalf("escaped path = %q, want one escaped segment", parsed.EscapedPath())
	}
}

func TestLogURLAlwaysRedactsXCCredentials(t *testing.T) {
	credentials := []string{"user-secret", "pass-secret"}
	urls := []string{
		"http://user-secret:pass-secret@provider/path",
		"http://provider/player_api.php?username=user-secret&password=pass-secret&action=get_live_streams",
		"http://provider/live/user-secret/pass-secret/1.ts",
		"http://proxy/s/user-secret/pass-secret/channel",
	}
	for _, rawURL := range urls {
		got := LogURL(&config.Config{}, rawURL)
		for _, credential := range credentials {
			if strings.Contains(got, credential) {
				t.Errorf("LogURL(%q) exposed credential in %q", rawURL, got)
			}
		}
	}

	encodedURL, err := AppendURLPath("http://provider", "live", "user/secret-part", "pass-secret", "1.ts")
	if err != nil {
		t.Fatal(err)
	}
	if got := LogURL(&config.Config{}, encodedURL); strings.Contains(got, "secret-part") || strings.Contains(got, "pass-secret") {
		t.Fatalf("LogURL() exposed encoded-slash credentials in %q", got)
	}
}

func TestContentTypeOfStreamPrefersExplicitAndPreservesLegacyFallbacks(t *testing.T) {
	InitContentRegexes()
	tests := []struct {
		name string
		in   *types.Stream
		want types.ContentType
	}{
		{"explicit beats category", &types.Stream{ContentType: types.ContentTypeVOD, URL: "http://provider/live/1.ts", Attributes: map[string]string{"group-title": "News"}}, types.ContentTypeVOD},
		{"legacy URL", &types.Stream{URL: "http://provider/movie/1.mp4", Attributes: map[string]string{"group-title": "News"}}, types.ContentTypeVOD},
		{"legacy tvg group", &types.Stream{URL: "http://provider/stream/1", Attributes: map[string]string{"tvg-group": "Series"}}, types.ContentTypeSeries},
		{"default", &types.Stream{URL: "http://provider/stream/1", Attributes: map[string]string{"group-title": "Provider News"}}, types.ContentTypeLive},
	}
	for _, test := range tests {
		if got := ContentTypeOfStream(test.in); got != test.want {
			t.Errorf("%s: ContentTypeOfStream() = %q, want %q", test.name, got, test.want)
		}
	}
}
