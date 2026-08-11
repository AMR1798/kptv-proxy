package handlers

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"kptv-proxy/work/epgindex"
	"kptv-proxy/work/logger"
	"kptv-proxy/work/parser"
	"kptv-proxy/work/proxy"
	"kptv-proxy/work/types"
	"kptv-proxy/work/utils"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
)

// xcUserInfo represents the user_info block in XC API responses.
type xcUserInfo struct {
	Username             string   `json:"username"`
	Password             string   `json:"password"`
	Message              string   `json:"message"`
	Auth                 int      `json:"auth"`
	Status               string   `json:"status"`
	ExpDate              *string  `json:"exp_date"`
	IsTrial              string   `json:"is_trial"`
	ActiveCons           string   `json:"active_cons"`
	CreatedAt            string   `json:"created_at"`
	MaxConnections       string   `json:"max_connections"`
	AllowedOutputFormats []string `json:"allowed_output_formats"`
}

// xcServerInfo represents the server_info block in XC API responses.
type xcServerInfo struct {
	URL            string `json:"url"`
	Port           string `json:"port"`
	HTTPSPort      string `json:"https_port"`
	ServerProtocol string `json:"server_protocol"`
	RTMPPort       string `json:"rtmp_port"`
	Timezone       string `json:"timezone"`
	TimestampNow   int64  `json:"timestamp_now"`
	TimeNow        string `json:"time_now"`
}

// xcStream represents a stream entry (live, VOD, or series) in XC API output.
type xcStream struct {
	Num                int    `json:"num"`
	Name               string `json:"name"`
	StreamType         string `json:"stream_type"`
	StreamID           int    `json:"stream_id"`
	StreamIcon         string `json:"stream_icon"`
	EPGChannelID       string `json:"epg_channel_id"`
	Added              string `json:"added"`
	CategoryID         string `json:"category_id"`
	CustomSid          string `json:"custom_sid"`
	TVArchive          int    `json:"tv_archive"`
	DirectSource       string `json:"direct_source"`
	TVArchiveDuration  int    `json:"tv_archive_duration"`
	ContainerExtension string `json:"container_extension,omitempty"`
}

// xcCategory represents a category in XC API output.
type xcCategory struct {
	CategoryID   string `json:"category_id"`
	CategoryName string `json:"category_name"`
	ParentID     int    `json:"parent_id"`
}

// xcChannelBatch is a lightweight name+channel pair for sorted iteration.
type xcChannelBatch struct {
	name    string
	channel *types.Channel
}

// xcEPGListing is one programme entry in XC EPG API responses. Title and
// description are base64-encoded per the XC API convention.
type xcEPGListing struct {
	ID             string `json:"id"`
	EPGID          string `json:"epg_id"`
	Title          string `json:"title"`
	Lang           string `json:"lang"`
	Start          string `json:"start"`
	End            string `json:"end"`
	Description    string `json:"description"`
	ChannelID      string `json:"channel_id"`
	StartTimestamp string `json:"start_timestamp"`
	StopTimestamp  string `json:"stop_timestamp"`
	NowPlaying     int    `json:"now_playing,omitempty"`
	HasArchive     int    `json:"has_archive"`
}

const (
	maxXCFormBodyBytes = 64 << 10
	maxXCEPGListings   = 1000
)

type xcRequestParameters struct {
	username   string
	password   string
	action     string
	categoryID string
	vodID      string
	seriesID   string
	streamID   string
	outputType string
	limit      int
}

type xcRequestError struct {
	status int
}

func (e *xcRequestError) Error() string {
	return http.StatusText(e.status)
}

var xcSupportedParameters = []string{
	"username", "password", "action", "category_id", "vod_id", "series_id", "stream_id", "type", "limit",
}

