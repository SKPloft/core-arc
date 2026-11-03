package session

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/datarhei/core/v16/session"

	"github.com/labstack/echo/v4"
)

// testSessionID is a syntactically valid session ID, 22 characters from the
// shortuuid alphabet.
const testSessionID = "23456789ABCDEFGHJKLMNP"

func newHLSTestServer(t *testing.T) (*echo.Echo, session.Collector, session.Collector) {
	t.Helper()

	egress := session.NewCollector(session.CollectorConfig{
		InactiveTimeout: time.Minute,
		SessionTimeout:  time.Minute,
	})

	ingress := session.NewCollector(session.CollectorConfig{
		InactiveTimeout: time.Minute,
		SessionTimeout:  time.Minute,
	})

	t.Cleanup(func() {
		egress.Stop()
		ingress.Stop()
	})

	e := echo.New()
	e.Use(NewHLSWithConfig(HLSConfig{
		EgressCollector:  egress,
		IngressCollector: ingress,
	}))

	return e, egress, ingress
}

// serveFiles registers a GET/HEAD handler that returns the given body for the
// given path and a PUT handler that consumes the whole request body.
func serveFiles(e *echo.Echo, files map[string]string) {
	handler := func(c echo.Context) error {
		body, ok := files[c.Request().URL.Path]
		if !ok {
			return echo.NewHTTPError(http.StatusNotFound)
		}

		return c.String(http.StatusOK, body)
	}

	e.GET("/*", handler)
	e.HEAD("/*", handler)

	e.PUT("/*", func(c echo.Context) error {
		if _, err := io.Copy(io.Discard, c.Request().Body); err != nil {
			return err
		}

		return c.NoContent(http.StatusOK)
	})
}

func newRequest(method, target, body string) *http.Request {
	var reader io.Reader
	if len(body) != 0 {
		reader = strings.NewReader(body)
	}

	return httptest.NewRequest(method, target, reader)
}

// request returns the result of the request together with the size of the
// request header, which is what the middleware accounts for.
func request(e *echo.Echo, method, target, body string) (*httptest.ResponseRecorder, int64) {
	req := newRequest(method, target, body)
	hsize := headerSize(req.Header)

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	return rec, hsize
}

func get(e *echo.Echo, target string) *httptest.ResponseRecorder {
	rec, _ := request(e, http.MethodGet, target, "")
	return rec
}

func put(e *echo.Echo, target, body string) (*httptest.ResponseRecorder, int64) {
	rec, hsize := request(e, http.MethodPut, target, body)
	return rec, hsize
}

func requireContains(t *testing.T, haystack, needle string) {
	t.Helper()

	if !strings.Contains(haystack, needle) {
		t.Fatalf("expected the response to contain %q, got:\n%s", needle, haystack)
	}
}

func requireNotContains(t *testing.T, haystack, needle string) {
	t.Helper()

	if strings.Contains(haystack, needle) {
		t.Fatalf("expected the response to not contain %q, got:\n%s", needle, haystack)
	}
}

// uriAttribute returns the value of the URI attribute of a HLS tag.
func uriAttribute(t *testing.T, line string) string {
	t.Helper()

	start := strings.Index(line, `URI="`)
	if start < 0 {
		t.Fatalf("no URI attribute in %q", line)
	}

	start += len(`URI="`)

	end := strings.Index(line[start:], `"`)
	if end < 0 {
		t.Fatalf("unterminated URI attribute in %q", line)
	}

	return line[start : start+end]
}

// sessionParam returns the session query parameter of a playlist URI.
func sessionParam(t *testing.T, uri string) string {
	t.Helper()

	u, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("unable to parse %q: %v", uri, err)
	}

	sessionID := u.Query().Get("session")
	if len(sessionID) == 0 {
		t.Fatalf("no session parameter in %q", uri)
	}

	return sessionID
}

func requireValidSessionID(t *testing.T, sessionID string) {
	t.Helper()

	if len(sessionID) != 22 {
		t.Fatalf("expected a session ID with 22 characters, got %q", sessionID)
	}
}

