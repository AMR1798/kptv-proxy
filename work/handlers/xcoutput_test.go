package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"kptv-proxy/work/config"
	"kptv-proxy/work/proxy"
	"kptv-proxy/work/types"

	"github.com/gorilla/mux"
	"github.com/puzpuzpuz/xsync/v3"
)

func newXCPlayerAPITestProxy(enableVOD bool) (*proxy.StreamProxy, int) {
	const channelName = "Test Movie"
	streamID := streamIDFromName(channelName)
	channels := xsync.NewMapOf[string, *types.Channel]()
	channels.Store(channelName, &types.Channel{
		Name: channelName,
		Streams: []*types.Stream{{
			Name:               channelName,
			URL:                "http://upstream.example/movie/provider/upstream-secret/99.mkv",
			Source:             &config.SourceConfig{URL: "http://upstream.example", Username: "provider", Password: "upstream-secret"},
			ContentType:        types.ContentTypeVOD,
			ContainerExtension: "mkv",
			Attributes: map[string]string{
				"group-title": "Movies",
				"category-id": "7",
				"tvg-logo":    "https://images.example/movie.jpg",
			},
		}},
	})
	return &proxy.StreamProxy{
		Config: &config.Config{
			BaseURL: "http://proxy.example",
			XCOutputAccounts: []config.XCOutputAccount{{
				Username:       "viewer",
				Password:       "sec#ret",
				MaxConnections: 1,
				EnableVOD:      enableVOD,
			}},
		},
		Channels: channels,
	}, streamID
}

func requestXCPlayerAPI(t *testing.T, sp *proxy.StreamProxy, query string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/player_api.php?username=viewer&password="+url.QueryEscape("sec#ret")+"&"+query, nil)
	response := httptest.NewRecorder()
	HandleXCPlayerAPI(sp).ServeHTTP(response, req)
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding response %q: %v", response.Body.String(), err)
	}
	return response.Code, body
}

func TestGetChannelContentTypePrefersExplicitType(t *testing.T) {
	channel := &types.Channel{Streams: []*types.Stream{{ContentType: types.ContentTypeVOD, Attributes: map[string]string{"group-title": "Provider News"}}}}
	if got := getChannelContentType(channel); got != "vod" {
		t.Fatalf("getChannelContentType() = %q, want vod", got)
	}
}

func TestBuildXCStreamURLUsesVODExtensionAndPath(t *testing.T) {
	got := buildXCStreamURL("http://proxy", "vod", "u", "sec#ret", 42, ".MKV")
	if got != "http://proxy/movie/u/sec%23ret/42.mkv" {
		t.Fatalf("buildXCStreamURL() = %q, want VOD path with normalized extension", got)
	}
	if got := buildXCStreamURL("http://proxy", "live", "u", "p", 42, "mp4"); got != "http://proxy/live/u/p/42.ts" {
		t.Fatalf("buildXCStreamURL() live = %q, want ts", got)
	}
}

func TestHandleXCPlayerAPIGetVODInfo(t *testing.T) {
	sp, streamID := newXCPlayerAPITestProxy(true)
	status, body := requestXCPlayerAPI(t, sp, fmt.Sprintf("action=get_vod_info&vod_id=%d", streamID))

	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if _, exists := body["user_info"]; exists {
		t.Fatalf("get_vod_info returned account metadata: %#v", body)
	}
	info, ok := body["info"].(map[string]any)
	if !ok || info["name"] != "Test Movie" || info["movie_image"] != "https://images.example/movie.jpg" {
		t.Fatalf("info = %#v, want movie metadata", body["info"])
	}
	movieData, ok := body["movie_data"].(map[string]any)
	if !ok {
		t.Fatalf("movie_data = %#v, want object", body["movie_data"])
	}
	if movieData["stream_id"] != float64(streamID) || movieData["category_id"] != categoryIDFromName("Movies") || movieData["container_extension"] != "mkv" {
		t.Fatalf("movie_data = %#v, want matching VOD stream", movieData)
	}
	wantURL := fmt.Sprintf("http://proxy.example/movie/viewer/sec%%23ret/%d.mkv", streamID)
	if movieData["direct_source"] != wantURL {
		t.Fatalf("direct_source = %#v, want %q", movieData["direct_source"], wantURL)
	}
	encodedBody, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encodedBody), "upstream.example") || strings.Contains(string(encodedBody), "upstream-secret") {
		t.Fatalf("response leaked upstream source details: %s", encodedBody)
	}
}