// normalizeXCRequestParameters applies one policy across XC query and form
// requests: form values override query values, while duplicates within either
// source are rejected as ambiguous.
func normalizeXCRequestParameters(r *http.Request) (xcRequestParameters, error) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return xcRequestParameters{}, &xcRequestError{status: http.StatusBadRequest}
	}
	if err := rejectDuplicateXCParameters(query); err != nil {
		return xcRequestParameters{}, err
	}

	var form url.Values
	if r.Method == http.MethodPost {
		if r.ContentLength > maxXCFormBodyBytes {
			return xcRequestParameters{}, &xcRequestError{status: http.StatusRequestEntityTooLarge}
		}
		if r.ContentLength != 0 || r.Header.Get("Content-Type") != "" {
			mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || mediaType != "application/x-www-form-urlencoded" {
				return xcRequestParameters{}, &xcRequestError{status: http.StatusUnsupportedMediaType}
			}
			body, err := io.ReadAll(io.LimitReader(r.Body, maxXCFormBodyBytes+1))
			if err != nil {
				return xcRequestParameters{}, &xcRequestError{status: http.StatusBadRequest}
			}
			if len(body) > maxXCFormBodyBytes {
				return xcRequestParameters{}, &xcRequestError{status: http.StatusRequestEntityTooLarge}
			}
			form, err = url.ParseQuery(string(body))
			if err != nil {
				return xcRequestParameters{}, &xcRequestError{status: http.StatusBadRequest}
			}
			if err := rejectDuplicateXCParameters(form); err != nil {
				return xcRequestParameters{}, err
			}
		}
	}

	value := func(key string) string {
		if form != nil {
			if values, ok := form[key]; ok {
				return values[0]
			}
		}
		return query.Get(key)
	}
	params := xcRequestParameters{
		username:   value("username"),
		password:   value("password"),
		action:     value("action"),
		categoryID: value("category_id"),
		vodID:      value("vod_id"),
		seriesID:   value("series_id"),
		streamID:   value("stream_id"),
		outputType: value("type"),
		limit:      4,
	}
	if rawLimit := value("limit"); rawLimit != "" {
		limit, err := strconv.Atoi(rawLimit)
		if err != nil || limit < 1 || limit > maxXCEPGListings {
			return xcRequestParameters{}, &xcRequestError{status: http.StatusBadRequest}
		}
		params.limit = limit
	}
	return params, nil
}

func rejectDuplicateXCParameters(values url.Values) error {
	for _, key := range xcSupportedParameters {
		if len(values[key]) > 1 {
			return &xcRequestError{status: http.StatusBadRequest}
		}
	}
	return nil
}

func writeXCRequestError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if requestErr, ok := err.(*xcRequestError); ok {
		status = requestErr.status
	}
	http.Error(w, "Invalid XC request", status)
}

// getSortedChannels snapshots the channel map and returns it sorted alphabetically
// by channel name. All XC output functions must use this instead of ranging the
// map directly to guarantee consistent ordering across every response.
func getSortedChannels(sp *proxy.StreamProxy) []xcChannelBatch {
	batch := make([]xcChannelBatch, 0, 1000)
	sp.Channels.Range(func(name string, ch *types.Channel) bool {
		batch = append(batch, xcChannelBatch{name, ch})
		return true
	})
	if sp.Config.SortField == "preserve-order" {
		sort.Slice(batch, func(i, j int) bool {
			return xcChannelOriginalOrderLess(batch[i], batch[j])
		})
	} else {
		sort.Slice(batch, func(i, j int) bool {
			return strings.ToLower(batch[i].name) < strings.ToLower(batch[j].name)
		})
	}
	return batch
}

func xcChannelOriginalOrderLess(a, b xcChannelBatch) bool {
	aSourceOrder, aImportOrder := xcChannelOriginalOrder(a.channel)
	bSourceOrder, bImportOrder := xcChannelOriginalOrder(b.channel)
	if aSourceOrder != bSourceOrder {
		return aSourceOrder < bSourceOrder
	}
	if aImportOrder != bImportOrder {
		return aImportOrder < bImportOrder
	}
	return strings.ToLower(a.name) < strings.ToLower(b.name)
}

