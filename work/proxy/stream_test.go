package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"kptv-proxy/work/config"
	"kptv-proxy/work/types"
	"kptv-proxy/work/utils"

	"github.com/puzpuzpuz/xsync/v3"
)

func TestBuildXCSnapshotKeepsTypedSameNameRecords(t *testing.T) {
	channel := &types.Channel{Name: "Shared", Streams: []*types.Stream{
		{Name: "Shared", ContentType: types.ContentTypeLive, ProviderID: "11", ProviderSource: "live-source"},
		{Name: "Shared", ContentType: types.ContentTypeVOD, ProviderID: "22", ProviderSource: "vod-source", ContainerExtension: "mp4"},
		{Name: "Shared", ContentType: types.ContentTypeEpisode, ProviderID: "33", ProviderSource: "series-source", ContainerExtension: "mkv"},
	}}
	snapshot, err := BuildXCSnapshot([]*types.Channel{channel})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Records) != 3 {
		t.Fatalf("snapshot has %d records, want 3", len(snapshot.Records))
	}
	outputID := XCOutputID(channel.Name)
	if snapshot.Lookup(types.ContentTypeLive, outputID) == nil || snapshot.Lookup(types.ContentTypeVOD, outputID) == nil || snapshot.Lookup(types.ContentTypeEpisode, outputID) == nil {
		t.Fatalf("typed records were not independently indexed: %#v", snapshot.Records)
	}
}

func TestBuildXCSnapshotRejectsOutputIDCollision(t *testing.T) {
	name := "Collision"
	first := &types.Channel{Name: name, Streams: []*types.Stream{{Name: name, ContentType: types.ContentTypeLive}}}
	second := &types.Channel{Name: name + " copy", Streams: []*types.Stream{{Name: name + " copy", ContentType: types.ContentTypeLive}}}
	// Force a collision without depending on a particular hash implementation.
	snapshot, err := buildXCSnapshotWithIDs([]*types.Channel{first, second}, func(string) int { return 42 })
	if err == nil || snapshot != nil {
		t.Fatalf("collision result = snapshot %#v, err %v; want an error and no snapshot", snapshot, err)
	}
}

func TestRegisterXCEpisodeRejectsDerivedIDCollision(t *testing.T) {
	sp := New(&config.Config{}, nil, nil, nil, nil)
	generation := sp.XCSnapshot()
	first := &types.XCRecord{Identity: types.XCIdentity{ContentType: types.ContentTypeEpisode, OutputID: 42, ProviderSource: "source-a", ProviderSeriesID: "series-a", ProviderID: "episode"}}
	second := &types.XCRecord{Identity: types.XCIdentity{ContentType: types.ContentTypeEpisode, OutputID: 42, ProviderSource: "source-b", ProviderSeriesID: "series-b", ProviderID: "episode"}}
	if err := sp.RegisterXCEpisode(generation, first); err != nil {
		t.Fatalf("first episode registration failed: %v", err)
	}
	if err := sp.RegisterXCEpisode(generation, second); err == nil {
		t.Fatal("conflicting episode registration did not return an error")
	}
	if got := sp.LookupXCRecord(types.ContentTypeEpisode, 42); got != first {
		t.Fatalf("collision replaced first record with %#v", got)
	}
}

func TestChannelStorePublishesChannelAndXCGenerationTogether(t *testing.T) {
	channel := &types.Channel{Name: "Live", Streams: []*types.Stream{{Name: "Live", ContentType: types.ContentTypeLive, ProviderID: "101"}}}
	snapshot, err := BuildXCSnapshot([]*types.Channel{channel})
	if err != nil {
		t.Fatal(err)
	}
	channels := xsync.NewMapOf[string, *types.Channel]()
	channels.Store(channel.Name, channel)
	store := newChannelStore()
	store.replace(channels, snapshot)

	published, exists := store.Load(channel.Name)
	record := store.xcSnapshot().Lookup(types.ContentTypeLive, XCOutputID(channel.Name))
	if !exists || published != channel || record == nil || record.Channel != published || record.Identity.ProviderID != "101" {
		t.Fatalf("published generation is inconsistent: channel=%p record=%#v", published, record)
	}
}

func TestImportStreamsWithoutSourcesPublishesEmptyGeneration(t *testing.T) {
	channel := &types.Channel{Name: "Old"}
	snapshot, err := BuildXCSnapshot([]*types.Channel{channel})
	if err != nil {
		t.Fatal(err)
	}
	channels := xsync.NewMapOf[string, *types.Channel]()
	channels.Store(channel.Name, channel)

	sp := &StreamProxy{Config: &config.Config{}, Channels: newChannelStore()}
	sp.Channels.replace(channels, snapshot)
	sp.ImportStreams()

	if _, exists := sp.Channels.Load(channel.Name); exists || len(sp.XCSnapshot().Records) != 0 {
		t.Fatal("ImportStreams() retained the previous generation with no configured sources")
	}
}

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

func TestPlaylistCacheKeyDoesNotContainCredentials(t *testing.T) {
	account := XCAccount{Config: config.XCOutputAccount{ID: 12, Username: "user-secret", Password: "pass-secret"}, Generation: 3}
	key := playlistCacheKey(account, 9, "News")
	if strings.Contains(key, account.Config.Username) || strings.Contains(key, account.Config.Password) {
		t.Fatalf("playlist cache key contains credentials: %q", key)
	}
}

