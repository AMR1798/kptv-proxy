package restream

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"kptv-proxy/work/client"
	"kptv-proxy/work/config"
	"kptv-proxy/work/types"

	"github.com/puzpuzpuz/xsync/v3"
)

func TestGetStreamVariantsReturnsMediaPlaylistResponseForStreaming(t *testing.T) {
	const playlist = "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6,\nsegment.ts\n"
	segment := []byte("MPEG-TS segment data")
	var playlistRequests atomic.Int32
	var segmentRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/segment.ts" {
			segmentRequests.Add(1)
			w.Header().Set("Content-Type", "video/mp2t")
			_, _ = w.Write(segment)
			return
		}

		playlistRequests.Add(1)
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte(playlist))
	}))
	defer server.Close()

	source := &config.SourceConfig{
		Name:           "test HLS source",
		URL:            server.URL + "/stream.m3u8",
		MaxConnections: 1,
	}
	channel := &types.Channel{
		Name: "test channel",
		Streams: []*types.Stream{{
			URL:    source.URL,
			Source: source,
		}},
	}
	cfg := &config.Config{BufferSizePerStream: 1}
	restreamer := NewRestreamer(channel, 1024*1024, client.NewHeaderSettingClient(time.Second), cfg, nil)
	restreamer.Clients = xsync.NewMapOf[string, *types.RestreamClient]()
	restreamer.Clients.Store("test client", &types.RestreamClient{
		Done:      make(chan bool),
		WriteChan: make(chan []byte, 1),
	})

	_, isMaster, response, cancel, err := restreamer.getStreamVariants(source.URL, source)
	if err != nil {
		t.Fatalf("getStreamVariants() error = %v", err)
	}
	if isMaster {
		t.Fatal("getStreamVariants() classified media playlist as master")
	}
	if response == nil {
		t.Fatal("getStreamVariants() returned nil response for media playlist")
	}
	if cancel == nil {
		t.Fatal("getStreamVariants() returned nil cancel function for media playlist")
	}
	defer cancel()

	result := make(chan int64, 1)
	go func() {
		_, bytesTransferred := restreamer.sniffAndStreamResponse(response, source.URL, source)
		result <- bytesTransferred
	}()

	streamClient, _ := restreamer.Clients.Load("test client")
	select {
	case got := <-streamClient.WriteChan:
		if string(got) != string(segment) {
			t.Fatalf("streamed segment = %q, want %q", got, segment)
		}
		restreamer.CancelStream()
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for HLS segment")
	}

	select {
	case bytesTransferred := <-result:
		if bytesTransferred != int64(len(segment)) {
			t.Fatalf("bytes transferred = %d, want %d", bytesTransferred, len(segment))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for HLS stream shutdown")
	}

	if got := playlistRequests.Load(); got != 2 {
		t.Fatalf("playlist requests = %d, want 2 (probe and initial HLS fetch)", got)
	}
	if got := segmentRequests.Load(); got != 1 {
		t.Fatalf("segment requests = %d, want 1", got)
	}
}