// TestHLSRewriteFMP4MediaPlaylist checks that a fMP4 media playlist gets the
// session ID added to the initialization segment and to all media segments.
func TestHLSRewriteFMP4MediaPlaylist(t *testing.T) {
	e, egress, _ := newHLSTestServer(t)

	playlist := "#EXTM3U\n" +
		"#EXT-X-VERSION:7\n" +
		"#EXT-X-TARGETDURATION:4\n" +
		"#EXT-X-MAP:URI=\"init.mp4\",BYTERANGE=\"720@0\"\n" +
		"#EXTINF:4.000000,\n" +
		"segment1.m4s\n" +
		"#EXTINF:4.000000,\n" +
		"segment2.mp4\n"

	serveFiles(e, map[string]string{
		"/hls/live.m3u8": playlist,
	})

	rec := get(e, "/hls/live.m3u8?session="+testSessionID)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	body := rec.Body.String()

	requireContains(t, body, `#EXT-X-MAP:URI="init.mp4?session=`+testSessionID+`",BYTERANGE="720@0"`)
	requireContains(t, body, "segment1.m4s?session="+testSessionID)
	requireContains(t, body, "segment2.mp4?session="+testSessionID)

	if got := rec.Header().Get("Cache-Control"); got != "private" {
		t.Fatalf("expected Cache-Control private, got %q", got)
	}

	if !egress.IsKnownSession(testSessionID) {
		t.Fatalf("expected the session %q to be registered", testSessionID)
	}
}

// TestHLSRewriteSegmentKeepsExistingQuery checks that all other query
// parameters of a segment URI are kept byte by byte and that every existing
// session parameter is removed instead of being duplicated. The receiver reads
// the session with the decoded query semantics, hence percent-encoded names
// and parameters without a value have to be recognized as well.
func TestHLSRewriteSegmentKeepsExistingQuery(t *testing.T) {
	tests := []struct {
		name string
		uri  string
		want string
	}{
		{
			name: "plain parameters",
			uri:  "segment.m4s?token=a%2Fb&expires=99",
			want: "segment.m4s?token=a%2Fb&expires=99&session=" + testSessionID,
		},
		{
			name: "existing session parameter",
			uri:  "segment.m4s?session=AAAAAAAAAAAAAAAAAAAAAA&expires=99",
			want: "segment.m4s?expires=99&session=" + testSessionID,
		},
		{
			name: "percent encoded parameter name",
			uri:  "segment.m4s?%73ession=AAAAAAAAAAAAAAAAAAAAAA&expires=99",
			want: "segment.m4s?expires=99&session=" + testSessionID,
		},
		{
			name: "parameter without a value",
			uri:  "segment.m4s?session",
			want: "segment.m4s?session=" + testSessionID,
		},
		{
			name: "look-alike parameter names",
			uri:  "segment.m4s?session_id=7&xsession=1",
			want: "segment.m4s?session_id=7&xsession=1&session=" + testSessionID,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			e, _, _ := newHLSTestServer(t)

			playlist := "#EXTM3U\n" +
				"#EXT-X-VERSION:7\n" +
				"#EXT-X-MAP:URI=\"init.mp4?token=a%2Fb\"\n" +
				"#EXTINF:4.000000,\n" +
				test.uri + "\n"

			serveFiles(e, map[string]string{
				"/hls/live.m3u8": playlist,
			})

			rec := get(e, "/hls/live.m3u8?session="+testSessionID)

			body := rec.Body.String()

			requireContains(t, body, `#EXT-X-MAP:URI="init.mp4?token=a%2Fb&session=`+testSessionID+`"`)
			requireContains(t, body, test.want)
			requireNotContains(t, body, "session=AAAAAAAAAAAAAAAAAAAAAA")
		})
	}
}

// TestHLSRewriteSkipsExternalURIs checks that URIs pointing outside of the
// stream are not modified.
func TestHLSRewriteSkipsExternalURIs(t *testing.T) {
	e, _, _ := newHLSTestServer(t)

	externalSegment := "https://cdn.example.com/live/segment1.m4s"
	externalInit := `#EXT-X-MAP:URI="https://cdn.example.com/live/init.mp4"`

	playlist := "#EXTM3U\n" +
		"#EXT-X-VERSION:7\n" +
		externalInit + "\n" +
		"#EXTINF:4.000000,\n" +
		externalSegment + "\n" +
		"#EXTINF:4.000000,\n" +
		"segment2.m4s\n"

	serveFiles(e, map[string]string{
		"/hls/live.m3u8": playlist,
	})

	rec := get(e, "/hls/live.m3u8?session="+testSessionID)

	body := rec.Body.String()

	requireContains(t, body, externalInit+"\n")
	requireContains(t, body, externalSegment+"\n")
	requireContains(t, body, "segment2.m4s?session="+testSessionID)
}

