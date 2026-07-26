package parser

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"kptv-proxy/work/cache"
	"kptv-proxy/work/client"
	"kptv-proxy/work/config"
	"kptv-proxy/work/types"
)

func TestXCIDAcceptsStringAndNumber(t *testing.T) {
	var value struct {
		StringID XCID `json:"string_id"`
		NumberID XCID `json:"number_id"`
	}
	if err := json.Unmarshal([]byte(`{"string_id":"7","number_id":8}`), &value); err != nil {
		t.Fatal(err)
	}
	if value.StringID != "7" || value.NumberID != "8" {
		t.Fatalf("decoded IDs = %q and %q, want 7 and 8", value.StringID, value.NumberID)
	}
}

func TestBuildCategoryMapUsesFlatDisplayNames(t *testing.T) {
	got := buildCategoryMap([]XCCategory{
		{CategoryID: "1", CategoryName: "News"},
		{CategoryID: "", CategoryName: "ignored"},
		{CategoryID: "2", CategoryName: ""},
		{CategoryID: "1", CategoryName: "Renamed"},
	})
	if got["1"] != "Renamed" || len(got) != 1 {
		t.Fatalf("category map = %#v, want only final non-empty name for ID 1", got)
	}
}

func TestProcessXCBatchesWithCategoryAndVODMetadata(t *testing.T) {
	source := &config.SourceConfig{URL: "http://provider", Username: "u", Password: "p"}
	live := processLiveBatchWorker([]XCLiveStream{{StreamID: 1, Name: "Live", CategoryID: "10"}}, map[string]string{"10": "Provider News"}, source)
	series := processSeriesBatchWorker([]XCSeries{{SeriesID: 2, Name: "Series", CategoryID: "20"}}, map[string]string{"20": "Provider Shows"}, source)
	vod := processVODBatchWorker([]XCVODStream{{StreamID: 3, Name: "Movie", CategoryID: "30", ContainerExtension: ".MP4"}}, map[string]string{"30": "Provider Movies"}, source)

	if live[0].ContentType != types.ContentTypeLive || live[0].Attributes["group-title"] != "Provider News" {
		t.Fatalf("live stream metadata = %#v", live[0])
	}
	if series[0].ContentType != types.ContentTypeSeries || series[0].Attributes["group-title"] != "Provider Shows" {
		t.Fatalf("series stream metadata = %#v", series[0])
	}
	if vod[0].ContentType != types.ContentTypeVOD || vod[0].ContainerExtension != "mp4" || vod[0].URL != "http://provider/movie/u/p/3.mp4" {
		t.Fatalf("VOD stream metadata = %#v", vod[0])
	}
}

func TestProcessXCBatchesPreservesProviderOrder(t *testing.T) {
	items := make([]XCLiveStream, 2001)
	for i := range items {
		items[i] = XCLiveStream{StreamID: i + 1, Name: fmt.Sprintf("Channel %04d", i)}
	}
	source := &config.SourceConfig{URL: "http://provider"}
	streams := processXCBatches(context.Background(), items, 4, func(batch []XCLiveStream) []*types.Stream {
		return processLiveBatchWorker(batch, nil, source)
	})
	if len(streams) != len(items) {
		t.Fatalf("processXCBatches() returned %d streams, want %d", len(streams), len(items))
	}
	for i, stream := range streams {
		if stream.Name != items[i].Name {
			t.Fatalf("stream %d = %q, want %q", i, stream.Name, items[i].Name)
		}
	}
}

