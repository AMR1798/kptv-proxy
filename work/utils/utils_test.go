package utils

import (
	"testing"

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
