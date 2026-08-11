package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"kptv-proxy/work/cache"
	"kptv-proxy/work/client"
	"kptv-proxy/work/config"
	"kptv-proxy/work/constants"
	"kptv-proxy/work/db"
	"kptv-proxy/work/parser"
	"kptv-proxy/work/proxy"
	"kptv-proxy/work/types"
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "kptv-handlers-test-")
	if err != nil {
		panic(err)
	}
	constants.Internal.DatabasePath = filepath.Join(dir, "kptv.db")
	code := m.Run()
	db.Close()
	_ = os.RemoveAll(dir)
	os.Exit(code)
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

func TestBuildXCStreamURLEscapesEachPathSegmentOnce(t *testing.T) {
	username := "user +&=%?#/雪"
	password := "pass +&=%?#/雪"
	got := buildXCStreamURL("http://proxy/base", "vod", username, password, 42, ".MKV")

	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Path != "/base/movie/"+username+"/"+password+"/42.mkv" {
		t.Fatalf("decoded path = %q, want credentials as path segments", parsed.Path)
	}
	if strings.Contains(parsed.EscapedPath(), "%2525") {
		t.Fatalf("escaped path was double encoded: %q", parsed.EscapedPath())
	}
	if !strings.Contains(parsed.EscapedPath(), "%2F") || !strings.HasSuffix(parsed.EscapedPath(), "/42.mkv") {
		t.Fatalf("escaped path = %q, want escaped credential slash and preserved extension", parsed.EscapedPath())
	}
}

