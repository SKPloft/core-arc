// Package session is a HLS session middleware for echo.
//
// The middleware accounts the ingress and egress traffic of a HLS session. A
// session is identified by the session query parameter that is added to all
// URIs of a playlist. Both MPEG-TS (.ts) and fMP4 (.mp4, .m4s) segments are
// supported, including the initialization segment that is referenced by
// EXT-X-MAP.
package session

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"net/url"
	urlpath "path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/datarhei/core/v16/net"
	"github.com/datarhei/core/v16/session"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/lithammer/shortuuid/v4"
)

const hlsPlaylistSuffix = ".m3u8"

// hlsSegmentSuffixes are the suffixes of all media segments of a HLS session.
// .ts is the suffix of MPEG-TS segments, .mp4 and .m4s are the suffixes of
// fMP4 segments. The initialization segment of EXT-X-MAP can use any of them.
var hlsSegmentSuffixes = []string{".ts", ".mp4", ".m4s"}

// hlsTagURI matches the URI attribute of a HLS tag, i.e. #EXT-X-MAP:URI="init.mp4".
var hlsTagURI = regexp.MustCompile(`URI="([^"]*)"`)

type HLSConfig struct {
	// Skipper defines a function to skip middleware.
	Skipper          middleware.Skipper
	EgressCollector  session.Collector
	IngressCollector session.Collector
}

var DefaultHLSConfig = HLSConfig{
	Skipper:          middleware.DefaultSkipper,
	EgressCollector:  session.NewNullCollector(),
	IngressCollector: session.NewNullCollector(),
}

// NewHTTP returns a new HTTP session middleware with default config
func NewHLS() echo.MiddlewareFunc {
	return NewHLSWithConfig(DefaultHLSConfig)
}

type hls struct {
	egressCollector  session.Collector
	ingressCollector session.Collector
	reSessionID      *regexp.Regexp

	rxsegments map[string]int64
	lock       sync.Mutex
}

// NewHLS returns a new HLS session middleware
func NewHLSWithConfig(config HLSConfig) echo.MiddlewareFunc {
	if config.Skipper == nil {
		config.Skipper = DefaultHLSConfig.Skipper
	}

	if config.EgressCollector == nil && config.IngressCollector == nil {
		config.Skipper = func(c echo.Context) bool { return true }
	}

	if config.EgressCollector == nil {
		config.EgressCollector = DefaultHLSConfig.EgressCollector
	}

	if config.IngressCollector == nil {
		config.IngressCollector = DefaultHLSConfig.IngressCollector
	}

	hls := hls{
		egressCollector:  config.EgressCollector,
		ingressCollector: config.IngressCollector,
		reSessionID:      regexp.MustCompile(`^[` + regexp.QuoteMeta(shortuuid.DefaultAlphabet) + `]{22}$`),
		rxsegments:       make(map[string]int64),
	}

	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if config.Skipper(c) {
				return next(c)
			}

			req := c.Request()

			if req.Method == "PUT" || req.Method == "POST" {
				return hls.handleIngress(c, next)
			} else if req.Method == "GET" || req.Method == "HEAD" {
				return hls.handleEgress(c, next)
			}

			return next(c)
		}
	}
}

func (h *hls) handleIngress(c echo.Context, next echo.HandlerFunc) error {
	req := c.Request()

	path := req.URL.Path

	if isPlaylistPath(path) {
		// Read out the path of the media segments and look them up in the segment-map.
		// Add it as ingress for the respective "sessionId". The "sessionId" is the .m3u8 file name.
		reader := req.Body
		r := &bodyReader{
			reader: req.Body,
		}
		req.Body = r

		defer func() {
			req.Body = reader

			if r.size == 0 {
				return
			}

			if !h.ingressCollector.IsKnownSession(path) {
				// Register a new session
				reference := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
				h.ingressCollector.RegisterAndActivate(path, reference, path, "")
				h.ingressCollector.Extra(path, req.Header.Get("User-Agent"))
			}

			h.ingressCollector.Ingress(path, headerSize(req.Header))
			h.ingressCollector.Ingress(path, r.size)

			segments := r.getSegments(urlpath.Dir(path))

			if len(segments) != 0 {
				h.lock.Lock()
				for _, s := range segments {
					if size, ok := h.rxsegments[s]; ok {
						// Update ingress value
						h.ingressCollector.Ingress(path, size)
						delete(h.rxsegments, s)
					}
				}
				h.lock.Unlock()
			}
		}()
	} else if isSegmentPath(path) {
		// Get the size of the media segment and store it in the segment-map for later use.
		reader := req.Body
		r := &bodysizeReader{
			reader: req.Body,
		}
		req.Body = r

		defer func() {
			req.Body = reader

			if r.size != 0 {
				h.lock.Lock()
				h.rxsegments[path] = r.size + headerSize(req.Header)
				h.lock.Unlock()
			}
		}()
	}

	return next(c)
}