func xcChannelOriginalOrder(ch *types.Channel) (int, int) {
	ch.Mu.RLock()
	defer ch.Mu.RUnlock()

	if len(ch.Streams) == 0 {
		return int(^uint(0) >> 1), int(^uint(0) >> 1)
	}

	sourceOrder := ch.Streams[0].Source.Order
	importOrder := ch.Streams[0].ImportOrder
	for _, stream := range ch.Streams[1:] {
		if stream.Source.Order < sourceOrder || (stream.Source.Order == sourceOrder && stream.ImportOrder < importOrder) {
			sourceOrder = stream.Source.Order
			importOrder = stream.ImportOrder
		}
	}
	return sourceOrder, importOrder
}

// categoryIDFromName generates a stable string category ID from a group name.
func categoryIDFromName(name string) string {
	h := fnv.New32a()
	h.Write([]byte(name))
	id := int(h.Sum32() & 0x7FFFFFFF)
	if id == 0 {
		id = 1
	}
	return fmt.Sprintf("%d", id)
}

func buildXCStreamURL(baseURL, contentType, username, password string, streamID int, extension string) string {
	pathType := "live"
	suffix := "ts"
	switch contentType {
	case "vod":
		pathType = "movie"
		suffix = utils.NormalizeContainerExtension(extension)
	case "series":
		pathType = "series"
		suffix = utils.NormalizeContainerExtension(extension)
	}
	streamURL, err := utils.AppendURLPath(baseURL, pathType, username, password, fmt.Sprintf("%d.%s", streamID, suffix))
	if err != nil {
		return ""
	}
	return streamURL
}

// findXCAccount locates an XC output account by username and password.
func findXCAccount(sp *proxy.StreamProxy, username, password string) *proxy.XCAccount {
	account, _ := sp.AccountRegistry().Authenticate(username, password)
	return account
}

// getChannelContentType returns the content type for a channel.
// Caller must hold the channel read lock.
func getChannelContentType(ch *types.Channel) string {
	if len(ch.Streams) == 0 {
		return "live"
	}
	return string(utils.ContentTypeOfStream(ch.Streams[0]))
}

// buildXCServerInfo constructs the server_info block from the configured base URL.
func buildXCServerInfo(baseURL string) xcServerInfo {
	protocol := "http"
	host := baseURL
	port := "80"

	if strings.HasPrefix(baseURL, "https://") {
		protocol = "https"
		host = strings.TrimPrefix(baseURL, "https://")
		port = "443"
	} else {
		host = strings.TrimPrefix(host, "http://")
	}

	if idx := strings.LastIndex(host, ":"); idx != -1 {
		port = host[idx+1:]
		host = host[:idx]
	}

	return xcServerInfo{
		URL:            host,
		Port:           port,
		HTTPSPort:      "443",
		ServerProtocol: protocol,
		RTMPPort:       "1935",
		Timezone:       "UTC",
		TimestampNow:   time.Now().Unix(),
		TimeNow:        time.Now().Format("2006-01-02 15:04:05"),
	}
}

// buildXCUserInfo constructs the user_info block for an XC output account.
func buildXCUserInfo(account *proxy.XCAccount) xcUserInfo {
	return xcUserInfo{
		Username:             account.Config.Username,
		Password:             account.Config.Password,
		Message:              "",
		Auth:                 1,
		Status:               "Active",
		ExpDate:              nil,
		IsTrial:              "0",
		ActiveCons:           fmt.Sprintf("%d", account.ActiveConnections()),
		CreatedAt:            "0",
		MaxConnections:       fmt.Sprintf("%d", account.Config.MaxConnections),
		AllowedOutputFormats: []string{"ts", "m3u8"},
	}
}

