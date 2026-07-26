package proxy

import (
	"testing"

	"kptv-proxy/work/types"
)

func TestStreamContentTypePrefersExplicitClassification(t *testing.T) {
	stream := &types.Stream{ContentType: types.ContentTypeSeries, Attributes: map[string]string{"group-title": "Provider News"}}
	if got := streamContentType(stream); got != "series" {
		t.Fatalf("streamContentType() = %q, want series", got)
	}
}

func TestStreamContentTypeFallsBackToLegacyGroup(t *testing.T) {
	stream := &types.Stream{Attributes: map[string]string{"group-title": "Movies"}}
	if got := streamContentType(stream); got != "vod" {
		t.Fatalf("streamContentType() = %q, want vod", got)
	}
}

func TestStreamResponseContentTypeUsesContainerExtension(t *testing.T) {
	channel := &types.Channel{Streams: []*types.Stream{{ContainerExtension: "mp4"}}}
	if got := streamResponseContentType(channel); got != "video/mp4" {
		t.Fatalf("streamResponseContentType() = %q, want video/mp4", got)
	}
	if got := streamResponseContentType(&types.Channel{Streams: []*types.Stream{{}}}); got != "video/mp2t" {
		t.Fatalf("streamResponseContentType() fallback = %q, want video/mp2t", got)
	}
}
