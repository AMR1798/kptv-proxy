package parser

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"kptv-proxy/work/cache"
	"kptv-proxy/work/client"
	"kptv-proxy/work/config"
	"kptv-proxy/work/logger"
	"kptv-proxy/work/types"
	"kptv-proxy/work/utils"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.uber.org/ratelimit"
)

const xcDetailRequestTimeout = 10 * time.Second

// XCMetadata preserves optional provider fields without making incomplete
// metadata fatal to an otherwise valid detail response.
type XCMetadata map[string]any

type XCVODMovieData struct {
	StreamID           XCID   `json:"stream_id,omitempty"`
	Name               string `json:"name,omitempty"`
	ContainerExtension string `json:"container_extension,omitempty"`
}

type XCVODDetail struct {
	Info      XCMetadata     `json:"info"`
	MovieData XCVODMovieData `json:"movie_data"`
}

type XCSeriesSeason struct {
	SeasonNumber XCID   `json:"season_number,omitempty"`
	Name         string `json:"name,omitempty"`
	EpisodeCount int    `json:"episode_count,omitempty"`
	Cover        string `json:"cover,omitempty"`
}

type XCSeriesEpisode struct {
	ID                 XCID       `json:"id"`
	EpisodeNum         XCID       `json:"episode_num,omitempty"`
	Title              string     `json:"title,omitempty"`
	ContainerExtension string     `json:"container_extension,omitempty"`
	Info               XCMetadata `json:"info,omitempty"`
}

type XCSeriesDetail struct {
	Info     XCMetadata                   `json:"info"`
	Seasons  []XCSeriesSeason             `json:"seasons"`
	Episodes map[string][]XCSeriesEpisode `json:"episodes"`
}