// buildStreamList iterates sorted channels and builds the XC stream list for a
// given content type. Channels are always ordered alphabetically by name.
func buildStreamList(sp *proxy.StreamProxy, contentType, baseURL, username, password, categoryID string) []xcStream {
	streams := make([]xcStream, 0)
	num := 1

	// channel-name -> mapped epg_id; unmapped channels fall back to the dummy id
	epgMap := proxy.ChannelEPGMap()

	for _, record := range getSortedXCRecords(sp, types.ContentType(contentType)) {
		stream := record.Stream
		attrs := stream.Attributes
		extension := utils.NormalizeContainerExtension(stream.ContainerExtension)

		streamID := record.Identity.OutputID
		group := attrs["group-title"]
		generatedCategoryID := categoryIDFromName(group)
		if categoryID != "" && categoryID != "0" && categoryID != generatedCategoryID {
			continue
		}
		logo := attrs["tvg-logo"]
		tvgID := proxy.EPGIDForChannel(record.Name, epgMap)

		directURL := ""
		if contentType != string(types.ContentTypeSeries) {
			directURL = buildXCStreamURL(baseURL, contentType, username, password, streamID, extension)
		}

		s := xcStream{
			Num:               num,
			Name:              record.Name,
			StreamType:        contentType,
			StreamID:          streamID,
			StreamIcon:        logo,
			EPGChannelID:      tvgID,
			Added:             "0",
			CategoryID:        generatedCategoryID,
			CustomSid:         "",
			TVArchive:         0,
			DirectSource:      directURL,
			TVArchiveDuration: 0,
		}
		if contentType == "vod" || contentType == "series" {
			s.ContainerExtension = extension
		}

		streams = append(streams, s)
		num++
	}

	return streams
}

// buildCategoryList iterates sorted channels and returns unique categories for a
// given content type. Category order follows first-seen in alphabetical channel order.
func buildCategoryList(sp *proxy.StreamProxy, contentType string) []xcCategory {
	seen := make(map[string]bool)
	categories := make([]xcCategory, 0)

	for _, record := range getSortedXCRecords(sp, types.ContentType(contentType)) {
		group := record.Stream.Attributes["group-title"]
		if group == "" || seen[group] {
			continue
		}

		seen[group] = true
		categories = append(categories, xcCategory{
			CategoryID:   categoryIDFromName(group),
			CategoryName: group,
			ParentID:     0,
		})
	}

	return categories
}

func getSortedXCRecords(sp *proxy.StreamProxy, contentType types.ContentType) []*types.XCRecord {
	records := make([]*types.XCRecord, 0)
	for _, record := range sp.XCSnapshot().Records {
		if contentType == types.ContentTypeUnknown || record.Identity.ContentType == contentType {
			records = append(records, record)
		}
	}
	if sp.Config.SortField == "preserve-order" {
		sort.SliceStable(records, func(i, j int) bool {
			return xcChannelOriginalOrderLess(
				xcChannelBatch{records[i].Name, records[i].Channel},
				xcChannelBatch{records[j].Name, records[j].Channel},
			)
		})
	} else {
		sort.SliceStable(records, func(i, j int) bool {
			return strings.ToLower(records[i].Name) < strings.ToLower(records[j].Name)
		})
	}
	return records
}

