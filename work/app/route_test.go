package app

import (
	"context"
	"encoding/base64"
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

	"kptv-proxy/work/cache"
	"kptv-proxy/work/client"
	"kptv-proxy/work/config"
	"kptv-proxy/work/constants"
	"kptv-proxy/work/db"
	"kptv-proxy/work/epgindex"
	"kptv-proxy/work/proxy"
	"kptv-proxy/work/types"

	"github.com/gorilla/mux"
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "kptv-app-test-")
	if err != nil {
		panic(err)
	}
	constants.Internal.DatabasePath = filepath.Join(dir, "kptv.db")
	code := m.Run()
	db.Close()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

type xcRouteFixture struct {
	router *mux.Router
	proxy  *proxy.StreamProxy
}

func newXCRouteFixture(t *testing.T, account config.XCOutputAccount) *xcRouteFixture {
	t.Helper()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("action") {
		case "get_live_categories":
			_, _ = w.Write([]byte(`[{"category_id":"1","category_name":"News"}]`))
		case "get_live_streams":
			_, _ = w.Write([]byte(`[{"stream_id":11,"name":"Live News","category_id":1,"epg_channel_id":"news.epg"}]`))
		case "get_vod_categories":
			_, _ = w.Write([]byte(`[{"category_id":2,"category_name":"Movies"}]`))
		case "get_vod_streams":
			_, _ = w.Write([]byte(`[{"stream_id":"33","name":"Movie One","category_id":"2","container_extension":".MP4"}]`))
		case "get_series_categories":
			_, _ = w.Write([]byte(`[{"category_id":"3","category_name":"Drama"}]`))
		case "get_series":
			_, _ = w.Write([]byte(`[{"series_id":44,"name":"Series One","category_id":3}]`))
		case "get_vod_info":
			_, _ = w.Write([]byte(`{"info":{"plot":"partial metadata"},"movie_data":{"stream_id":33,"container_extension":"mp4"}}`))
		case "get_series_info":
			_, _ = w.Write([]byte(`{"info":{"name":"Series One"},"seasons":[{"season_number":1}],"episodes":{"1":[{"id":501,"episode_num":"1","title":"Pilot","container_extension":"mkv"}]}}`))
		default:
			if strings.HasPrefix(r.URL.Path, "/live/") || strings.HasPrefix(r.URL.Path, "/movie/") || strings.HasPrefix(r.URL.Path, "/series/") {
				w.Header().Set("Content-Type", "video/mp2t")
				_, _ = w.Write([]byte("stream-data"))
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
				<-r.Context().Done()
				return
			}
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	t.Cleanup(provider.Close)

	xcCache, err := cache.NewCache(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(xcCache.Close)
	account.ID = 101
	if account.Username == "" {
		account.Username = "u"
	}
	if account.Password == "" {
		account.Password = "p"
	}
	cfg := &config.Config{
		BaseURL: "http://proxy", WorkerThreads: 2, MaxConnectionsToApp: 10, BufferSizePerStream: 1,
		Sources:          []config.SourceConfig{{URL: provider.URL, Username: "provider-user", Password: "provider-pass", MaxConnections: 2}},
		XCOutputAccounts: []config.XCOutputAccount{account},
	}
	sp := proxy.New(cfg, nil, client.NewHeaderSettingClient(time.Second), nil, xcCache)
	sp.ImportStreams()
	t.Cleanup(func() {
		provider.CloseClientConnections()
		sp.Channels.Range(func(_ string, channel *types.Channel) bool {
			channel.Mu.RLock()
			restreamer := channel.Restreamer
			channel.Mu.RUnlock()
			if restreamer != nil {
				restreamer.CancelStream()
			}
			return true
		})
	})
	router := mux.NewRouter()
	RegisterRoutes(router, sp)
	return &xcRouteFixture{router: router, proxy: sp}
}

func (f *xcRouteFixture) request(t *testing.T, method, target string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	var body *strings.Reader
	if form == nil {
		body = strings.NewReader("")
	} else {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, target, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	recorder := httptest.NewRecorder()
	f.router.ServeHTTP(recorder, req)
	return recorder
}

func decodeJSON[T any](t *testing.T, recorder *httptest.ResponseRecorder) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(recorder.Body.Bytes(), &value); err != nil {
		t.Fatalf("decode response %q: %v", recorder.Body.String(), err)
	}
	return value
}

func waitForActiveConnections(t *testing.T, account *proxy.XCAccount, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if account.ActiveConnections() == int32(want) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("active connections = %d, want %d", account.ActiveConnections(), want)
}

func (f *xcRouteFixture) startPlayback(t *testing.T, target string) (*httptest.ResponseRecorder, context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, target, nil).WithContext(ctx)
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		f.router.ServeHTTP(recorder, req)
		close(done)
	}()
	account, _ := f.proxy.AccountRegistry().Authenticate("u", "p")
	deadline := time.Now().Add(2 * time.Second)
	for account.ActiveConnections() != 1 && time.Now().Before(deadline) {
		select {
		case <-done:
			cancel()
			t.Fatalf("playback %s returned before admission: status = %d, body = %q", target, recorder.Code, recorder.Body.String())
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if account.ActiveConnections() != 1 {
		cancel()
		<-done
		t.Fatalf("playback %s did not acquire a connection", target)
	}
	return recorder, cancel, done
}

func stopPlayback(t *testing.T, recorder *httptest.ResponseRecorder, cancel context.CancelFunc, done <-chan struct{}) {
	t.Helper()
	cancel()
	<-done
	if recorder.Code != http.StatusOK {
		t.Fatalf("playback status = %d, body = %q", recorder.Code, recorder.Body.String())
	}
}

func TestPlayerAPIRouteNormalizesGETAndFormPOST(t *testing.T) {
	username := "user +&=%?#/雪"
	password := "pass +&=%?#/雪"
	sp := &proxy.StreamProxy{Config: &config.Config{
		BaseURL: "http://proxy",
		XCOutputAccounts: []config.XCOutputAccount{{
			Name: "test", Username: username, Password: password, MaxConnections: 1,
		}},
	}}
	router := mux.NewRouter()
	RegisterRoutes(router, sp)

	t.Run("GET query", func(t *testing.T) {
		query := url.Values{"username": {username}, "password": {password}}
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/player_api.php?"+query.Encode(), nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %q", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("form body overrides query", func(t *testing.T) {
		form := url.Values{"username": {username}, "password": {password}}
		req := httptest.NewRequest(http.MethodPost, "/player_api.php?username=wrong&password=wrong", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %q", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("POST query without body", func(t *testing.T) {
		query := url.Values{"username": {username}, "password": {password}}
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/player_api.php?"+query.Encode(), nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %q", recorder.Code, recorder.Body.String())
		}
	})
}

func TestPlayerAPIRouteRejectsAmbiguousOrUnboundedParameters(t *testing.T) {
	sp := &proxy.StreamProxy{Config: &config.Config{}}
	router := mux.NewRouter()
	RegisterRoutes(router, sp)

	tests := []struct {
		name        string
		req         *http.Request
		wantStatus  int
		secretValue string
	}{
		{
			name:        "duplicate query value",
			req:         httptest.NewRequest(http.MethodGet, "/player_api.php?username=secret&username=other&password=x", nil),
			wantStatus:  http.StatusBadRequest,
			secretValue: "secret",
		},
		{
			name:       "bounded limit",
			req:        httptest.NewRequest(http.MethodGet, "/player_api.php?limit=1001", nil),
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "get.php duplicate credential",
			req:        httptest.NewRequest(http.MethodGet, "/get.php?username=one&username=two", nil),
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "xmltv.php duplicate credential",
			req:        httptest.NewRequest(http.MethodGet, "/xmltv.php?password=one&password=two", nil),
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "oversized form body",
			req: func() *http.Request {
				req := httptest.NewRequest(http.MethodPost, "/player_api.php", strings.NewReader("username="+strings.Repeat("s", 64<<10)))
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				return req
			}(),
			wantStatus: http.StatusRequestEntityTooLarge,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, test.req)
			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body = %q", recorder.Code, test.wantStatus, recorder.Body.String())
			}
			if test.secretValue != "" && strings.Contains(recorder.Body.String(), test.secretValue) {
				t.Fatalf("error response exposed credential: %q", recorder.Body.String())
			}
		})
	}
}