// UnmarshalJSON skips malformed episode entries while preserving every valid
// season array. Providers commonly mix incomplete records into large payloads.
func (detail *XCSeriesDetail) UnmarshalJSON(data []byte) error {
	var raw struct {
		Info     XCMetadata                   `json:"info"`
		Seasons  []XCSeriesSeason             `json:"seasons"`
		Episodes map[string][]json.RawMessage `json:"episodes"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	detail.Info = raw.Info
	detail.Seasons = raw.Seasons
	detail.Episodes = make(map[string][]XCSeriesEpisode, len(raw.Episodes))
	for season, entries := range raw.Episodes {
		detail.Episodes[season] = []XCSeriesEpisode{}
		for _, entry := range entries {
			var episode XCSeriesEpisode
			if err := json.Unmarshal(entry, &episode); err != nil || strings.TrimSpace(string(episode.ID)) == "" {
				continue
			}
			detail.Episodes[season] = append(detail.Episodes[season], episode)
		}
	}
	return nil
}

type xcDetailCall struct {
	done chan struct{}
	data []byte
	err  error
}

var xcDetails = struct {
	sync.Mutex
	flights    map[string]*xcDetailCall
	semaphores map[string]chan struct{}
}{flights: make(map[string]*xcDetailCall), semaphores: make(map[string]chan struct{})}

// XCID normalizes XC identifiers, which providers may encode as strings or
// JSON numbers.
type XCID string

func (id *XCID) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*id = ""
		return nil
	}
	var value string
	if err := json.Unmarshal(data, &value); err == nil {
		*id = XCID(value)
		return nil
	}
	var number json.Number
	if err := json.Unmarshal(data, &number); err != nil {
		return err
	}
	*id = XCID(number.String())
	return nil
}

// XCCategory contains the flat category metadata used for display grouping.
type XCCategory struct {
	CategoryID   XCID   `json:"category_id"`
	CategoryName string `json:"category_name"`
}

// XCLiveStream represents a single live stream entry from the Xtreme Codes API response,
// containing essential metadata for live television channels including stream identification,
// categorization, and EPG (Electronic Program Guide) integration information.
// This structure maps directly to the JSON response format from the get_live_streams endpoint.
type XCLiveStream struct {
	StreamID     XCID   `json:"stream_id"`      // Provider identifier, tolerant of string or number JSON
	Name         string `json:"name"`           // Display name of the live channel for user interfaces and playlists
	CategoryID   XCID   `json:"category_id"`    // Category identifier for grouping related channels
	StreamIcon   string `json:"stream_icon"`    // URL to channel logo/icon image for display purposes
	EpgChannelID string `json:"epg_channel_id"` // EPG channel identifier for program guide integration
}

// XCSeries represents a single series entry from the Xtreme Codes API response,
// containing metadata for television series and episodic content including identification,
// categorization, and artwork information. This structure maps to the JSON response
// format from the get_series endpoint.
type XCSeries struct {
	SeriesID   XCID   `json:"series_id"`   // Provider identifier, tolerant of string or number JSON
	Name       string `json:"name"`        // Display name of the series for user interfaces and playlists
	CategoryID XCID   `json:"category_id"` // Category identifier for grouping related series content
	Cover      string `json:"cover"`       // URL to series cover artwork/poster image for display purposes
}

// XCVODStream represents a single video-on-demand stream entry from the Xtreme Codes API response,
// containing metadata for movies and other on-demand video content including identification,
// categorization, artwork, and format information. This structure maps to the JSON response
// format from the get_vod_streams endpoint.
type XCVODStream struct {
	StreamID           XCID   `json:"stream_id"`           // Provider identifier, tolerant of string or number JSON
	Name               string `json:"name"`                // Display name of the video content for user interfaces and playlists
	CategoryID         XCID   `json:"category_id"`         // Category identifier for grouping related video content
	StreamIcon         string `json:"stream_icon"`         // URL to video thumbnail/poster image for display purposes
	ContainerExtension string `json:"container_extension"` // File format extension (mp4, mkv, etc.) for container type identification
}

// processLiveBatchWorker processes a batch of XC live streams and converts API responses
// to internal Stream objects. This function is designed
// to be called by worker goroutines in a concurrent processing pool.
//
// Category labels are resolved from the provider's flat category list.
//
// Parameters:
//   - batch: slice of XCLiveStream objects to process
//   - categoryMap: provider category ID to display-name mapping
//   - source: source configuration containing credentials and connection parameters
//
// Returns:
//   - []*types.Stream: slice of processed streams ready for channel aggregation
func processLiveBatchWorker(batch []XCLiveStream, categoryMap map[string]string, source *config.SourceConfig) []*types.Stream {
	results := make([]*types.Stream, 0, len(batch))
	logger.Debug("{parser/xtremecodes - processLiveBatchWorker} process the live batch")

	// loop the stream objects
	for _, stream := range batch {
		streamURL, err := utils.AppendURLPath(source.URL, "live", source.Username, source.Password, string(stream.StreamID)+".ts")
		if err != nil {
			logger.Error("{parser/xtremecodes - processLiveBatchWorker} Invalid provider URL for source %s", source.Name)
			continue
		}
		group := categoryName(categoryMap, string(stream.CategoryID), "live")

		// create the stream group
		s := &types.Stream{
			URL:            streamURL,
			Name:           stream.Name,
			Source:         source,
			ContentType:    types.ContentTypeLive,
			ProviderID:     string(stream.StreamID),
			ProviderSource: source.URL,
			Attributes: map[string]string{
				"tvg-name":    stream.Name,
				"group-title": group,
				"tvg-id":      string(stream.StreamID),
				"category-id": string(stream.CategoryID),
			},
		}

		// if there's a logo or tvg-id
		if stream.StreamIcon != "" {
			s.Attributes["tvg-logo"] = stream.StreamIcon
		}
		if stream.EpgChannelID != "" {
			s.Attributes["tvg-id"] = stream.EpgChannelID
		}
		logger.Debug("{parser/xtremecodes - processLiveBatchWorker} process the live stream %v", stream.Name)
		results = append(results, s)
	}
	logger.Debug("{parser/xtremecodes - processLiveBatchWorker} live batch results")
	return results
}

// processSeriesBatchWorker processes a batch of XC series and converts API responses to
// internal Stream objects. This function is designed
// to be called by worker goroutines in a concurrent processing pool.
//
// Category labels are resolved from the provider's flat category list.
//
// Parameters:
//   - batch: slice of XCSeries objects to process
//   - categoryMap: provider category ID to display-name mapping
//   - source: source configuration containing credentials and connection parameters
//
// Returns:
//   - []*types.Stream: slice of processed series streams ready for channel aggregation
func processSeriesBatchWorker(batch []XCSeries, categoryMap map[string]string, source *config.SourceConfig) []*types.Stream {
	results := make([]*types.Stream, 0, len(batch))
	logger.Debug("{parser/xtremecodes - processSeriesBatchWorker} process the series batch")

	// loop the stream objects
	for _, serie := range batch {
		// setup the stream url
		streamURL, err := utils.AppendURLPath(source.URL, "series", source.Username, source.Password, string(serie.SeriesID)+".ts")
		if err != nil {
			logger.Error("{parser/xtremecodes - processSeriesBatchWorker} Invalid provider URL for source %s", source.Name)
			continue
		}

		// setup the stream
		s := &types.Stream{
			URL:                streamURL,
			Name:               serie.Name,
			Source:             source,
			ContentType:        types.ContentTypeSeries,
			ProviderID:         string(serie.SeriesID),
			ProviderSource:     source.URL,
			ContainerExtension: "ts",
			Attributes: map[string]string{
				"tvg-name":    serie.Name,
				"group-title": categoryName(categoryMap, string(serie.CategoryID), "series"),
				"tvg-id":      string(serie.SeriesID),
				"category-id": string(serie.CategoryID),
			},
		}
		// if theres a logo
		if serie.Cover != "" {
			s.Attributes["tvg-logo"] = serie.Cover
		}
		logger.Debug("{parser/xtremecodes - processSeriesBatchWorker} process the live stream %v", serie.Name)
		results = append(results, s)
	}
	logger.Debug("{parser/xtremecodes - processSeriesBatchWorker} series batch results")
	return results
}

func processVODBatchWorker(batch []XCVODStream, categoryMap map[string]string, source *config.SourceConfig) []*types.Stream {
	results := make([]*types.Stream, 0, len(batch))
	for _, stream := range batch {
		extension := utils.NormalizeContainerExtension(stream.ContainerExtension)
		streamURL, err := utils.AppendURLPath(source.URL, "movie", source.Username, source.Password, string(stream.StreamID)+"."+extension)
		if err != nil {
			logger.Error("{parser/xtremecodes - processVODBatchWorker} Invalid provider URL for source %s", source.Name)
			continue
		}
		result := &types.Stream{
			URL:                streamURL,
			Name:               stream.Name,
			Source:             source,
			ContentType:        types.ContentTypeVOD,
			ProviderID:         string(stream.StreamID),
			ProviderSource:     source.URL,
			ContainerExtension: extension,
			Attributes: map[string]string{
				"tvg-name":    stream.Name,
				"group-title": categoryName(categoryMap, string(stream.CategoryID), "vod"),
				"tvg-id":      string(stream.StreamID),
				"category-id": string(stream.CategoryID),
			},
		}
		if stream.StreamIcon != "" {
			result.Attributes["tvg-logo"] = stream.StreamIcon
		}
		results = append(results, result)
	}
	return results
}

func categoryName(categoryMap map[string]string, categoryID, fallback string) string {
	if name, ok := categoryMap[categoryID]; ok && strings.TrimSpace(name) != "" {
		return name
	}
	return fallback
}

func buildCategoryMap(categories []XCCategory) map[string]string {
	categoryMap := make(map[string]string, len(categories))
	for _, category := range categories {
		if strings.TrimSpace(string(category.CategoryID)) == "" || strings.TrimSpace(category.CategoryName) == "" {
			continue
		}
		categoryMap[string(category.CategoryID)] = category.CategoryName
	}
	return categoryMap
}

func processXCBatches[T any](ctx context.Context, items []T, workers int, process func([]T) []*types.Stream) []*types.Stream {
	if len(items) == 0 {
		return nil
	}
	if workers < 1 {
		workers = 1
	}
	const batchSize = 1000
	type batchJob struct {
		index int
		items []T
	}
	type batchResult struct {
		index   int
		streams []*types.Stream
	}
	workChan := make(chan batchJob)
	resultsChan := make(chan batchResult)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case job, ok := <-workChan:
					if !ok {
						return
					}
					results := process(job.items)
					select {
					case resultsChan <- batchResult{index: job.index, streams: results}:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
	}
	go func() {
		defer close(workChan)
		for start := 0; start < len(items); start += batchSize {
			end := start + batchSize
			if end > len(items) {
				end = len(items)
			}
			select {
			case workChan <- batchJob{index: start / batchSize, items: items[start:end]}:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		wg.Wait()
		close(resultsChan)
	}()
	batchCount := (len(items) + batchSize - 1) / batchSize
	ordered := make([][]*types.Stream, batchCount)
	for result := range resultsChan {
		if result.index >= 0 && result.index < len(ordered) {
			ordered[result.index] = result.streams
		}
	}
	var results []*types.Stream
	for _, batch := range ordered {
		results = append(results, batch...)
	}
	return results
}

// ParseXtremeCodesAPI fetches and parses content from all three Xtreme Codes API endpoints
// (live streams, series, and VOD), aggregating the results into a unified stream collection
// with proper URL construction and metadata mapping. This function serves as the primary
// entry point for Xtreme Codes API integration, replacing standard M3U8 parsing when
// authentication credentials are available.
//
// The parsing process implements comprehensive error handling, rate limiting, and debug
// logging while constructing appropriate stream URLs for each content type using the
// Xtreme Codes URL format specifications. ContentType carries semantic classification;
// group-title preserves the provider category name, with a type-name fallback.
//
// Parameters:
//   - httpClient: configured HTTP client for API requests with header support
//   - logger: application logger for debugging and progress reporting
//   - cfg: application configuration containing debug settings and URL obfuscation preferences
//   - source: source configuration with URL, credentials, and connection parameters
//   - rateLimiter: rate limiter for controlling API request frequency to prevent server overload
//
// Returns:
//   - []*types.Stream: aggregated collection of streams from all three API endpoints
func ParseXtremeCodesAPI(httpClient *client.HeaderSettingClient, cfg *config.Config, source *config.SourceConfig, rateLimiter ratelimit.Limiter, cache *cache.Cache) []*types.Stream {
	streams, _ := ParseXtremeCodesAPIWithStatus(httpClient, cfg, source, rateLimiter, cache)
	return streams
}

// ParseXtremeCodesAPIWithStatus distinguishes a complete generation, including
// valid-empty catalogs, from a partial or failed provider fetch.
func ParseXtremeCodesAPIWithStatus(httpClient *client.HeaderSettingClient, cfg *config.Config, source *config.SourceConfig, rateLimiter ratelimit.Limiter, cache *cache.Cache) ([]*types.Stream, bool) {
	logger.Debug("{parser/xtremecodes - ParseXtremeCodesAPI} from %s with optimized batch processing", utils.LogURL(cfg, source.URL))

	cacheKey := xcCacheKey(source)
	if cached, found := cache.GetXCData(cacheKey); found {
		logger.Debug("{parser/xtremecodes - ParseXtremeCodesAPI} Using cached XC API data for %s", source.Name)
		var streams []*types.Stream
		if err := json.Unmarshal([]byte(cached), &streams); err == nil {
			return streams, true
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	var fetchWG sync.WaitGroup
	fetchWG.Add(6)
	var liveCategories, seriesCategories, vodCategories []XCCategory
	var liveStreams []XCLiveStream
	var series []XCSeries
	var vodStreams []XCVODStream
	var liveCategoryOK, seriesCategoryOK, vodCategoryOK bool
	var liveOK, seriesOK, vodOK bool
	go func() {
		defer fetchWG.Done()
		liveCategories, liveCategoryOK = fetchXCCategories(ctx, httpClient, cfg, source, rateLimiter, types.ContentTypeLive)
	}()
	go func() {
		defer fetchWG.Done()
		seriesCategories, seriesCategoryOK = fetchXCCategories(ctx, httpClient, cfg, source, rateLimiter, types.ContentTypeSeries)
	}()
	go func() {
		defer fetchWG.Done()
		vodCategories, vodCategoryOK = fetchXCCategories(ctx, httpClient, cfg, source, rateLimiter, types.ContentTypeVOD)
	}()
	go func() {
		defer fetchWG.Done()
		liveStreams, liveOK = fetchXCLiveStreamsWithContext(ctx, httpClient, cfg, source, rateLimiter)
	}()
	go func() {
		defer fetchWG.Done()
		series, seriesOK = fetchXCSeriesWithContext(ctx, httpClient, cfg, source, rateLimiter)
	}()
	go func() {
		defer fetchWG.Done()
		vodStreams, vodOK = fetchXCVODStreamsWithContext(ctx, httpClient, cfg, source, rateLimiter)
	}()
	fetchWG.Wait()

	liveCategoryMap := buildCategoryMap(liveCategories)
	seriesCategoryMap := buildCategoryMap(seriesCategories)
	vodCategoryMap := buildCategoryMap(vodCategories)

	allStreams := processXCBatches(ctx, liveStreams, cfg.WorkerThreads, func(batch []XCLiveStream) []*types.Stream {
		return processLiveBatchWorker(batch, liveCategoryMap, source)
	})
	allStreams = append(allStreams, processXCBatches(ctx, series, cfg.WorkerThreads, func(batch []XCSeries) []*types.Stream {
		return processSeriesBatchWorker(batch, seriesCategoryMap, source)
	})...)
	allStreams = append(allStreams, processXCBatches(ctx, vodStreams, cfg.WorkerThreads, func(batch []XCVODStream) []*types.Stream {
		return processVODBatchWorker(batch, vodCategoryMap, source)
	})...)

	logger.Debug("{parser/xtremecodes - ParseXtremeCodesAPI} XC API parsing complete: %d total streams", len(allStreams))
	complete := ctx.Err() == nil && liveCategoryOK && seriesCategoryOK && vodCategoryOK && liveOK && seriesOK && vodOK
	if complete {
		if len(allStreams) > 0 {
			if data, err := json.Marshal(allStreams); err == nil {
				cache.SetXCData(cacheKey, string(data))
				logger.Debug("{parser/xtremecodes - ParseXtremeCodesAPI} Cached %d streams for %s", len(allStreams), source.Name)
			}
		} else {
			logger.Debug("{parser/xtremecodes - ParseXtremeCodesAPI} Valid-empty XC catalog for %s was not cached", source.Name)
		}
	} else {
		logger.Debug("{parser/xtremecodes - ParseXtremeCodesAPI} Skipping cache after incomplete XC fetch (live-category=%t, series-category=%t, vod-category=%t, live=%t, series=%t, vod=%t)", liveCategoryOK, seriesCategoryOK, vodCategoryOK, liveOK, seriesOK, vodOK)
	}
	return allStreams, complete
}

func xcCacheKey(source *config.SourceConfig) string {
	identity := sha256.Sum256([]byte(xcSourceIdentity(source)))
	return fmt.Sprintf("xc:v2:%x", identity)
}

// fetchXCCategories retrieves the category list for one XC content type.
func fetchXCCategories(ctx context.Context, httpClient *client.HeaderSettingClient, cfg *config.Config, source *config.SourceConfig, rateLimiter ratelimit.Limiter, contentType types.ContentType) ([]XCCategory, bool) {
	action := ""
	switch contentType {
	case types.ContentTypeLive:
		action = "get_live_categories"
	case types.ContentTypeSeries:
		action = "get_series_categories"
	case types.ContentTypeVOD:
		action = "get_vod_categories"
	default:
		return nil, false
	}
	if rateLimiter != nil {
		rateLimiter.Take()
	}
	categories, err := fetchXCDataWithContext[XCCategory](ctx, httpClient, cfg, source, action)
	if err != nil {
		logger.Error("{parser/xtremecodes - fetchXCCategories} Failed to fetch %s categories from %s: %v", contentType, utils.LogURL(cfg, source.URL), err)
		return nil, false
	}
	return categories, true
}

func fetchXCVODStreamsWithContext(ctx context.Context, httpClient *client.HeaderSettingClient, cfg *config.Config, source *config.SourceConfig, rateLimiter ratelimit.Limiter) ([]XCVODStream, bool) {
	if rateLimiter != nil {
		rateLimiter.Take()
	}
	streams, err := fetchXCDataWithContext[XCVODStream](ctx, httpClient, cfg, source, "get_vod_streams")
	if err != nil {
		logger.Error("{parser/xtremecodes - fetchXCVODStreamsWithContext} Failed to fetch XC VOD streams from %s: %v", utils.LogURL(cfg, source.URL), err)
		return nil, false
	}
	logger.Debug("{parser/xtremecodes - fetchXCVODStreamsWithContext} Successfully fetched %d VOD streams from XC API", len(streams))
	return streams, true
}

// fetchXCLiveStreamsWithContext retrieves live television stream data with context support
func fetchXCLiveStreamsWithContext(ctx context.Context, httpClient *client.HeaderSettingClient, cfg *config.Config, source *config.SourceConfig, rateLimiter ratelimit.Limiter) ([]XCLiveStream, bool) {
	// Apply rate limiting before making API request to prevent server overload
	if rateLimiter != nil {
		rateLimiter.Take()
		logger.Debug("{parser/xtremecodes - fetchXCLiveStreamsWithContext} Applied rate limit for XC live streams request: %s", source.Name)
	}

	// Execute generic API data fetching with proper error handling
	streams, err := fetchXCDataWithContext[XCLiveStream](ctx, httpClient, cfg, source, "get_live_streams")
	if err != nil {
		logger.Error("{parser/xtremecodes - fetchXCLiveStreamsWithContext} Failed to fetch XC live streams from %s: %v", utils.LogURL(cfg, source.URL), err)
		return nil, false
	}

	logger.Debug("{parser/xtremecodes - fetchXCLiveStreamsWithContext} Successfully fetched %d live streams from XC API", len(streams))
	return streams, true
}

// fetchXCSeriesWithContext retrieves television series data with context support
func fetchXCSeriesWithContext(ctx context.Context, httpClient *client.HeaderSettingClient, cfg *config.Config, source *config.SourceConfig, rateLimiter ratelimit.Limiter) ([]XCSeries, bool) {
	// Apply rate limiting before making API request to prevent server overload
	if rateLimiter != nil {
		rateLimiter.Take()
		logger.Debug("{parser/xtremecodes - fetchXCSeriesWithContext} Applied rate limit for XC series request: %s", source.Name)
	}

	// Execute generic API data fetching with proper error handling
	series, err := fetchXCDataWithContext[XCSeries](ctx, httpClient, cfg, source, "get_series")
	if err != nil {
		logger.Error("{parser/xtremecodes - fetchXCSeriesWithContext} Failed to fetch XC series from %s: %v", utils.LogURL(cfg, source.URL), err)
		return nil, false
	}
	logger.Debug("{parser/xtremecodes - fetchXCSeriesWithContext} Successfully fetched %d series from XC API", len(series))
	return series, true
}

// fetchXCDataWithContext implements context-aware HTTP request handler for Xtreme Codes API endpoints
func fetchXCDataWithContext[T any](ctx context.Context, httpClient *client.HeaderSettingClient, cfg *config.Config, source *config.SourceConfig, action string) ([]T, error) {
	req, err := buildXCProviderRequest(ctx, source, action)
	if err != nil {
		logger.Error("{parser/xtremecodes - fetchXCDataWithContext} Failed to create XC API request for source %s", source.Name)
		return nil, err
	}

	// set the keep-alive
	req.Header.Set("Connection", "keep-alive")

	// do the request with the right headers
	resp, err := httpClient.DoWithHeaders(req, source.UserAgent, source.ReqOrigin, source.ReqReferrer)
	if err != nil {
		logger.Error("{parser/xtremecodes - fetchXCDataWithContext} XC API request failed for %s", utils.LogURL(cfg, source.URL))
		return nil, fmt.Errorf("XC API request failed")
	}

	// close the connection
	defer func() {
		resp.Body.Close()
		logger.Debug("{parser/xtremecodes - fetchXCDataWithContext} Closed XC API connection for: %s", utils.LogURL(cfg, source.URL))

	}()

	// invalid response
	if resp.StatusCode != http.StatusOK {
		logger.Error("{parser/xtremecodes - fetchXCDataWithContext} XC API returned HTTP %d for: %s", resp.StatusCode, utils.LogURL(cfg, source.URL))
		return nil, fmt.Errorf("XC API returned HTTP %d", resp.StatusCode)
	}

	decoder := json.NewDecoder(resp.Body)
	var data []T

	// decode the response
	if err := decoder.Decode(&data); err != nil {
		logger.Error("{parser/xtremecodes - fetchXCDataWithContext} Failed to parse XC API JSON response: %v", err)
		return nil, err
	}

	logger.Debug("{parser/xtremecodes - fetchXCDataWithContext} Successfully parsed %d items from XC API response", len(data))
	return data, nil
}

func FetchXCVODDetail(ctx context.Context, httpClient *client.HeaderSettingClient, cfg *config.Config, source *config.SourceConfig, cacheInstance *cache.Cache, generation, providerID string) (XCVODDetail, error) {
	var detail XCVODDetail
	err := fetchXCDetail(ctx, httpClient, cfg, source, cacheInstance, generation, types.ContentTypeVOD, providerID, &detail)
	return detail, err
}

func FetchXCSeriesDetail(ctx context.Context, httpClient *client.HeaderSettingClient, cfg *config.Config, source *config.SourceConfig, cacheInstance *cache.Cache, generation, providerID string) (XCSeriesDetail, error) {
	var detail XCSeriesDetail
	err := fetchXCDetail(ctx, httpClient, cfg, source, cacheInstance, generation, types.ContentTypeSeries, providerID, &detail)
	return detail, err
}

func fetchXCDetail(ctx context.Context, httpClient *client.HeaderSettingClient, cfg *config.Config, source *config.SourceConfig, cacheInstance *cache.Cache, generation string, contentType types.ContentType, providerID string, destination any) error {
	action := "get_vod_info"
	idParameter := "vod_id"
	if contentType == types.ContentTypeSeries {
		action = "get_series_info"
		idParameter = "series_id"
	}
	key := xcDetailCacheKey(source, generation, contentType, providerID)
	if cached, ok := cacheInstance.GetXCData(key); ok {
		return decodeXCDetail([]byte(cached), destination)
	}

	xcDetails.Lock()
	if call := xcDetails.flights[key]; call != nil {
		xcDetails.Unlock()
		select {
		case <-call.done:
			if call.err != nil {
				return call.err
			}
			return decodeXCDetail(call.data, destination)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	call := &xcDetailCall{done: make(chan struct{})}
	xcDetails.flights[key] = call
	semaphore := xcDetailSemaphoreLocked(source)
	xcDetails.Unlock()

	defer func() {
		xcDetails.Lock()
		delete(xcDetails.flights, key)
		close(call.done)
		xcDetails.Unlock()
	}()

	requestCtx, cancel := context.WithTimeout(ctx, xcDetailRequestTimeout)
	defer cancel()
	select {
	case semaphore <- struct{}{}:
		defer func() { <-semaphore }()
	case <-requestCtx.Done():
		call.err = requestCtx.Err()
		return call.err
	}

	req, err := buildXCProviderRequestWithParameters(requestCtx, source, action, map[string]string{idParameter: providerID})
	if err != nil {
		call.err = err
		return err
	}
	resp, err := httpClient.DoWithHeaders(req, source.UserAgent, source.ReqOrigin, source.ReqReferrer)
	if err != nil {
		call.err = fmt.Errorf("XC detail request failed")
		return call.err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		call.err = fmt.Errorf("XC detail request returned HTTP %d", resp.StatusCode)
		return call.err
	}
	const maxDetailBytes = 16 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDetailBytes+1))
	if err != nil {
		call.err = err
		return err
	}
	if len(data) > maxDetailBytes {
		call.err = fmt.Errorf("XC detail response exceeds size limit")
		return call.err
	}
	if err := decodeXCDetail(data, destination); err != nil {
		call.err = err
		return err
	}
	call.data = append([]byte(nil), data...)
	cacheInstance.SetXCData(key, string(data))
	return nil
}

func decodeXCDetail(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("unexpected trailing XC detail data")
		}
		return err
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return err
	}
	if _, failed := object["error"]; failed {
		return fmt.Errorf("XC provider returned an error payload")
	}
	return nil
}

func xcDetailSemaphoreLocked(source *config.SourceConfig) chan struct{} {
	key := xcSourceIdentity(source)
	if semaphore := xcDetails.semaphores[key]; semaphore != nil {
		return semaphore
	}
	limit := source.MaxConnections
	if limit < 1 {
		limit = 2
	}
	if limit > 8 {
		limit = 8
	}
	semaphore := make(chan struct{}, limit)
	xcDetails.semaphores[key] = semaphore
	return semaphore
}

func xcDetailCacheKey(source *config.SourceConfig, generation string, contentType types.ContentType, providerID string) string {
	identity := sha256.Sum256([]byte(xcSourceIdentity(source) + "\x00" + generation + "\x00" + string(contentType) + "\x00" + providerID))
	return fmt.Sprintf("xc-detail:v1:%x", identity)
}

func xcSourceIdentity(source *config.SourceConfig) string {
	return source.URL + "\x00" + source.Username + "\x00" + source.Password
}

func buildXCProviderRequest(ctx context.Context, source *config.SourceConfig, action string) (*http.Request, error) {
	return buildXCProviderRequestWithParameters(ctx, source, action, nil)
}

func buildXCProviderRequestWithParameters(ctx context.Context, source *config.SourceConfig, action string, parameters map[string]string) (*http.Request, error) {
	endpoint, err := utils.AppendURLPath(source.URL, "player_api.php")
	if err != nil {
		return nil, fmt.Errorf("invalid XC provider URL")
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid XC provider URL")
	}
	query := u.Query()
	query.Set("username", source.Username)
	query.Set("password", source.Password)
	query.Set("action", action)
	for key, value := range parameters {
		query.Set(key, value)
	}
	u.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("invalid XC provider request")
	}
	return req, nil
}