func findXCDetailRecord(snapshot *types.XCCatalogSnapshot, contentType types.ContentType, rawID string) *types.XCRecord {
	id, err := strconv.Atoi(rawID)
	if err != nil || id < 1 {
		return nil
	}
	return snapshot.Lookup(contentType, id)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func xcDetailCacheGeneration(account *proxy.XCAccount, catalog uint64) string {
	return fmt.Sprintf("account:%d:auth:%d:catalog:%d", account.Config.ID, account.Generation, catalog)
}

func prepareXCSeriesEpisodes(sp *proxy.StreamProxy, generation *types.XCCatalogSnapshot, series *types.XCRecord, detail *parser.XCSeriesDetail) {
	if detail.Info == nil {
		detail.Info = parser.XCMetadata{}
	}
	if detail.Seasons == nil {
		detail.Seasons = []parser.XCSeriesSeason{}
	}
	if detail.Episodes == nil {
		detail.Episodes = map[string][]parser.XCSeriesEpisode{}
		return
	}
	for season, episodes := range detail.Episodes {
		valid := make([]parser.XCSeriesEpisode, 0, len(episodes))
		for _, episode := range episodes {
			providerID := string(episode.ID)
			outputID := proxy.XCEpisodeOutputID(series.Identity.ProviderSource, series.Identity.ProviderID, providerID)
			extension := utils.NormalizeContainerExtension(episode.ContainerExtension)
			episode.ContainerExtension = extension
			streamURL, err := utils.AppendURLPath(series.Stream.Source.URL, "series", series.Stream.Source.Username, series.Stream.Source.Password, providerID+"."+extension)
			if err != nil {
				continue
			}
			name := firstNonEmpty(episode.Title, series.Name+" episode "+string(episode.EpisodeNum))
			stream := &types.Stream{
				URL: streamURL, Name: name, Source: series.Stream.Source,
				ContentType: types.ContentTypeEpisode, ProviderID: providerID,
				ProviderSource: series.Identity.ProviderSource, ContainerExtension: extension,
				Attributes: map[string]string{"group-title": series.Stream.Attributes["group-title"]},
			}
			channel := &types.Channel{Name: series.Name + " - " + name, Streams: []*types.Stream{stream}}
			record := &types.XCRecord{
				Identity: types.XCIdentity{ContentType: types.ContentTypeEpisode, ProviderID: providerID, ProviderSource: series.Identity.ProviderSource, ProviderSeriesID: series.Identity.ProviderID, OutputID: outputID},
				Name:     name, Channel: channel, Stream: stream,
			}
			if err := sp.RegisterXCEpisode(generation, record); err != nil {
				logger.Error("{handlers/xcoutput - prepareXCSeriesEpisodes} Failed to register episode: %v", err)
				continue
			}
			episode.ID = parser.XCID(strconv.Itoa(outputID))
			valid = append(valid, episode)
		}
		detail.Episodes[season] = valid
	}
}

func accountAllowsContent(account *proxy.XCAccount, contentType types.ContentType) bool {
	return account.Allows(string(contentType))
}

func typedPlaybackChannel(record *types.XCRecord) *types.Channel {
	channel := record.Channel
	channel.Mu.RLock()
	defer channel.Mu.RUnlock()
	matching := make([]*types.Stream, 0, len(channel.Streams))
	for _, stream := range channel.Streams {
		if utils.ContentTypeOfStream(stream) == record.Identity.ContentType {
			matching = append(matching, stream)
		}
	}
	if len(matching) == 0 {
		return nil
	}
	if len(matching) == len(channel.Streams) {
		return channel
	}
	return &types.Channel{Name: channel.Name, Streams: matching, PreferredStreamIndex: channel.PreferredStreamIndex}
}

// HandleXCPlayerAPI handles /player_api.php requests from Xtream Codes compatible clients.
func HandleXCPlayerAPI(sp *proxy.StreamProxy) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		params, err := normalizeXCRequestParameters(r)
		if err != nil {
			writeXCRequestError(w, err)
			return
		}
		username := params.username
		password := params.password
		action := params.action

		w.Header().Set("Content-Type", "application/json")

		account := findXCAccount(sp, username, password)
		if account == nil {
			logger.Debug("{handlers/xcoutput - HandleXCPlayerAPI} Invalid XC credentials")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]any{
				"user_info": xcUserInfo{Auth: 0, Message: "Invalid credentials"},
			})
			return
		}

		serverInfo := buildXCServerInfo(sp.Config.BaseURL)
		userInfo := buildXCUserInfo(account)

		switch action {
		case "get_live_categories":
			if !account.Config.EnableLive {
				json.NewEncoder(w).Encode([]xcCategory{})
				return
			}
			json.NewEncoder(w).Encode(buildCategoryList(sp, "live"))

		case "get_live_streams":
			if !account.Config.EnableLive {
				json.NewEncoder(w).Encode([]xcStream{})
				return
			}
			json.NewEncoder(w).Encode(buildStreamList(sp, "live", sp.Config.BaseURL, username, password, params.categoryID))

		case "get_vod_categories":
			if !account.Config.EnableVOD {
				json.NewEncoder(w).Encode([]xcCategory{})
				return
			}
			json.NewEncoder(w).Encode(buildCategoryList(sp, "vod"))

		case "get_vod_streams":
			if !account.Config.EnableVOD {
				json.NewEncoder(w).Encode([]xcStream{})
				return
			}
			json.NewEncoder(w).Encode(buildStreamList(sp, "vod", sp.Config.BaseURL, username, password, params.categoryID))

		case "get_vod_info":
			if !account.Config.EnableVOD {
				http.NotFound(w, r)
				return
			}
			generation := sp.XCSnapshot()
			record := findXCDetailRecord(generation, types.ContentTypeVOD, params.vodID)
			if record == nil || record.Stream.Source == nil {
				http.NotFound(w, r)
				return
			}
			detail, err := parser.FetchXCVODDetail(r.Context(), sp.HttpClient, sp.Config, record.Stream.Source, sp.Cache, xcDetailCacheGeneration(account, generation.Generation), record.Identity.ProviderID)
			if err != nil {
				logger.Error("{handlers/xcoutput - HandleXCPlayerAPI} VOD detail fetch failed: %v", err)
				http.Error(w, "VOD detail unavailable", http.StatusBadGateway)
				return
			}
			if generation != sp.XCSnapshot() || !sp.AccountRegistry().IsCurrent(account) {
				http.Error(w, "Catalog refreshed; retry detail request", http.StatusServiceUnavailable)
				return
			}
			if detail.Info == nil {
				detail.Info = parser.XCMetadata{}
			}
			detail.MovieData.StreamID = parser.XCID(strconv.Itoa(record.Identity.OutputID))
			if detail.MovieData.Name == "" {
				detail.MovieData.Name = record.Name
			}
			detail.MovieData.ContainerExtension = utils.NormalizeContainerExtension(firstNonEmpty(detail.MovieData.ContainerExtension, record.Stream.ContainerExtension))
			json.NewEncoder(w).Encode(detail)

		case "get_series_categories":
			if !account.Config.EnableSeries {
				json.NewEncoder(w).Encode([]xcCategory{})
				return
			}
			json.NewEncoder(w).Encode(buildCategoryList(sp, "series"))

		case "get_series":
			if !account.Config.EnableSeries {
				json.NewEncoder(w).Encode([]xcStream{})
				return
			}
			json.NewEncoder(w).Encode(buildStreamList(sp, "series", sp.Config.BaseURL, username, password, params.categoryID))

		case "get_series_info":
			if !account.Config.EnableSeries {
				http.NotFound(w, r)
				return
			}
			generation := sp.XCSnapshot()
			record := findXCDetailRecord(generation, types.ContentTypeSeries, params.seriesID)
			if record == nil || record.Stream.Source == nil {
				http.NotFound(w, r)
				return
			}
			detail, err := parser.FetchXCSeriesDetail(r.Context(), sp.HttpClient, sp.Config, record.Stream.Source, sp.Cache, xcDetailCacheGeneration(account, generation.Generation), record.Identity.ProviderID)
			if err != nil {
				logger.Error("{handlers/xcoutput - HandleXCPlayerAPI} Series detail fetch failed: %v", err)
				http.Error(w, "Series detail unavailable", http.StatusBadGateway)
				return
			}
			if generation != sp.XCSnapshot() || !sp.AccountRegistry().IsCurrent(account) {
				http.Error(w, "Catalog refreshed; retry detail request", http.StatusServiceUnavailable)
				return
			}
			prepareXCSeriesEpisodes(sp, generation, record, &detail)
			json.NewEncoder(w).Encode(detail)

		case "get_short_epg":
			if !account.Config.EnableLive {
				json.NewEncoder(w).Encode(map[string]any{"epg_listings": []xcEPGListing{}})
				return
			}
			json.NewEncoder(w).Encode(buildXCEPGListings(sp, params.streamID, params.limit, false))

		case "get_simple_data_table":
			if !account.Config.EnableLive {
				json.NewEncoder(w).Encode(map[string]any{"epg_listings": []xcEPGListing{}})
				return
			}
			json.NewEncoder(w).Encode(buildXCEPGListings(sp, params.streamID, 0, true))

		default:
			json.NewEncoder(w).Encode(map[string]any{
				"user_info":   userInfo,
				"server_info": serverInfo,
			})
		}

		logger.Debug("{handlers/xcoutput - HandleXCPlayerAPI} Handled action '%s' for account: %s", action, account.Config.Name)
	}
}