func TestDirectXCRouteRejectsEncodedCredentialSeparatorWithoutExposure(t *testing.T) {
	password := "pass/secret"
	sp := &proxy.StreamProxy{Config: &config.Config{XCOutputAccounts: []config.XCOutputAccount{{Username: "user", Password: password}}}}
	router := mux.NewRouter()
	RegisterRoutes(router, sp)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/live/user/pass%2Fsecret/1.ts", nil))
	if recorder.Code < 400 {
		t.Fatalf("status = %d, want controlled rejection for encoded slash", recorder.Code)
	}
	if strings.Contains(recorder.Body.String(), password) {
		t.Fatalf("error response exposed credential: %q", recorder.Body.String())
	}
}

func TestXCRoutesClientCompatibilityFlow(t *testing.T) {
	fixture := newXCRouteFixture(t, config.XCOutputAccount{
		Username: "u", Password: "p", MaxConnections: 1, EnableLive: true, EnableVOD: true, EnableSeries: true,
	})

	auth := fixture.request(t, http.MethodGet, "/player_api.php?username=u&password=p", nil)
	if auth.Code != http.StatusOK {
		t.Fatalf("authentication status = %d, body = %q", auth.Code, auth.Body.String())
	}
	authBody := decodeJSON[struct {
		UserInfo struct {
			Auth           int    `json:"auth"`
			ActiveCons     string `json:"active_cons"`
			MaxConnections string `json:"max_connections"`
		} `json:"user_info"`
		ServerInfo struct {
			Protocol string `json:"server_protocol"`
		} `json:"server_info"`
	}](t, auth)
	if authBody.UserInfo.Auth != 1 || authBody.UserInfo.ActiveCons != "0" || authBody.UserInfo.MaxConnections != "1" || authBody.ServerInfo.Protocol != "http" {
		t.Fatalf("authentication response = %#v", authBody)
	}

	type category struct {
		ID   string `json:"category_id"`
		Name string `json:"category_name"`
	}
	type stream struct {
		ID                 int    `json:"stream_id"`
		Name               string `json:"name"`
		CategoryID         string `json:"category_id"`
		DirectSource       string `json:"direct_source"`
		ContainerExtension string `json:"container_extension"`
	}

	vodCategories := decodeJSON[[]category](t, fixture.request(t, http.MethodGet, "/player_api.php?username=u&password=p&action=get_vod_categories", nil))
	if len(vodCategories) != 1 || vodCategories[0].Name != "Movies" || vodCategories[0].ID == "" {
		t.Fatalf("VOD categories = %#v", vodCategories)
	}
	vodCatalog := decodeJSON[[]stream](t, fixture.request(t, http.MethodGet, "/player_api.php?username=u&password=p&action=get_vod_streams&category_id="+url.QueryEscape(vodCategories[0].ID), nil))
	if len(vodCatalog) != 1 || vodCatalog[0].Name != "Movie One" || vodCatalog[0].CategoryID != vodCategories[0].ID || vodCatalog[0].ContainerExtension != "mp4" {
		t.Fatalf("filtered VOD catalog = %#v", vodCatalog)
	}
	emptyCatalog := decodeJSON[[]stream](t, fixture.request(t, http.MethodGet, "/player_api.php?username=u&password=p&action=get_vod_streams&category_id=missing", nil))
	if emptyCatalog == nil || len(emptyCatalog) != 0 {
		t.Fatalf("unknown category response = %#v, want non-nil empty array", emptyCatalog)
	}

	vodDetail := fixture.request(t, http.MethodGet, fmt.Sprintf("/player_api.php?username=u&password=p&action=get_vod_info&vod_id=%d", vodCatalog[0].ID), nil)
	vodBody := decodeJSON[struct {
		Info      map[string]any `json:"info"`
		MovieData struct {
			ID                 string `json:"stream_id"`
			Name               string `json:"name"`
			ContainerExtension string `json:"container_extension"`
		} `json:"movie_data"`
	}](t, vodDetail)
	if vodBody.MovieData.ID != strconv.Itoa(vodCatalog[0].ID) || vodBody.MovieData.Name != "Movie One" || vodBody.MovieData.ContainerExtension != "mp4" || vodBody.Info["plot"] != "partial metadata" {
		t.Fatalf("partial VOD detail = %#v", vodBody)
	}

	vodPlayback, cancelVOD, vodDone := fixture.startPlayback(t, mustRequestURI(t, vodCatalog[0].DirectSource))
	stopPlayback(t, vodPlayback, cancelVOD, vodDone)
	account, _ := fixture.proxy.AccountRegistry().Authenticate("u", "p")
	waitForActiveConnections(t, account, 0)

	seriesCategories := decodeJSON[[]category](t, fixture.request(t, http.MethodGet, "/player_api.php?username=u&password=p&action=get_series_categories", nil))
	seriesCatalog := decodeJSON[[]stream](t, fixture.request(t, http.MethodGet, "/player_api.php?username=u&password=p&action=get_series&category_id="+url.QueryEscape(seriesCategories[0].ID), nil))
	if len(seriesCatalog) != 1 || seriesCatalog[0].Name != "Series One" || seriesCatalog[0].DirectSource != "" {
		t.Fatalf("filtered series catalog = %#v", seriesCatalog)
	}
	seriesDetail := fixture.request(t, http.MethodGet, fmt.Sprintf("/player_api.php?username=u&password=p&action=get_series_info&series_id=%d", seriesCatalog[0].ID), nil)
	seriesBody := decodeJSON[struct {
		Info     map[string]any   `json:"info"`
		Seasons  []map[string]any `json:"seasons"`
		Episodes map[string][]struct {
			ID                 string `json:"id"`
			ContainerExtension string `json:"container_extension"`
		} `json:"episodes"`
	}](t, seriesDetail)
	if seriesBody.Info["name"] != "Series One" || len(seriesBody.Seasons) != 1 || len(seriesBody.Episodes["1"]) != 1 || seriesBody.Episodes["1"][0].ContainerExtension != "mkv" {
		t.Fatalf("series detail = %#v", seriesBody)
	}
	episodeID, err := strconv.Atoi(seriesBody.Episodes["1"][0].ID)
	if err != nil || fixture.proxy.LookupXCRecord(types.ContentTypeEpisode, episodeID) == nil {
		t.Fatalf("series episode ID %q was not registered: %v", seriesBody.Episodes["1"][0].ID, err)
	}
	for _, target := range []string{
		"/player_api.php?username=u&password=p&action=get_vod_info&vod_id=malformed",
		"/player_api.php?username=u&password=p&action=get_series_info&series_id=999999",
		"/movie/u/p/" + strconv.Itoa(proxy.XCOutputID("Live News")) + ".mp4",
		"/series/u/p/" + strconv.Itoa(seriesCatalog[0].ID) + ".mkv",
	} {
		if response := fixture.request(t, http.MethodGet, target, nil); response.Code != http.StatusNotFound {
			t.Errorf("%s status = %d, want 404", target, response.Code)
		}
	}
	episodePlayback, cancelEpisode, episodeDone := fixture.startPlayback(t, "/series/u/p/"+seriesBody.Episodes["1"][0].ID+".mkv")
	stopPlayback(t, episodePlayback, cancelEpisode, episodeDone)
	waitForActiveConnections(t, account, 0)

	playlist := fixture.request(t, http.MethodGet, "/get.php?username=u&password=p&type=m3u_plus", nil)
	if playlist.Code != http.StatusOK || !strings.HasPrefix(playlist.Body.String(), "#EXTM3U\n") {
		t.Fatalf("M3U response status = %d, body = %q", playlist.Code, playlist.Body.String())
	}
	for _, directURL := range []string{vodCatalog[0].DirectSource, firstDirectURL(t, fixture, "get_live_streams")} {
		playback, cancel, done := fixture.startPlayback(t, mustRequestURI(t, directURL))
		stopPlayback(t, playback, cancel, done)
		waitForActiveConnections(t, account, 0)
		if !strings.Contains(playlist.Body.String(), directURL) {
			t.Errorf("M3U does not contain requestable catalog URL %q", directURL)
		}
	}
}