// TestHLSRewriteMasterPlaylistSubSessions checks that all references to other
// playlists get an own session ID, both as plain URI and as tag attribute.
func TestHLSRewriteMasterPlaylistSubSessions(t *testing.T) {
	e, egress, _ := newHLSTestServer(t)

	master := "#EXTM3U\n" +
		"#EXT-X-VERSION:7\n" +
		`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="en",DEFAULT=YES,URI="audio.m3u8"` + "\n" +
		`#EXT-X-STREAM-INF:BANDWIDTH=1024,CODECS="av01.0.05M.08"` + "\n" +
		"video.m3u8\n"

	serveFiles(e, map[string]string{
		"/hls/master.m3u8": master,
		"/hls/video.m3u8":  "#EXTM3U\n#EXTINF:4.000000,\nsegment1.m4s\n",
	})

	rec := get(e, "/hls/master.m3u8?session="+testSessionID)

	body := rec.Body.String()

	var audioURI, videoURI string

	for _, line := range strings.Split(body, "\n") {
		switch {
		case strings.HasPrefix(line, "#EXT-X-MEDIA:"):
			audioURI = uriAttribute(t, line)
		case strings.HasPrefix(line, "video.m3u8"):
			videoURI = line
		}
	}

	if len(audioURI) == 0 {
		t.Fatalf("no rewritten EXT-X-MEDIA URI in:\n%s", body)
	}

	if len(videoURI) == 0 {
		t.Fatalf("no rewritten video playlist URI in:\n%s", body)
	}

	audioSession := sessionParam(t, audioURI)
	videoSession := sessionParam(t, videoURI)

	requireValidSessionID(t, audioSession)
	requireValidSessionID(t, videoSession)

	if audioSession == videoSession {
		t.Fatalf("expected different sessions for audio and video, got %q twice", audioSession)
	}

	if audioSession == testSessionID || videoSession == testSessionID {
		t.Fatalf("expected a new session for every substream")
	}

	// Requesting the substream registers its session.
	get(e, "/hls/video.m3u8?session="+videoSession)

	if !egress.IsKnownSession(videoSession) {
		t.Fatalf("expected the substream session %q to be registered", videoSession)
	}
}

// TestHLSEgressSegmentActivatesSession checks that a media segment request
// activates the pending session and that its bytes are accounted for. It
// covers MPEG-TS as well as fMP4 segments.
func TestHLSEgressSegmentActivatesSession(t *testing.T) {
	tests := []struct {
		name     string
		segment  string
		playlist string
	}{
		{
			name:     "mpegts",
			segment:  "/hls/segment1.ts",
			playlist: "#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:4.000000,\nsegment1.ts\n",
		},
		{
			name:     "fmp4 m4s",
			segment:  "/hls/segment1.m4s",
			playlist: "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:4.000000,\nsegment1.m4s\n",
		},
		{
			name:     "fmp4 mp4",
			segment:  "/hls/segment1.mp4",
			playlist: "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:4.000000,\nsegment1.mp4\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			e, egress, _ := newHLSTestServer(t)

			segmentBody := strings.Repeat("s", 1024)

			serveFiles(e, map[string]string{
				"/hls/live.m3u8": test.playlist,
				test.segment:     segmentBody,
			})

			// The playlist registers the session, but does not activate it.
			get(e, "/hls/live.m3u8?session="+testSessionID)

			if active := egress.Active(); len(active) != 0 {
				t.Fatalf("expected no active session after the playlist request, got %d", len(active))
			}

			rec := get(e, test.segment+"?session="+testSessionID)

			if rec.Code != http.StatusOK {
				t.Fatalf("expected status 200, got %d", rec.Code)
			}

			active := egress.Active()
			if len(active) != 1 {
				t.Fatalf("expected one active session after the segment request, got %d", len(active))
			}

			if active[0].ID != testSessionID {
				t.Fatalf("expected the session %q to be active, got %q", testSessionID, active[0].ID)
			}

			if active[0].TxBytes < uint64(len(segmentBody)) {
				t.Fatalf("expected at least %d transmitted bytes, got %d", len(segmentBody), active[0].TxBytes)
			}
		})
	}
}