func TestParseXtremeCodesAPIImportsCatalogsAndCachesCompleteData(t *testing.T) {
	var mu sync.Mutex
	counts := make(map[string]int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		action := r.URL.Query().Get("action")
		mu.Lock()
		counts[action]++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch action {
		case "get_live_categories":
			_, _ = w.Write([]byte(`[{"category_id":"1","category_name":"World, News","parent_id":0}]`))
		case "get_series_categories":
			_, _ = w.Write([]byte(`[{"category_id":2,"category_name":"Drama"}]`))
		case "get_vod_categories":
			_, _ = w.Write([]byte(`[{"category_id":"3","category_name":"Movies"}]`))
		case "get_live_streams":
			_, _ = w.Write([]byte(`[{"stream_id":11,"name":"Live One","category_id":1,"stream_icon":"logo","epg_channel_id":"epg"}]`))
		case "get_series":
			_, _ = w.Write([]byte(`[{"series_id":22,"name":"Series One","category_id":"2","cover":"cover"}]`))
		case "get_vod_streams":
			_, _ = w.Write([]byte(`[{"stream_id":33,"name":"Movie One","category_id":3,"container_extension":".MP4"}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	xcCache, err := cache.NewCache(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	source := &config.SourceConfig{URL: server.URL, Username: "user", Password: "pass"}
	cfg := &config.Config{WorkerThreads: 2}
	httpClient := client.NewHeaderSettingClient(time.Second)

	streams := ParseXtremeCodesAPI(httpClient, cfg, source, nil, xcCache)
	if len(streams) != 3 {
		t.Fatalf("ParseXtremeCodesAPI() returned %d streams, want 3", len(streams))
	}
	byName := make(map[string]*types.Stream, len(streams))
	for _, stream := range streams {
		byName[stream.Name] = stream
	}
	if byName["Live One"].Attributes["group-title"] != "World, News" || byName["Live One"].ContentType != types.ContentTypeLive {
		t.Fatalf("live stream lost category metadata: %#v", byName["Live One"])
	}
	if byName["Series One"].Attributes["group-title"] != "Drama" || byName["Series One"].ContentType != types.ContentTypeSeries {
		t.Fatalf("series stream lost category metadata: %#v", byName["Series One"])
	}
	if byName["Movie One"].URL != server.URL+"/movie/user/pass/33.mp4" || byName["Movie One"].ContainerExtension != "mp4" {
		t.Fatalf("VOD stream URL/extension = %#v", byName["Movie One"])
	}

	second := ParseXtremeCodesAPI(httpClient, cfg, source, nil, xcCache)
	if len(second) != 3 {
		t.Fatalf("cached ParseXtremeCodesAPI() returned %d streams, want 3", len(second))
	}
	mu.Lock()
	defer mu.Unlock()
	for action, count := range counts {
		if count != 1 {
			t.Errorf("action %s was requested %d times, want cache hit on second parse", action, count)
		}
	}
}

func TestParseXtremeCodesAPIFallsBackWhenCategoryEndpointFails(t *testing.T) {
	var mu sync.Mutex
	counts := make(map[string]int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		action := r.URL.Query().Get("action")
		mu.Lock()
		counts[action]++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch action {
		case "get_vod_categories":
			w.WriteHeader(http.StatusBadGateway)
		case "get_vod_streams":
			_, _ = w.Write([]byte(`[{"stream_id":9,"name":"Fallback Movie","category_id":"99","container_extension":"mkv"}]`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer server.Close()
	xcCache, err := cache.NewCache(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	streams := ParseXtremeCodesAPI(client.NewHeaderSettingClient(time.Second), &config.Config{WorkerThreads: 1}, &config.SourceConfig{URL: server.URL, Username: "u", Password: "p"}, nil, xcCache)
	if len(streams) != 1 || streams[0].Attributes["group-title"] != "vod" {
		t.Fatalf("fallback VOD category = %#v, want group-title vod", streams)
	}
	streams = ParseXtremeCodesAPI(client.NewHeaderSettingClient(time.Second), &config.Config{WorkerThreads: 1}, &config.SourceConfig{URL: server.URL, Username: "u", Password: "p"}, nil, xcCache)
	if len(streams) != 1 || streams[0].Attributes["group-title"] != "vod" {
		t.Fatalf("second fallback VOD category = %#v, want group-title vod", streams)
	}
	mu.Lock()
	defer mu.Unlock()
	if counts["get_vod_categories"] != 2 {
		t.Fatalf("failed category endpoint was called %d times, want retry after uncached fallback", counts["get_vod_categories"])
	}
}

func TestParseXtremeCodesAPIDoesNotCachePartialStreamFetch(t *testing.T) {
	var mu sync.Mutex
	counts := make(map[string]int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		action := r.URL.Query().Get("action")
		mu.Lock()
		counts[action]++
		call := counts[action]
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if action == "get_live_streams" && call == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()
	xcCache, err := cache.NewCache(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{WorkerThreads: 1}
	source := &config.SourceConfig{URL: server.URL, Username: "u", Password: "p"}
	httpClient := client.NewHeaderSettingClient(time.Second)
	if got := ParseXtremeCodesAPI(httpClient, cfg, source, nil, xcCache); len(got) != 0 {
		t.Fatalf("first partial parse returned %d streams, want 0", len(got))
	}
	if got := ParseXtremeCodesAPI(httpClient, cfg, source, nil, xcCache); len(got) != 0 {
		t.Fatalf("second empty parse returned %d streams, want 0", len(got))
	}
	mu.Lock()
	defer mu.Unlock()
	if counts["get_live_streams"] != 2 {
		t.Fatalf("live stream endpoint called %d times, want 2 after uncached partial fetch", counts["get_live_streams"])
	}
}
