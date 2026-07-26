package filter

import (
	"testing"

	"kptv-proxy/work/config"
	"kptv-proxy/work/types"
	"kptv-proxy/work/utils"
)

func TestFilterStreamsUsesExplicitContentType(t *testing.T) {
	utils.InitContentRegexes()
	source := &config.SourceConfig{Name: "xc", URL: "http://provider", VODIncludeRegex: "movie"}
	streams := []*types.Stream{
		{Name: "movie one", URL: "http://provider/live/1.ts", ContentType: types.ContentTypeVOD, Attributes: map[string]string{"group-title": "News"}, Source: source},
		{Name: "other", URL: "http://provider/live/2.ts", ContentType: types.ContentTypeVOD, Attributes: map[string]string{"group-title": "News"}, Source: source},
	}

	filtered := FilterStreams(streams, source, NewFilterManager())
	if len(filtered) != 1 || filtered[0].Name != "movie one" {
		t.Fatalf("FilterStreams() kept %#v, want only movie one", filtered)
	}
}

func TestFilterManagerRecompilesWhenPatternsChange(t *testing.T) {
	source := &config.SourceConfig{URL: "http://provider", LiveIncludeRegex: "one"}
	manager := NewFilterManager()
	first := manager.GetOrCreateFilter(source)
	if first.LiveInclude == nil || !first.LiveInclude.MatchString("one") {
		t.Fatal("initial live filter was not compiled")
	}

	source.LiveIncludeRegex = "two"
	second := manager.GetOrCreateFilter(source)
	if second == first || second.LiveInclude == nil || !second.LiveInclude.MatchString("two") {
		t.Fatal("changed source pattern did not produce a new compiled filter")
	}
}