func TestNormalizeXCRequestParametersUsesFormPrecedenceAndParsesSupportedValues(t *testing.T) {
	query := url.Values{
		"username": {"query-user"}, "password": {"query-pass"}, "action": {"query-action"},
		"category_id": {"query-category"}, "stream_id": {"query-stream"},
	}
	form := url.Values{
		"username": {"form-user"}, "password": {"form-pass"}, "action": {"form-action"},
		"category_id": {"form-category"}, "vod_id": {"vod"}, "series_id": {"series"},
		"stream_id": {"stream"}, "type": {"m3u_plus"}, "limit": {"25"},
	}
	req := httptest.NewRequest(http.MethodPost, "/player_api.php?"+query.Encode(), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")

	params, err := normalizeXCRequestParameters(req)
	if err != nil {
		t.Fatal(err)
	}
	if params.username != "form-user" || params.password != "form-pass" || params.action != "form-action" ||
		params.categoryID != "form-category" || params.vodID != "vod" || params.seriesID != "series" ||
		params.streamID != "stream" || params.outputType != "m3u_plus" || params.limit != 25 {
		t.Fatalf("normalized parameters = %#v", params)
	}
}

func TestNormalizeXCRequestParametersRejectsDuplicateFormAndUnboundedLimit(t *testing.T) {
	tests := []struct {
		name string
		form url.Values
	}{
		{"duplicate form", url.Values{"username": {"one", "two"}}},
		{"zero limit", url.Values{"limit": {"0"}}},
		{"excessive limit", url.Values{"limit": {"1001"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/player_api.php", strings.NewReader(test.form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if _, err := normalizeXCRequestParameters(req); err == nil {
				t.Fatal("normalizeXCRequestParameters() succeeded, want rejection")
			}
		})
	}
}

func newXCHandlerTestProxy(t *testing.T, providerURL string, account config.XCOutputAccount) *proxy.StreamProxy {
	t.Helper()
	xcCache, err := cache.NewCache(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(xcCache.Close)
	cfg := &config.Config{
		BaseURL: "http://proxy", WorkerThreads: 2, MaxConnectionsToApp: 10,
		Sources:          []config.SourceConfig{{URL: providerURL, Username: "provider-user", Password: "provider-pass", MaxConnections: 2}},
		XCOutputAccounts: []config.XCOutputAccount{account},
	}
	sp := proxy.New(cfg, nil, client.NewHeaderSettingClient(time.Second), nil, xcCache)
	sp.ImportStreams()
	return sp
}

func TestXCMetadataDoesNotConsumePlaybackLease(t *testing.T) {
	sp := proxy.New(&config.Config{BaseURL: "http://proxy", MaxConnectionsToApp: 10, XCOutputAccounts: []config.XCOutputAccount{{ID: 1, Username: "u", Password: "p", MaxConnections: 1, EnableLive: true}}}, nil, nil, nil, nil)
	handler := HandleXCPlayerAPI(sp)
	for _, action := range []string{"", "get_live_categories", "get_short_epg"} {
		response := httptest.NewRecorder()
		handler(response, httptest.NewRequest(http.MethodGet, "/player_api.php?username=u&password=p&action="+action, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("action %q status = %d", action, response.Code)
		}
	}
	account, _ := sp.AccountRegistry().Authenticate("u", "p")
	if got := account.ActiveConnections(); got != 0 {
		t.Fatalf("metadata left %d active leases", got)
	}
}

func TestLegacyAuthorizationAllowsEnabledStreamInMixedChannel(t *testing.T) {
	sp := proxy.New(&config.Config{XCOutputAccounts: []config.XCOutputAccount{{ID: 1, EnableLive: true, MaxConnections: 1}}}, nil, nil, nil, nil)
	account, _ := sp.AccountRegistry().Authenticate("", "")
	channel := &types.Channel{Name: "Mixed", Streams: []*types.Stream{
		{Name: "Mixed", ContentType: types.ContentTypeLive},
		{Name: "Mixed", ContentType: types.ContentTypeVOD},
	}}
	authorized := authorizedLegacyChannel(channel, account)
	if authorized == nil || len(authorized.Streams) != 1 || authorized.Streams[0].ContentType != types.ContentTypeLive {
		t.Fatalf("authorized channel = %#v", authorized)
	}
}

func TestXCStreamRejectsCrossRouteAndDisabledIDsWithoutLease(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("action") {
		case "get_live_streams":
			_, _ = w.Write([]byte(`[{"stream_id":1,"name":"Shared","category_id":"1"}]`))
		case "get_vod_streams":
			_, _ = w.Write([]byte(`[{"stream_id":2,"name":"Shared","category_id":"2"}]`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer provider.Close()
	sp := newXCHandlerTestProxy(t, provider.URL, config.XCOutputAccount{ID: 1, Username: "u", Password: "p", MaxConnections: 1, EnableLive: true})

	for _, path := range []string{"/movie/u/p/" + strconv.Itoa(proxy.XCOutputID("Shared")), "/series/u/p/" + strconv.Itoa(proxy.XCOutputID("Shared"))} {
		req := mux.SetURLVars(httptest.NewRequest(http.MethodGet, path, nil), map[string]string{"username": "u", "password": "p", "id": strconv.Itoa(proxy.XCOutputID("Shared"))})
		response := httptest.NewRecorder()
		HandleXCStream(sp)(response, req)
		if response.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404", path, response.Code)
		}
	}
	account, _ := sp.AccountRegistry().Authenticate("u", "p")
	if got := account.ActiveConnections(); got != 0 {
		t.Fatalf("denials consumed %d leases", got)
	}
}

func TestBuildStreamListCategoryFilteringAbsentZeroValidAndUnknown(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("action") {
		case "get_live_categories":
			_, _ = w.Write([]byte(`[{"category_id":"1","category_name":"News"},{"category_id":"2","category_name":"Sports"}]`))
		case "get_live_streams":
			_, _ = w.Write([]byte(`[{"stream_id":1,"name":"Alpha","category_id":"1"},{"stream_id":2,"name":"Beta","category_id":"2"}]`))
		case "get_vod_categories":
			_, _ = w.Write([]byte(`[{"category_id":"1","category_name":"Movies"},{"category_id":"2","category_name":"Documentaries"}]`))
		case "get_vod_streams":
			_, _ = w.Write([]byte(`[{"stream_id":3,"name":"Cinema","category_id":"1"},{"stream_id":4,"name":"Doc","category_id":"2"}]`))
		case "get_series_categories":
			_, _ = w.Write([]byte(`[{"category_id":"1","category_name":"Drama"},{"category_id":"2","category_name":"Comedy"}]`))
		case "get_series":
			_, _ = w.Write([]byte(`[{"series_id":5,"name":"Drama Show","category_id":"1"},{"series_id":6,"name":"Comedy Show","category_id":"2"}]`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer provider.Close()
	sp := newXCHandlerTestProxy(t, provider.URL, config.XCOutputAccount{Username: "u", Password: "p", EnableLive: true, EnableVOD: true, EnableSeries: true})
	for _, content := range []struct {
		contentType string
		category    string
	}{{"live", "News"}, {"vod", "Movies"}, {"series", "Drama"}} {
		for _, test := range []struct {
			category string
			want     int
		}{{"", 2}, {"0", 2}, {categoryIDFromName(content.category), 1}, {"999999", 0}} {
			if got := buildStreamList(sp, content.contentType, "http://proxy", "u", "p", test.category); len(got) != test.want {
				t.Errorf("%s category %q returned %d streams, want %d", content.contentType, test.category, len(got), test.want)
			} else if test.want == 0 {
				encoded, err := json.Marshal(got)
				if err != nil || string(encoded) != "[]" {
					t.Errorf("%s empty category encoded as %s, err %v; want []", content.contentType, encoded, err)
				}
			}
		}
	}
}

func TestXCPlayerAPIDetailsHonorFlagsAndExposeSeriesEpisodes(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("action") {
		case "get_vod_categories", "get_series_categories":
			_, _ = w.Write([]byte(`[]`))
		case "get_live_categories", "get_live_streams":
			_, _ = w.Write([]byte(`[]`))
		case "get_vod_streams":
			_, _ = w.Write([]byte(`[{"stream_id":33,"name":"Movie","category_id":"3","container_extension":"mkv"}]`))
		case "get_series":
			_, _ = w.Write([]byte(`[{"series_id":"44","name":"Show","category_id":"4"}]`))
		case "get_vod_info":
			_, _ = w.Write([]byte(`{"info":{"plot":"partial"},"movie_data":{"stream_id":33,"name":"Movie","container_extension":"mkv"}}`))
		case "get_series_info":
			_, _ = w.Write([]byte(`{"seasons":[{"season_number":1},{"season_number":"2"}],"episodes":{"1":[{"id":"501","episode_num":1,"title":"Pilot","container_extension":".MP4"},{"id":false}],"2":[{"id":502,"episode_num":"2","title":"Finale","container_extension":"mkv"}]}}`))
		}
	}))
	defer provider.Close()
	account := config.XCOutputAccount{Username: "u", Password: "p", EnableVOD: true, EnableSeries: true, MaxConnections: 1}
	sp := newXCHandlerTestProxy(t, provider.URL, account)
	handler := HandleXCPlayerAPI(sp)

	vodID := proxy.XCOutputID("Movie")
	vod := httptest.NewRecorder()
	handler(vod, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/player_api.php?username=u&password=p&action=get_vod_info&vod_id=%d", vodID), nil))
	if vod.Code != http.StatusOK {
		t.Fatalf("VOD detail status = %d, body %s", vod.Code, vod.Body.String())
	}
	var vodBody map[string]any
	if err := json.Unmarshal(vod.Body.Bytes(), &vodBody); err != nil || vodBody["movie_data"] == nil {
		t.Fatalf("VOD detail body = %s, err %v", vod.Body.String(), err)
	}

	seriesID := proxy.XCOutputID("Show")
	series := httptest.NewRecorder()
	handler(series, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/player_api.php?username=u&password=p&action=get_series_info&series_id=%d", seriesID), nil))
	if series.Code != http.StatusOK {
		t.Fatalf("series detail status = %d, body %s", series.Code, series.Body.String())
	}
	var seriesBody struct {
		Episodes map[string][]struct {
			ID                 string `json:"id"`
			ContainerExtension string `json:"container_extension"`
		} `json:"episodes"`
	}
	if err := json.Unmarshal(series.Body.Bytes(), &seriesBody); err != nil {
		t.Fatal(err)
	}
	if len(seriesBody.Episodes["1"]) != 1 || len(seriesBody.Episodes["2"]) != 1 || seriesBody.Episodes["1"][0].ID == "501" || seriesBody.Episodes["1"][0].ContainerExtension != "mp4" {
		t.Fatalf("series episodes = %#v", seriesBody.Episodes)
	}
	publicID, err := strconv.Atoi(seriesBody.Episodes["1"][0].ID)
	if err != nil {
		t.Fatalf("public episode ID = %q, want numeric proxy ID", seriesBody.Episodes["1"][0].ID)
	}
	if record := sp.LookupXCRecord(types.ContentTypeEpisode, publicID); record == nil || record.Identity.ProviderID != "501" || record.Identity.ProviderSeriesID != "44" || record.Stream.ContainerExtension != "mp4" {
		t.Fatalf("episode record = %#v", record)
	}

	sp.Config.XCOutputAccounts[0].EnableSeries = false
	sp.AccountRegistry().Replace(sp.Config.XCOutputAccounts)
	denied := httptest.NewRecorder()
	handler(denied, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/player_api.php?username=u&password=p&action=get_series_info&series_id=%d", seriesID), nil))
	if denied.Code != http.StatusNotFound {
		t.Fatalf("disabled series detail status = %d, want 404", denied.Code)
	}
}

func TestPrepareXCSeriesEpisodesScopesPublicIDsBySourceAndSeries(t *testing.T) {
	sp := proxy.New(&config.Config{}, nil, nil, nil, nil)
	generation := sp.XCSnapshot()

	prepare := func(sourceURL, seriesID string) (int, string) {
		source := &config.SourceConfig{URL: sourceURL, Username: "provider-user", Password: "provider-pass"}
		series := &types.XCRecord{
			Identity: types.XCIdentity{ContentType: types.ContentTypeSeries, ProviderID: seriesID, ProviderSource: sourceURL},
			Name:     "Show " + seriesID,
			Stream:   &types.Stream{Source: source, Attributes: map[string]string{"group-title": "Drama"}},
		}
		detail := parser.XCSeriesDetail{Episodes: map[string][]parser.XCSeriesEpisode{
			"1": {{ID: "shared-episode", EpisodeNum: "1", Title: "Pilot", ContainerExtension: ".MKV"}},
		}}
		prepareXCSeriesEpisodes(sp, generation, series, &detail)
		if len(detail.Episodes["1"]) != 1 {
			t.Fatalf("episodes for %s/%s = %#v", sourceURL, seriesID, detail.Episodes)
		}
		publicID, err := strconv.Atoi(string(detail.Episodes["1"][0].ID))
		if err != nil {
			t.Fatalf("public episode ID = %q: %v", detail.Episodes["1"][0].ID, err)
		}
		record := sp.LookupXCRecord(types.ContentTypeEpisode, publicID)
		if record == nil || record.Identity.ProviderID != "shared-episode" || record.Identity.ProviderSeriesID != seriesID {
			t.Fatalf("episode lookup for %d = %#v", publicID, record)
		}
		return publicID, record.Stream.URL
	}

	sourceA, urlA := prepare("http://provider-a", "series-10")
	sourceB, urlB := prepare("http://provider-b", "series-10")
	seriesB, urlSeriesB := prepare("http://provider-a", "series-20")
	if sourceA == sourceB || sourceA == seriesB || sourceB == seriesB {
		t.Fatalf("scoped public IDs collided: source-a=%d source-b=%d series-b=%d", sourceA, sourceB, seriesB)
	}
	for _, stream := range []struct {
		streamURL string
		host      string
	}{{urlA, "provider-a"}, {urlB, "provider-b"}, {urlSeriesB, "provider-a"}} {
		parsed, err := url.Parse(stream.streamURL)
		if err != nil {
			t.Fatal(err)
		}
		if parsed.Host != stream.host || !strings.HasSuffix(parsed.Path, "/series/provider-user/provider-pass/shared-episode.mkv") {
			t.Errorf("upstream episode URL = %q", parsed.String())
		}
	}
}

func TestHandleXCStreamRedirectsM3U8LiveRequestToTS(t *testing.T) {
	const (
		username = "viewer"
		password = "sec#ret"
	)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("action") {
		case "get_live_streams":
			_, _ = w.Write([]byte(`[{"stream_id":1,"name":"Test Channel","category_id":"1"}]`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer provider.Close()
	sp := newXCHandlerTestProxy(t, provider.URL, config.XCOutputAccount{
		ID: 1, Username: username, Password: password, MaxConnections: 1, EnableLive: true,
	})
	streamID := proxy.XCOutputID("Test Channel")

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
