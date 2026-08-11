package parser

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kptv-proxy/work/cache"
	"kptv-proxy/work/client"
	"kptv-proxy/work/config"
	"kptv-proxy/work/types"
)

func TestXCDetailTypesDecodePartialMetadataAndMalformedEpisodes(t *testing.T) {
	var vod XCVODDetail
	if err := json.Unmarshal([]byte(`{"info":{"name":"Movie"},"movie_data":{"stream_id":33,"container_extension":".MKV"}}`), &vod); err != nil {
		t.Fatal(err)
	}
	if vod.MovieData.StreamID != "33" || vod.MovieData.ContainerExtension != ".MKV" || vod.Info["name"] != "Movie" {
		t.Fatalf("VOD detail = %#v", vod)
	}

	var series XCSeriesDetail
	payload := `{"info":{"name":"Show"},"seasons":[{"season_number":"1"},{"season_number":2}],"episodes":{"1":[{"id":"101","episode_num":1,"title":"One","container_extension":".MP4"},{"id":false},{"title":"missing id"}],"2":[{"id":202,"episode_num":"2","title":"Two"}]}}`
	if err := json.Unmarshal([]byte(payload), &series); err != nil {
		t.Fatal(err)
	}
	if len(series.Seasons) != 2 || len(series.Episodes["1"]) != 1 || len(series.Episodes["2"]) != 1 {
		t.Fatalf("series detail = %#v, want two seasons and malformed entries skipped", series)
	}
	if series.Episodes["2"][0].ID != "202" || series.Episodes["2"][0].EpisodeNum != "2" {
		t.Fatalf("numeric episode fields were not normalized: %#v", series.Episodes["2"][0])
	}
	if err := decodeXCDetail([]byte(`{"error":"not available"}`), &XCVODDetail{}); err == nil {
		t.Fatal("object-shaped provider error decoded as valid detail")
	}
	if err := decodeXCDetail([]byte(`{"info":{}`), &XCVODDetail{}); err == nil {
		t.Fatal("truncated detail decoded as valid detail")
	}
}

func TestFetchXCDetailsCoalescesAndCachesValidEmptyButNotErrors(t *testing.T) {
	var calls atomic.Int32
	mode := atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(25 * time.Millisecond)
		switch mode.Load() {
		case 0:
			_, _ = w.Write([]byte(`{"info":{},"movie_data":{"stream_id":"7","container_extension":"mp4"}}`))
		case 1:
			w.WriteHeader(http.StatusBadGateway)
		case 2:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer server.Close()
	xcCache, err := cache.NewCache(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer xcCache.Close()
	source := &config.SourceConfig{URL: server.URL, Username: "u", Password: "p", MaxConnections: 1}
	httpClient := client.NewHeaderSettingClient(time.Second)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := FetchXCVODDetail(context.Background(), httpClient, &config.Config{}, source, xcCache, "generation-a", "7"); err != nil {
				t.Errorf("FetchXCVODDetail() error = %v", err)
			}
		}()
	}
	wg.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("concurrent detail calls = %d, want one coalesced upstream call", got)
	}

	mode.Store(1)
	if _, err := FetchXCVODDetail(context.Background(), httpClient, &config.Config{}, source, xcCache, "generation-b", "8"); err == nil {
		t.Fatal("HTTP error succeeded")
	}
	if _, err := FetchXCVODDetail(context.Background(), httpClient, &config.Config{}, source, xcCache, "generation-b", "8"); err == nil {
		t.Fatal("HTTP error was cached")
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("calls after two errors = %d, want 3", got)
	}

	mode.Store(2)
	if _, err := FetchXCVODDetail(context.Background(), httpClient, &config.Config{}, source, xcCache, "generation-c", "9"); err != nil {
		t.Fatal(err)
	}
	if _, err := FetchXCVODDetail(context.Background(), httpClient, &config.Config{}, source, xcCache, "generation-c", "9"); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 4 {
		t.Fatalf("valid-empty detail calls = %d, want cached second call", got)
	}
}