func TestPlaylistCacheKeyIsolatesAccountAndAuthorizationGeneration(t *testing.T) {
	first := XCAccount{Config: config.XCOutputAccount{ID: 1}, Generation: 4}
	second := XCAccount{Config: config.XCOutputAccount{ID: 2}, Generation: 4}
	if playlistCacheKey(first, 10, "") == playlistCacheKey(second, 10, "") {
		t.Fatal("different stable accounts shared a playlist cache key")
	}
	second.Config.ID = 1
	second.Generation = 5
	if playlistCacheKey(first, 10, "") == playlistCacheKey(second, 10, "") {
		t.Fatal("authorization update retained a playlist cache key")
	}
}

func TestGeneratePlaylistUsesEnabledStreamFromMixedChannel(t *testing.T) {
	account := &XCAccount{Config: config.XCOutputAccount{EnableLive: true}}
	stream := firstAllowedStream([]*types.Stream{
		{Name: "Mixed", ContentType: types.ContentTypeVOD, Attributes: map[string]string{"group-title": "Movies"}},
		{Name: "Mixed", ContentType: types.ContentTypeLive, Attributes: map[string]string{"group-title": "Live"}},
	}, account)
	if stream == nil || stream.ContentType != types.ContentTypeLive {
		t.Fatalf("selected mixed-channel stream = %#v, want enabled live stream", stream)
	}
}

func TestXCAccountRegistryAtomicAdmissionAndDoubleRelease(t *testing.T) {
	registry := NewXCAccountRegistry([]config.XCOutputAccount{{ID: 1, Username: "u", Password: "p", MaxConnections: 8, EnableLive: true}})
	account, ok := registry.Authenticate("u", "p")
	if !ok {
		t.Fatal("account was not authenticated")
	}

	const attempts = 9
	start := make(chan struct{})
	results := make(chan *XCPlaybackLease, attempts)
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			lease, _ := account.TryAcquirePlayback()
			results <- lease
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	leases := make([]*XCPlaybackLease, 0, attempts)
	for lease := range results {
		if lease != nil {
			leases = append(leases, lease)
		}
	}
	if len(leases) != 8 || account.ActiveConnections() != 8 {
		t.Fatalf("admitted=%d active=%d, want 8", len(leases), account.ActiveConnections())
	}
	for _, lease := range leases {
		lease.Release()
		lease.Release()
	}
	if got := account.ActiveConnections(); got != 0 {
		t.Fatalf("active after double release = %d, want 0", got)
	}
}

func TestXCAccountRegistryPreservesLeasesAcrossUpdateAndDelete(t *testing.T) {
	registry := NewXCAccountRegistry([]config.XCOutputAccount{{ID: 7, Username: "old", Password: "p", MaxConnections: 1, EnableLive: true}})
	old, _ := registry.Authenticate("old", "p")
	lease, ok := old.TryAcquirePlayback()
	if !ok {
		t.Fatal("initial playback was not admitted")
	}

	registry.Replace([]config.XCOutputAccount{{ID: 7, Username: "new", Password: "p2", MaxConnections: 2, EnableVOD: true}})
	if _, ok := registry.Authenticate("old", "p"); ok {
		t.Fatal("old credentials remained current")
	}
	updated, ok := registry.Authenticate("new", "p2")
	if !ok || updated.ActiveConnections() != 1 || updated.Generation == old.Generation {
		t.Fatalf("updated account = %#v active=%d", updated, updated.ActiveConnections())
	}
	registry.Replace(nil)
	if _, ok := registry.Authenticate("new", "p2"); ok {
		t.Fatal("deleted account admitted a new request")
	}
	lease.Release()
	if got := old.ActiveConnections(); got != 0 {
		t.Fatalf("deleted account retained %d active leases", got)
	}
}

func TestHandleRestreamingClientBlocksUntilDisconnect(t *testing.T) {
	sp := New(&config.Config{
		MaxConnectionsToApp: 10,
		BufferSizePerStream: 1,
		XCOutputAccounts: []config.XCOutputAccount{{
			ID: 1, Username: "u", Password: "p", MaxConnections: 1, EnableLive: true,
		}},
	}, nil, nil, nil, nil)
	account, ok := sp.AccountRegistry().Authenticate("u", "p")
	if !ok {
		t.Fatal("account was not authenticated")
	}
	lease, ok := sp.AccountRegistry().Acquire(account, "live")
	if !ok {
		t.Fatal("playback lease was not acquired")
	}
	channel := &types.Channel{Name: "empty"}
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/stream", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		sp.HandleRestreamingClient(httptest.NewRecorder(), req, channel, lease)
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("handler returned before disconnect")
	default:
	}
	if got := account.ActiveConnections(); got != 1 {
		t.Fatalf("active connections while handler blocks = %d, want 1", got)
	}
	cancel()
	<-done
	if got := account.ActiveConnections(); got != 0 {
		t.Fatalf("active connections after disconnect = %d, want 0", got)
	}
}

func TestPlaylistStreamURLSegmentsAreEscapedOnce(t *testing.T) {
	username := "user +&=%?#/雪"
	password := "pass +&=%?#/雪"
	got, err := utils.AppendURLPath("http://proxy/base", "s", username, password, "channel")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Path != "/base/s/"+username+"/"+password+"/channel" || strings.Contains(parsed.EscapedPath(), "%2525") {
		t.Fatalf("playlist URL path = %q (%q)", parsed.Path, parsed.EscapedPath())
	}
}