func mustRequestURI(t *testing.T, rawURL string) string {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.RequestURI()
}

func firstDirectURL(t *testing.T, fixture *xcRouteFixture, action string) string {
	t.Helper()
	streams := decodeJSON[[]struct {
		DirectSource string `json:"direct_source"`
	}](t, fixture.request(t, http.MethodGet, "/player_api.php?username=u&password=p&action="+action, nil))
	if len(streams) == 0 || streams[0].DirectSource == "" {
		t.Fatalf("%s response has no direct URL", action)
	}
	return streams[0].DirectSource
}

func TestXCRoutesCompatibilityErrorsParityAndContentFlags(t *testing.T) {
	fixture := newXCRouteFixture(t, config.XCOutputAccount{
		Username: "u", Password: "p", MaxConnections: 1, EnableLive: true, EnableVOD: false, EnableSeries: false,
	})

	invalid := fixture.request(t, http.MethodGet, "/player_api.php?username=u&password=wrong", nil)
	invalidBody := decodeJSON[struct {
		UserInfo struct {
			Auth    int    `json:"auth"`
			Message string `json:"message"`
		} `json:"user_info"`
	}](t, invalid)
	if invalid.Code != http.StatusUnauthorized || invalidBody.UserInfo.Auth != 0 || invalidBody.UserInfo.Message != "Invalid credentials" {
		t.Fatalf("invalid credential response: status=%d body=%#v", invalid.Code, invalidBody)
	}

	unknown := fixture.request(t, http.MethodGet, "/player_api.php?username=u&password=p&action=unsupported_action", nil)
	base := decodeJSON[map[string]json.RawMessage](t, unknown)
	if unknown.Code != http.StatusOK || base["user_info"] == nil || base["server_info"] == nil {
		t.Fatalf("unknown action response: status=%d body=%q", unknown.Code, unknown.Body.String())
	}
	post := fixture.request(t, http.MethodPost, "/player_api.php?username=wrong&password=wrong", url.Values{"username": {"u"}, "password": {"p"}, "action": {"unsupported_action"}})
	var getUser, postUser struct {
		Auth           int    `json:"auth"`
		Username       string `json:"username"`
		MaxConnections string `json:"max_connections"`
	}
	if err := json.Unmarshal(base["user_info"], &getUser); err != nil {
		t.Fatal(err)
	}
	postBase := decodeJSON[map[string]json.RawMessage](t, post)
	if err := json.Unmarshal(postBase["user_info"], &postUser); err != nil {
		t.Fatal(err)
	}
	if getUser != postUser {
		t.Fatalf("GET user_info = %#v, form POST user_info = %#v", getUser, postUser)
	}

	for _, action := range []string{"get_vod_categories", "get_vod_streams", "get_series_categories", "get_series"} {
		body := decodeJSON[[]json.RawMessage](t, fixture.request(t, http.MethodGet, "/player_api.php?username=u&password=p&action="+action, nil))
		if body == nil || len(body) != 0 {
			t.Errorf("disabled %s response = %#v, want []", action, body)
		}
	}
	for _, target := range []string{
		"/player_api.php?username=u&password=p&action=get_vod_info&vod_id=not-a-number",
		"/player_api.php?username=u&password=p&action=get_series_info&series_id=999999",
		"/movie/u/p/" + strconv.Itoa(proxy.XCOutputID("Movie One")) + ".mp4",
		"/series/u/p/" + strconv.Itoa(proxy.XCOutputID("Series One")) + ".mkv",
	} {
		response := fixture.request(t, http.MethodGet, target, nil)
		if response.Code != http.StatusNotFound {
			t.Errorf("%s status = %d, want 404", target, response.Code)
		}
	}
	malformedPlayback := fixture.request(t, http.MethodGet, "/movie/u/p/not-a-number.mp4", nil)
	if malformedPlayback.Code != http.StatusBadRequest {
		t.Errorf("malformed direct playback status = %d, want 400", malformedPlayback.Code)
	}
}