func (h *hls) handleEgress(c echo.Context, next echo.HandlerFunc) error {
	req := c.Request()
	res := c.Response()

	if !h.egressCollector.IsCollectableIP(c.RealIP()) {
		return next(c)
	}

	path := req.URL.Path
	sessionID := c.QueryParam("session")

	isM3U8 := isPlaylistPath(path)
	isSegment := isSegmentPath(path)

	rewrite := false

	if isM3U8 {
		if !h.egressCollector.IsKnownSession(sessionID) {
			if h.egressCollector.IsSessionsExceeded() {
				return echo.NewHTTPError(509, "Number of sessions exceeded")
			}

			streamBitrate := h.ingressCollector.SessionTopIngressBitrate(path) * 2.0 // Multiply by 2 to cover the initial peak
			maxBitrate := h.egressCollector.MaxEgressBitrate()

			if maxBitrate > 0.0 {
				currentBitrate := h.egressCollector.CompanionTopEgressBitrate() * 1.15

				// Add the new session's top bitrate to the ingress top bitrate
				resultingBitrate := currentBitrate + streamBitrate

				if resultingBitrate >= maxBitrate {
					return echo.NewHTTPError(509, "Bitrate limit exceeded")
				}
			}

			if len(sessionID) != 0 {
				if !h.reSessionID.MatchString(sessionID) {
					return echo.NewHTTPError(http.StatusForbidden)
				}

				referrer := req.Header.Get("Referer")
				if u, err := url.Parse(referrer); err == nil {
					referrer = u.Host
				}

				ip, _ := net.AnonymizeIPString(c.RealIP())
				extra := "[" + ip + "] " + req.Header.Get("User-Agent")

				reference := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))

				// Register a new session
				h.egressCollector.Register(sessionID, reference, path, referrer)
				h.egressCollector.Extra(sessionID, extra)

				// Give the new session an initial top bitrate
				h.egressCollector.SessionSetTopEgressBitrate(sessionID, streamBitrate)
			}
		}

		// Remove any Range request headers, because the rewrite will mess up any lengths.
		req.Header.Del("Range")
		req.Header.Del("If-Range")

		rewrite = true
	}

	var rewriter *sessionRewriter

	// Keep the current writer for later
	writer := res.Writer

	if rewrite {
		// Put the session rewriter in the middle. This will collect
		// the data that we need to rewrite.
		rewriter = &sessionRewriter{
			ResponseWriter: res.Writer,
		}

		res.Writer = rewriter
	}

	if err := next(c); err != nil {
		c.Error(err)
	}

	// Restore the original writer
	res.Writer = writer

	if rewrite {
		if res.Status != 200 {
			res.Write(rewriter.buffer.Bytes())
			return nil
		}

		// Rewrite the data befor sending it to the client
		rewriter.rewriteHLS(sessionID, c.Request().URL)

		res.Header().Set("Cache-Control", "private")
		res.Write(rewriter.buffer.Bytes())
	}

	if isM3U8 || isSegment {
		// Collect how many bytes we've written in this session
		h.egressCollector.Egress(sessionID, headerSize(res.Header()))
		h.egressCollector.Egress(sessionID, res.Size)

		if isSegment {
			// Activate the session. If the session is already active, this is a noop.
			// For unknown or empty session IDs, i.e. a download outside of a HLS
			// session, the collector is a noop as well.
			h.egressCollector.Activate(sessionID)
		}
	}

	return nil
}

func headerSize(header http.Header) int64 {
	var buffer bytes.Buffer

	header.Write(&buffer)

	return int64(buffer.Len())
}

type bodyReader struct {
	reader io.ReadCloser
	buffer bytes.Buffer
	size   int64
}

