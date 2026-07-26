package handlers

import (
	"testing"

	"kptv-proxy/work/types"
)

func TestGetChannelContentTypePrefersExplicitType(t *testing.T) {
	channel := &types.Channel{Streams: []*types.Stream{{ContentType: types.ContentTypeVOD, Attributes: map[string]string{"group-title": "Provider News"}}}}
	if got := getChannelContentType(channel); got != "vod" {
		t.Fatalf("getChannelContentType() = %q, want vod", got)
	}
}

func TestBuildXCStreamURLUsesVODExtensionAndPath(t *testing.T) {
	got := buildXCStreamURL("http://proxy", "vod", "u", "p", 42, ".MKV")
	if got != "http://proxy/movie/u/p/42.mkv" {
		t.Fatalf("buildXCStreamURL() = %q, want VOD path with normalized extension", got)
	}
	if got := buildXCStreamURL("http://proxy", "live", "u", "p", 42, "mp4"); got != "http://proxy/live/u/p/42.ts" {
		t.Fatalf("buildXCStreamURL() live = %q, want ts", got)
	}
}