func TestFetchXCDetailBoundsSourceConcurrencyAndHonorsDeadline(t *testing.T) {
	var active atomic.Int32
	var maxActive atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			maximum := maxActive.Load()
			if current <= maximum || maxActive.CompareAndSwap(maximum, current) {
				break
			}
		}
		if r.URL.Query().Get("vod_id") == "timeout" {
			<-r.Context().Done()
			return
		}
		time.Sleep(20 * time.Millisecond)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	xcCache, err := cache.NewCache(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer xcCache.Close()
	source := &config.SourceConfig{URL: server.URL, Username: "u", Password: "p", MaxConnections: 1}
	httpClient := client.NewHeaderSettingClient(time.Second)

	var wg sync.WaitGroup
	for _, id := range []string{"1", "2"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := FetchXCVODDetail(context.Background(), httpClient, &config.Config{}, source, xcCache, "bounded", id); err != nil {
				t.Errorf("FetchXCVODDetail() error = %v", err)
			}
		}()
	}
	wg.Wait()
	if got := maxActive.Load(); got != 1 {
		t.Fatalf("maximum per-source concurrency = %d, want 1", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := FetchXCVODDetail(ctx, httpClient, &config.Config{}, source, xcCache, "deadline", "timeout"); err == nil {
		t.Fatal("detail request ignored caller deadline")
	}
}

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

func TestXCProviderIDsAcceptStringAndNumber(t *testing.T) {
	var value struct {
		Live   XCLiveStream `json:"live"`
		Series XCSeries     `json:"series"`
		VOD    XCVODStream  `json:"vod"`
	}
	if err := json.Unmarshal([]byte(`{"live":{"stream_id":"7"},"series":{"series_id":8},"vod":{"stream_id":"9"}}`), &value); err != nil {
		t.Fatal(err)
	}
	if value.Live.StreamID != "7" || value.Series.SeriesID != "8" || value.VOD.StreamID != "9" {
		t.Fatalf("provider IDs = %q, %q, %q, want 7, 8, 9", value.Live.StreamID, value.Series.SeriesID, value.VOD.StreamID)
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
	live := processLiveBatchWorker([]XCLiveStream{{StreamID: "1", Name: "Live", CategoryID: "10"}}, map[string]string{"10": "Provider News"}, source)
	series := processSeriesBatchWorker([]XCSeries{{SeriesID: "2", Name: "Series", CategoryID: "20"}}, map[string]string{"20": "Provider Shows"}, source)
	vod := processVODBatchWorker([]XCVODStream{{StreamID: "3", Name: "Movie", CategoryID: "30", ContainerExtension: ".MP4"}}, map[string]string{"30": "Provider Movies"}, source)

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

func TestProcessXCBatchesRetainsProviderIdentity(t *testing.T) {
	source := &config.SourceConfig{URL: "http://provider", Username: "u", Password: "p"}
	streams := processVODBatchWorker([]XCVODStream{{StreamID: "3", Name: "Movie", CategoryID: "30", ContainerExtension: "mp4"}}, nil, source)
	if len(streams) != 1 {
		t.Fatalf("got %d streams, want 1", len(streams))
	}
	if streams[0].ProviderID != "3" || streams[0].ProviderSource != source.URL || streams[0].ContentType != types.ContentTypeVOD {
		t.Fatalf("stream identity = %#v, want provider id 3 and source %q", streams[0], source.URL)
	}
}

func TestProcessXCBatchesEscapeProviderPathCredentialsAndPreserveExtensions(t *testing.T) {
	username := "user +&=%?#/雪"
	password := "pass +&=%?#/雪"
	source := &config.SourceConfig{URL: "http://provider/base", Username: username, Password: password}
	streams := []*types.Stream{
		processLiveBatchWorker([]XCLiveStream{{StreamID: "1", Name: "Live"}}, nil, source)[0],
		processSeriesBatchWorker([]XCSeries{{SeriesID: "2", Name: "Series"}}, nil, source)[0],
		processVODBatchWorker([]XCVODStream{{StreamID: "3", Name: "Movie", ContainerExtension: ".MKV"}}, nil, source)[0],
	}
	wantSuffixes := []string{"/1.ts", "/2.ts", "/3.mkv"}
	for i, stream := range streams {
		parsed, err := url.Parse(stream.URL)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(parsed.EscapedPath(), "%2F") || strings.Contains(parsed.EscapedPath(), "%2525") || !strings.HasSuffix(parsed.EscapedPath(), wantSuffixes[i]) {
			t.Errorf("stream URL path = %q, want escaped credentials and suffix %q", parsed.EscapedPath(), wantSuffixes[i])
		}
	}
}

func TestProcessXCBatchesPreservesProviderOrder(t *testing.T) {
	items := make([]XCLiveStream, 2001)
	for i := range items {
		items[i] = XCLiveStream{StreamID: XCID(fmt.Sprintf("%d", i+1)), Name: fmt.Sprintf("Channel %04d", i)}
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
	streams, complete := ParseXtremeCodesAPIWithStatus(client.NewHeaderSettingClient(time.Second), &config.Config{WorkerThreads: 1}, &config.SourceConfig{URL: server.URL, Username: "u", Password: "p"}, nil, xcCache)
	if len(streams) != 1 || streams[0].Attributes["group-title"] != "vod" || !complete {
		t.Fatalf("fallback VOD category = %#v, complete=%t; want group-title vod and publishable catalog", streams, complete)
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
	if got, complete := ParseXtremeCodesAPIWithStatus(httpClient, cfg, source, nil, xcCache); len(got) != 0 || complete {
		t.Fatalf("first partial parse returned %d streams with complete=%t, want 0 and false", len(got), complete)
	}
	if got, complete := ParseXtremeCodesAPIWithStatus(httpClient, cfg, source, nil, xcCache); len(got) != 0 || !complete {
		t.Fatalf("second empty parse returned %d streams with complete=%t, want 0 and true", len(got), complete)
	}
	mu.Lock()
	defer mu.Unlock()
	if counts["get_live_streams"] != 2 {
		t.Fatalf("live stream endpoint called %d times, want 2 after uncached partial fetch", counts["get_live_streams"])
	}
}

func TestParseXtremeCodesAPIEncodesProviderQueryCredentials(t *testing.T) {
	username := "user +&=%?#/雪"
	password := "pass +&=%?#/雪"
	var mu sync.Mutex
	seen := make(map[string]bool)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("username"); got != username {
			t.Errorf("username = %q, want %q", got, username)
		}
		if got := r.URL.Query().Get("password"); got != password {
			t.Errorf("password = %q, want %q", got, password)
		}
		action := r.URL.Query().Get("action")
		mu.Lock()
		seen[action] = true
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()

	xcCache, err := cache.NewCache(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer xcCache.Close()
	_, complete := ParseXtremeCodesAPIWithStatus(
		client.NewHeaderSettingClient(time.Second),
		&config.Config{WorkerThreads: 1},
		&config.SourceConfig{URL: server.URL, Username: username, Password: password},
		nil,
		xcCache,
	)
	if !complete {
		t.Fatal("ParseXtremeCodesAPIWithStatus() incomplete, want all encoded requests accepted")
	}
	for _, action := range []string{"get_live_categories", "get_series_categories", "get_vod_categories", "get_live_streams", "get_series", "get_vod_streams"} {
		if !seen[action] {
			t.Errorf("action %q was not requested", action)
		}
	}
}

func TestXCCacheKeyDoesNotContainCredentials(t *testing.T) {
	source := &config.SourceConfig{URL: "http://provider", Username: "user-secret", Password: "pass-secret"}
	key := xcCacheKey(source)
	if strings.Contains(key, source.Username) || strings.Contains(key, source.Password) {
		t.Fatalf("cache key contains credentials: %q", key)
	}
}