func (r *bodyReader) Read(b []byte) (int, error) {
	n, err := r.reader.Read(b)
	if n > 0 {
		r.buffer.Write(b[:n])
	}
	r.size += int64(n)

	return n, err
}

func (r *bodyReader) Close() error {
	return r.reader.Close()
}

func (r *bodyReader) getSegments(dir string) []string {
	segments := []string{}

	// Find all segment URLs in the .m3u8
	scanner := bufio.NewScanner(&r.buffer)
	for scanner.Scan() {
		line := scanner.Text()

		// Ignore empty lines
		if len(line) == 0 {
			continue
		}

		// Tags are ignored, except for the URI of the initialization segment.
		// Its bytes arrive outside of the playlist and have to be accounted for
		// together with the segments.
		if strings.HasPrefix(line, "#") {
			if !strings.HasPrefix(line, "#EXT-X-MAP:") {
				continue
			}

			for _, uri := range hlsTagURI.FindAllStringSubmatch(line, -1) {
				if path, ok := resolveHLSURI(dir, uri[1]); ok {
					segments = append(segments, path)
				}
			}

			continue
		}

		path, ok := resolveHLSURI(dir, line)
		if !ok {
			// Invalid or absolute URL
			continue
		}

		// Ignore anything that doesn't end in a media segment suffix
		if !isSegmentPath(path) {
			continue
		}

		segments = append(segments, path)
	}

	return segments
}

type bodysizeReader struct {
	reader io.ReadCloser
	size   int64
}

func (r *bodysizeReader) Read(b []byte) (int, error) {
	n, err := r.reader.Read(b)
	r.size += int64(n)

	return n, err
}

func (r *bodysizeReader) Close() error {
	return r.reader.Close()
}

type sessionRewriter struct {
	http.ResponseWriter
	buffer bytes.Buffer
}

func (g *sessionRewriter) Write(data []byte) (int, error) {
	// Write the data into internal buffer for later rewrite
	w, err := g.buffer.Write(data)

	return w, err
}

func (g *sessionRewriter) rewriteHLS(sessionID string, requestURL *url.URL) {
	var buffer bytes.Buffer

	isMaster := false

	dir := urlpath.Dir(requestURL.Path)

	// Find all URLS in the .m3u8 and add the session ID to the query string
	scanner := bufio.NewScanner(&g.buffer)
	for scanner.Scan() {
		line := scanner.Text()

		// Write empty lines unmodified
		if len(line) == 0 {
			buffer.WriteString(line + "\n")
			continue
		}

		// Tags are written unmodified except for the URI attribute they may carry
		if strings.HasPrefix(line, "#") {
			rewritten, subPlaylist := rewriteTagURI(line, sessionID)

			if subPlaylist {
				isMaster = true
			}

			buffer.WriteString(rewritten + "\n")
			continue
		}

		u, ok := parseHLSURI(line)
		if !ok {
			// Write invalid and absolute URLs unmodified
			buffer.WriteString(line + "\n")
			continue
		}

		// Write anything that doesn't end in .m3u8, .ts, .mp4, or .m4s unmodified
		if !isPlaylistPath(u.Path) && !isSegmentPath(u.Path) {
			buffer.WriteString(line + "\n")
			continue
		}

		// If this is a master manifest (i.e. an m3u8 which contains references to other m3u8), then
		// we give each substream an own session ID if they don't have already.
		if isPlaylistPath(u.Path) {
			// Check if we're referring to ourselves. This will cause an infinite loop
			// and has to be stopped.
			file := u.Path
			if !strings.HasPrefix(file, "/") {
				file = filepath.Join(dir, file)
			}

			loop := requestURL.Path == file

			u.RawQuery = addSessionQuery(u.RawQuery, shortuuid.New())

			isMaster = true

			if loop {
				buffer.WriteString("# m3u8 is referencing itself: " + u.String() + "\n")
			} else {
				buffer.WriteString(u.String() + "\n")
			}

			continue
		}

		u.RawQuery = addSessionQuery(u.RawQuery, sessionID)

		buffer.WriteString(u.String() + "\n")
	}

	if err := scanner.Err(); err != nil {
		return
	}

	// If this is not a master manifest and there isn't a session ID, we add a new session ID.
	if !isMaster && len(sessionID) == 0 {
		sessionID = shortuuid.New()

		buffer.Reset()

		buffer.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-STREAM-INF:BANDWIDTH=1024\n")

		// Add the session ID to the query string
		q := requestURL.Query()
		q.Set("session", sessionID)

		buffer.WriteString(urlpath.Base(requestURL.Path) + "?" + q.Encode())
	}

	g.buffer = buffer
}