// HandleXCGetPHP handles /get.php requests, returning a sorted M3U playlist.
func HandleXCGetPHP(sp *proxy.StreamProxy) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		params, err := normalizeXCRequestParameters(r)
		if err != nil {
			writeXCRequestError(w, err)
			return
		}
		username := params.username
		password := params.password
		outputType := params.outputType

		account := findXCAccount(sp, username, password)
		if account == nil {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		if outputType == "m3u_plus" || outputType == "m3u" {
			w.Header().Set("Content-Type", "application/x-mpegURL")
			w.Header().Set("Content-Disposition", "attachment; filename=\"playlist.m3u\"")
			writeXCM3UPlaylist(w, sp, account)
			return
		}

		http.Error(w, "Unsupported output type", http.StatusBadRequest)
	}
}

// HandleXCLiveStream handles live XC requests and canonicalizes misleading
// .m3u8 URLs before the continuous MPEG-TS response starts.
func HandleXCLiveStream(sp *proxy.StreamProxy) http.HandlerFunc {
	return handleXCStream(sp, true)
}

// HandleXCStream handles direct VOD and series stream requests from XC clients.
func HandleXCStream(sp *proxy.StreamProxy) http.HandlerFunc {
	return handleXCStream(sp, false)
}

func handleXCStream(sp *proxy.StreamProxy, redirectM3U8 bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		username := vars["username"]
		password := vars["password"]
		rawID := vars["id"]

		account := findXCAccount(sp, username, password)
		if account == nil {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		id := rawID
		if dotIdx := strings.LastIndex(rawID, "."); dotIdx != -1 {
			id = rawID[:dotIdx]
		}

		streamID, err := strconv.Atoi(id)
		if err != nil {
			http.Error(w, "Invalid stream ID", http.StatusBadRequest)
			return
		}

		contentType := types.ContentTypeLive
		switch {
		case strings.HasPrefix(r.URL.Path, "/movie/"):
			contentType = types.ContentTypeVOD
		case strings.HasPrefix(r.URL.Path, "/series/"):
			contentType = types.ContentTypeEpisode
		}
		if !accountAllowsContent(account, contentType) {
			http.Error(w, "Stream not found", http.StatusNotFound)
			return
		}
		record := sp.LookupXCRecord(contentType, streamID)
		if record == nil {
			http.Error(w, "Stream not found", http.StatusNotFound)
			return
		}
		channel := typedPlaybackChannel(record)
		if channel == nil {
			http.Error(w, "Stream not found", http.StatusNotFound)
			return
		}

		if redirectM3U8 && strings.HasSuffix(strings.ToLower(rawID), ".m3u8") {
			location := id + ".ts"
			if r.URL.RawQuery != "" {
				location += "?" + r.URL.RawQuery
			}
			w.Header().Set("Location", location)
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}

		logger.Debug("{handlers/xcoutput - HandleXCStream} XC stream: account=%s, id=%d, channel=%s",
			account.Config.Name, streamID, record.Name)

		lease, ok := sp.AccountRegistry().Acquire(account, string(contentType))
		if !ok {
			http.Error(w, "Connection limit reached", http.StatusTooManyRequests)
			return
		}
		sp.HandleRestreamingClient(w, r, channel, lease)
	}
}

