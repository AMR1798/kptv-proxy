package handlers

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"kptv-proxy/work/config"
	"kptv-proxy/work/proxy"
	"kptv-proxy/work/types"

	"github.com/gorilla/mux"
	"github.com/puzpuzpuz/xsync/v3"
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