// rewriteTagURI adds the session ID to the URI attribute of a HLS tag. The tag
// itself and all other attributes are kept byte by byte. A URI that refers to
// another playlist (i.e. #EXT-X-MEDIA:URI="audio.m3u8") is treated like a
// substream of a master playlist and gets an own session ID, all other URIs
// (i.e. the initialization segment of #EXT-X-MAP) stay within the current
// session. The second return value reports whether the tag refers to another
// playlist.
func rewriteTagURI(line, sessionID string) (string, bool) {
	matches := hlsTagURI.FindAllStringSubmatchIndex(line, -1)
	if len(matches) == 0 {
		return line, false
	}

	tag := line
	if i := strings.IndexByte(line, ':'); i >= 0 {
		tag = line[:i]
	}

	var buffer bytes.Buffer

	subPlaylist := false
	last := 0

	for _, match := range matches {
		uriStart, uriEnd := match[2], match[3]

		u, ok := parseHLSURI(line[uriStart:uriEnd])
		if !ok {
			continue
		}

		switch {
		case tag == "#EXT-X-MAP":
			u.RawQuery = addSessionQuery(u.RawQuery, sessionID)
		case isPlaylistPath(u.Path):
			u.RawQuery = addSessionQuery(u.RawQuery, shortuuid.New())
			subPlaylist = true
		case isSegmentPath(u.Path):
			u.RawQuery = addSessionQuery(u.RawQuery, sessionID)
		default:
			continue
		}

		buffer.WriteString(line[last:uriStart])
		buffer.WriteString(u.String())

		last = uriEnd
	}

	if last == 0 {
		return line, false
	}

	buffer.WriteString(line[last:])

	return buffer.String(), subPlaylist
}

func isPlaylistPath(path string) bool {
	return strings.HasSuffix(path, hlsPlaylistSuffix)
}

func isSegmentPath(path string) bool {
	for _, suffix := range hlsSegmentSuffixes {
		if strings.HasSuffix(path, suffix) {
			return true
		}
	}

	return false
}

// parseHLSURI parses a relative URI from a playlist. Absolute URIs and URIs
// without a path are not part of the session and are therefore rejected.
func parseHLSURI(uri string) (*url.URL, bool) {
	u, err := url.Parse(uri)
	if err != nil {
		return nil, false
	}

	if u.Scheme != "" || u.Host != "" || len(u.Path) == 0 {
		return nil, false
	}

	return u, true
}

// resolveHLSURI returns the absolute path of a relative URI from a playlist.
// The URI is resolved against the directory of the playlist.
func resolveHLSURI(dir, uri string) (string, bool) {
	u, ok := parseHLSURI(uri)
	if !ok {
		return "", false
	}

	if strings.HasPrefix(u.Path, "/") {
		return u.Path, true
	}

	return urlpath.Join(dir, u.Path), true
}

// addSessionQuery adds the session ID to a raw query string. Every existing
// session parameter is removed, including percent-encoded parameter names and
// parameters without a value, because a request reads the session with the
// decoded query semantics of the receiver. All other parameters are kept byte
// by byte, so that signed queries stay valid.
func addSessionQuery(rawQuery, sessionID string) string {
	if len(sessionID) == 0 {
		return rawQuery
	}

	session := "session=" + url.QueryEscape(sessionID)

	if len(rawQuery) == 0 {
		return session
	}

	components := strings.Split(rawQuery, "&")
	kept := make([]string, 0, len(components)+1)

	for _, component := range components {
		name := component
		if i := strings.IndexByte(component, '='); i >= 0 {
			name = component[:i]
		}

		// The receiver decodes the parameter names, hence a name that decodes
		// to session identifies a session parameter.
		if decoded, err := url.QueryUnescape(name); err == nil && decoded == "session" {
			continue
		}

		kept = append(kept, component)
	}

	if len(kept) == 0 {
		return session
	}

	return strings.Join(kept, "&") + "&" + session
}