func TestXCRoutesEPGUsesTypedLiveIdentity(t *testing.T) {
	fixture := newXCRouteFixture(t, config.XCOutputAccount{
		Username: "u", Password: "p", MaxConnections: 1, EnableLive: true, EnableVOD: true, EnableSeries: true,
	})
	liveID := proxy.XCOutputID("Live News")
	vodID := proxy.XCOutputID("Movie One")
	now := time.Now().UTC()
	epgindex.Rebuild(fmt.Sprintf(`<tv><channel id="%s"><display-name>Live News</display-name></channel><programme start="%s" stop="%s" channel="%s"><title>Current News</title><desc>Headlines</desc></programme></tv>`,
		proxy.DummyChannelID, now.Add(-time.Minute).Format("20060102150405 -0700"), now.Add(time.Hour).Format("20060102150405 -0700"), proxy.DummyChannelID))
	t.Cleanup(func() { epgindex.Rebuild("") })

	type epgResponse struct {
		Listings []struct {
			EPGID      string `json:"epg_id"`
			Title      string `json:"title"`
			NowPlaying int    `json:"now_playing"`
		} `json:"epg_listings"`
	}
	valid := decodeJSON[epgResponse](t, fixture.request(t, http.MethodGet, fmt.Sprintf("/player_api.php?username=u&password=p&action=get_simple_data_table&stream_id=%d", liveID), nil))
	if len(valid.Listings) != 1 {
		t.Fatalf("valid EPG listings = %#v, want one", valid.Listings)
	}
	title, err := base64.StdEncoding.DecodeString(valid.Listings[0].Title)
	if err != nil || valid.Listings[0].EPGID != strconv.Itoa(liveID) || string(title) != "Current News" || valid.Listings[0].NowPlaying != 1 {
		t.Fatalf("valid EPG response = %#v, decoded title = %q, err = %v", valid, title, err)
	}
	for _, streamID := range []string{"", "malformed", strconv.Itoa(vodID), "999999"} {
		response := decodeJSON[epgResponse](t, fixture.request(t, http.MethodGet, "/player_api.php?username=u&password=p&action=get_short_epg&stream_id="+streamID, nil))
		if response.Listings == nil || len(response.Listings) != 0 {
			t.Errorf("EPG stream_id %q response = %#v, want empty listings", streamID, response)
		}
	}

	fixture.proxy.Config.XCOutputAccounts[0].EnableLive = false
	fixture.proxy.AccountRegistry().Replace(fixture.proxy.Config.XCOutputAccounts)
	disabled := decodeJSON[epgResponse](t, fixture.request(t, http.MethodGet, fmt.Sprintf("/player_api.php?username=u&password=p&action=get_short_epg&stream_id=%d", liveID), nil))
	if disabled.Listings == nil || len(disabled.Listings) != 0 {
		t.Fatalf("disabled live EPG response = %#v", disabled)
	}

	fixture.proxy.Config.XCOutputAccounts[0].EnableLive = true
	fixture.proxy.AccountRegistry().Replace(fixture.proxy.Config.XCOutputAccounts)
	fixture.proxy.Config.Sources = nil
	fixture.proxy.ImportStreams()
	stale := decodeJSON[epgResponse](t, fixture.request(t, http.MethodGet, fmt.Sprintf("/player_api.php?username=u&password=p&action=get_short_epg&stream_id=%d", liveID), nil))
	if stale.Listings == nil || len(stale.Listings) != 0 {
		t.Fatalf("stale live EPG response = %#v", stale)
	}
}