// writeXCM3UPlaylist writes a sorted M3U playlist filtered by account content settings.
func writeXCM3UPlaylist(w http.ResponseWriter, sp *proxy.StreamProxy, account *proxy.XCAccount) {
	fmt.Fprintf(w, "#EXTM3U\n")

	// channel-name -> mapped epg_id; unmapped channels fall back to the dummy id
	epgMap := proxy.ChannelEPGMap()

	for _, record := range getSortedXCRecords(sp, types.ContentTypeUnknown) {
		contentType := string(record.Identity.ContentType)
		stream := record.Stream
		attrs := stream.Attributes
		extension := utils.NormalizeContainerExtension(stream.ContainerExtension)

		if contentType == "live" && !account.Config.EnableLive {
			continue
		}
		if contentType == "vod" && !account.Config.EnableVOD {
			continue
		}
		if contentType == "series" && !account.Config.EnableSeries {
			continue
		}
		if contentType == "series" {
			// Series parents are metadata records; only episode IDs are playable.
			continue
		}

		streamID := record.Identity.OutputID
		logo := attrs["tvg-logo"]
		group := attrs["group-title"]
		tvgID := proxy.EPGIDForChannel(record.Name, epgMap)

		// mapped channels advertise the raw mapped epg id on all three
		// guide-matching attributes; unmapped fall back to the dummy id
		epgAttrs := fmt.Sprintf(" tvg-id=\"%s\"", tvgID)
		if tvgID != proxy.DummyChannelID {
			epgAttrs = fmt.Sprintf(" tvg-id=\"%s\" tvg-epgid=\"%s\" tvc-guide-stationid=\"%s\"", tvgID, tvgID, tvgID)
		}

		displayName := utils.SanitizeM3UDisplayName(record.Name)
		fmt.Fprintf(w, "#EXTINF:-1%s tvg-name=\"%s\" tvg-logo=\"%s\" group-title=\"%s\",%s\n",
			epgAttrs, utils.EscapeM3UAttribute(displayName), utils.EscapeM3UAttribute(logo), utils.EscapeM3UAttribute(group), displayName)
		fmt.Fprintln(w, buildXCStreamURL(sp.Config.BaseURL, contentType, account.Config.Username, account.Config.Password, streamID, extension))
	}
}