// TestHLSEgressIgnoresPlainMP4Download checks that a download outside of a HLS
// session neither creates nor activates a session.
func TestHLSEgressIgnoresPlainMP4Download(t *testing.T) {
	e, egress, _ := newHLSTestServer(t)

	serveFiles(e, map[string]string{
		"/files/movie.mp4": strings.Repeat("m", 2048),
	})

	rec := get(e, "/files/movie.mp4")

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	if sessions := egress.Summary().CurrentSessions; sessions != 0 {
		t.Fatalf("expected no active sessions, got %d", sessions)
	}

	// Even a syntactically valid, but unknown session ID doesn't create a session.
	get(e, "/files/movie.mp4?session="+testSessionID)

	if egress.IsKnownSession(testSessionID) {
		t.Fatalf("expected no session %q to be created by a mp4 download", testSessionID)
	}

	if active := egress.Active(); len(active) != 0 {
		t.Fatalf("expected no active sessions, got %d", len(active))
	}
}

// TestHLSIngressFMP4SegmentsAccountedToPlaylist checks that the bytes of the
// initialization segment and of the fMP4 segments are accounted to the
// playlist session once the playlist arrives.
func TestHLSIngressFMP4SegmentsAccountedToPlaylist(t *testing.T) {
	e, _, ingress := newHLSTestServer(t)

	serveFiles(e, map[string]string{})

	initBody := strings.Repeat("i", 300)
	segmentBody := strings.Repeat("s", 700)

	playlist := "#EXTM3U\n" +
		"#EXT-X-VERSION:7\n" +
		"#EXT-X-MAP:URI=\"init.mp4\"\n" +
		"#EXTINF:4.000000,\n" +
		"segment1.m4s\n"

	// The segments arrive before the playlist that references them.
	rec, initHeader := put(e, "/hls/init.mp4", initBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	rec, segmentHeader := put(e, "/hls/segment1.m4s", segmentBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	rec, playlistHeader := put(e, "/hls/stream.m3u8", playlist)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	expected := int64(len(playlist)) + playlistHeader +
		int64(len(initBody)) + initHeader +
		int64(len(segmentBody)) + segmentHeader

	rxBytes := ingressRxBytes(t, ingress, "/hls/stream.m3u8")

	if rxBytes != expected {
		t.Fatalf("expected %d ingress bytes, got %d", expected, rxBytes)
	}

	// A repeated playlist doesn't account for the segments again.
	_, playlistHeader = put(e, "/hls/stream.m3u8", playlist)

	repeated := int64(len(playlist)) + playlistHeader
	rxBytes = ingressRxBytes(t, ingress, "/hls/stream.m3u8")

	if rxBytes != expected+repeated {
		t.Fatalf("expected %d ingress bytes after the repeated playlist, got %d", expected+repeated, rxBytes)
	}
}

// TestHLSIngressMPEGTSSegmentAccountedToPlaylist is the regression test for the
// MPEG-TS ingress accounting.
func TestHLSIngressMPEGTSSegmentAccountedToPlaylist(t *testing.T) {
	e, _, ingress := newHLSTestServer(t)

	serveFiles(e, map[string]string{})

	segmentBody := strings.Repeat("s", 500)

	playlist := "#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:4.000000,\nsegment1.ts\n"

	rec, segmentHeader := put(e, "/hls/segment1.ts", segmentBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	rec, playlistHeader := put(e, "/hls/ts.m3u8", playlist)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	expected := int64(len(playlist)) + playlistHeader + int64(len(segmentBody)) + segmentHeader

	rxBytes := ingressRxBytes(t, ingress, "/hls/ts.m3u8")

	if rxBytes != expected {
		t.Fatalf("expected %d ingress bytes, got %d", expected, rxBytes)
	}
}

func ingressRxBytes(t *testing.T, ingress session.Collector, id string) int64 {
	t.Helper()

	if !ingress.IsKnownSession(id) {
		t.Fatalf("expected the ingress session %q to exist", id)
	}

	for _, active := range ingress.Active() {
		if active.ID == id {
			return int64(active.RxBytes)
		}
	}

	t.Fatalf("expected the ingress session %q to be active", id)

	return 0
}