func TestXCRoutesEnforceConcurrentPlaybackLimit(t *testing.T) {
	fixture := newXCRouteFixture(t, config.XCOutputAccount{
		Username: "u", Password: "p", MaxConnections: 1, EnableLive: true,
	})
	directURL := firstDirectURL(t, fixture, "get_live_streams")
	first, cancel, done := fixture.startPlayback(t, mustRequestURI(t, directURL))

	denied := fixture.request(t, http.MethodGet, mustRequestURI(t, directURL), nil)
	if denied.Code != http.StatusTooManyRequests || !strings.Contains(denied.Body.String(), "Connection limit reached") {
		cancel()
		<-done
		t.Fatalf("concurrent playback status = %d, body = %q", denied.Code, denied.Body.String())
	}
	auth := fixture.request(t, http.MethodGet, "/player_api.php?username=u&password=p", nil)
	user := decodeJSON[struct {
		UserInfo struct {
			ActiveCons string `json:"active_cons"`
		} `json:"user_info"`
	}](t, auth)
	if user.UserInfo.ActiveCons != "1" {
		t.Errorf("active_cons = %q during playback, want 1", user.UserInfo.ActiveCons)
	}

	stopPlayback(t, first, cancel, done)
	account, _ := fixture.proxy.AccountRegistry().Authenticate("u", "p")
	waitForActiveConnections(t, account, 0)
	retry, retryCancel, retryDone := fixture.startPlayback(t, mustRequestURI(t, directURL))
	stopPlayback(t, retry, retryCancel, retryDone)
}