// HandleXCXMLTV handles /xmltv.php requests, delegating to the EPG handler.
func HandleXCXMLTV(sp *proxy.StreamProxy) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		params, err := normalizeXCRequestParameters(r)
		if err != nil {
			writeXCRequestError(w, err)
			return
		}
		username := params.username
		password := params.password

		account := findXCAccount(sp, username, password)
		if account == nil {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		if !account.Config.EnableLive {
			http.NotFound(w, r)
			return
		}

		logger.Debug("{handlers/xcoutput - HandleXCXMLTV} EPG request for account: %s", account.Config.Name)
		serveEPG(sp)(w, r)
	}
}

// buildXCEPGListings resolves a stream_id to its mapped EPG channel and returns
// the XC epg_listings payload for get_short_epg / get_simple_data_table.
func buildXCEPGListings(sp *proxy.StreamProxy, streamIDStr string, limit int, markNowPlaying bool) map[string]any {
	empty := map[string]any{"epg_listings": []xcEPGListing{}}

	streamID, err := strconv.Atoi(streamIDStr)
	if err != nil {
		return empty
	}

	record := sp.LookupXCRecord(types.ContentTypeLive, streamID)
	if record == nil {
		return empty
	}

	tvgID := proxy.EPGIDForChannel(record.Name, proxy.ChannelEPGMap())

	now := time.Now()
	progs := epgindex.Programmes(tvgID, now, limit)
	if len(progs) == 0 {
		return empty
	}

	listings := make([]xcEPGListing, 0, len(progs))
	for i, p := range progs {
		l := xcEPGListing{
			ID:             strconv.Itoa(i + 1),
			EPGID:          strconv.Itoa(streamID),
			Title:          base64.StdEncoding.EncodeToString([]byte(p.Title)),
			Lang:           "",
			Start:          p.Start.Format("2006-01-02 15:04:05"),
			End:            p.Stop.Format("2006-01-02 15:04:05"),
			Description:    base64.StdEncoding.EncodeToString([]byte(p.Desc)),
			ChannelID:      tvgID,
			StartTimestamp: strconv.FormatInt(p.Start.Unix(), 10),
			StopTimestamp:  strconv.FormatInt(p.Stop.Unix(), 10),
			HasArchive:     0,
		}
		if markNowPlaying && !p.Start.After(now) && p.Stop.After(now) {
			l.NowPlaying = 1
		}
		listings = append(listings, l)
	}

	return map[string]any{"epg_listings": listings}
}