func TestHandleXCPlayerAPIGetVODInfoAcceptsStreamIDAlias(t *testing.T) {
	sp, streamID := newXCPlayerAPITestProxy(true)
	_, body := requestXCPlayerAPI(t, sp, fmt.Sprintf("action=get_vod_info&stream_id=%d", streamID))
	if _, ok := body["movie_data"].(map[string]any); !ok {
		t.Fatalf("stream_id alias response = %#v, want VOD info", body)
	}
}

func TestHandleXCPlayerAPIGetVODInfoRejectsUnavailableVOD(t *testing.T) {
	tests := []struct {
		name      string
		enableVOD bool
		query     string
		nonVOD    bool
	}{
		{name: "disabled", enableVOD: false, query: "action=get_vod_info&vod_id=1"},
		{name: "missing ID", enableVOD: true, query: "action=get_vod_info"},
		{name: "malformed ID", enableVOD: true, query: "action=get_vod_info&vod_id=invalid"},
		{name: "unknown ID", enableVOD: true, query: "action=get_vod_info&vod_id=1"},
		{name: "non-VOD ID", enableVOD: true, nonVOD: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sp, _ := newXCPlayerAPITestProxy(tt.enableVOD)
			query := tt.query
			if tt.nonVOD {
				channel, _ := sp.Channels.Load("Test Movie")
				channel.Streams[0].ContentType = types.ContentTypeLive
				query = fmt.Sprintf("action=get_vod_info&vod_id=%d", streamIDFromName("Test Movie"))
			}
			status, body := requestXCPlayerAPI(t, sp, query)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want %d", status, http.StatusOK)
			}
			if len(body) != 0 {
				t.Fatalf("body = %#v, want empty object", body)
			}
		})
	}
}

func TestHandleXCStreamRedirectsM3U8LiveRequestToTS(t *testing.T) {
	const (
		username    = "viewer"
		password    = "sec#ret"
		channelName = "Test Channel"
	)
	streamID := streamIDFromName(channelName)
	channels := xsync.NewMapOf[string, *types.Channel]()
	channels.Store(channelName, &types.Channel{Name: channelName})
	sp := &proxy.StreamProxy{
		Config: &config.Config{XCOutputAccounts: []config.XCOutputAccount{{
			Username: username,
			Password: password,
		}}},
		Channels: channels,
	}

	rawID := fmt.Sprintf("%d.m3u8", streamID)
	requestPath := "/kptv/live/" + username + "/" + url.PathEscape(password) + "/" + rawID + "?token=abc"
	req := httptest.NewRequest(http.MethodGet, requestPath, nil)
	response := httptest.NewRecorder()
	router := mux.NewRouter()
	router.HandleFunc("/kptv/live/{username}/{password}/{id}", HandleXCLiveStream(sp))

	router.ServeHTTP(response, req)

	if response.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusTemporaryRedirect)
	}
	wantLocation := fmt.Sprintf("%d.ts?token=abc", streamID)
	if got := response.Header().Get("Location"); got != wantLocation {
		t.Fatalf("Location = %q, want %q", got, wantLocation)
	}
	redirectURL, err := req.URL.Parse(wantLocation)
	if err != nil {
		t.Fatalf("resolving redirect: %v", err)
	}
	wantPath := fmt.Sprintf("/kptv/live/%s/%s/%d.ts", username, url.PathEscape(password), streamID)
	if got := redirectURL.EscapedPath(); got != wantPath {
		t.Fatalf("resolved redirect path = %q, want %q", got, wantPath)
	}
}
