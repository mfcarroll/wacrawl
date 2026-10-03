package webui

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/openclaw/wacrawl/internal/store"
)

//go:embed static/*
var assets embed.FS

const (
	snippetStartMarker = "\ue000"
	snippetEndMarker   = "\ue001"

	// maxInlineMediaBytes caps how large an archived image may be before the
	// viewer falls back to the metadata card instead of inlining bytes. Images
	// only: video and audio stream in ranges.
	maxInlineMediaBytes = 25 << 20

	// mediaURLTTL limits a signed link leaked from a long-open page. Links also
	// die with the server, whose per-run token is the signing key.
	mediaURLTTL = 12 * time.Hour
)

type Config struct {
	Port   int
	Output io.Writer
	// AllowCloudMedia materializes dataless (cloud-synced) media on demand. Off
	// by default: Desktop leaves stubs for media it never downloaded, and
	// blocking on those would hang the request.
	AllowCloudMedia bool
}

type handler struct {
	store             *store.Store
	token             string
	allowedHost       string
	allowedMediaRoots []allowedMediaRoot
	skippedMediaRoots []skippedMediaRoot
	allowCloudMedia   bool
}

type allowedMediaRoot struct {
	path        string
	lexicalPath string
	root        *os.Root
}

// skippedMediaRoot is a media root the viewer couldn't use, reported at startup.
type skippedMediaRoot struct {
	path   string
	reason string
	// optional roots are reported only when nothing is readable.
	optional bool
}

type statusResponse struct {
	Chats          int       `json:"chats"`
	UnreadChats    int       `json:"unread_chats"`
	UnreadMessages int       `json:"unread_messages"`
	Contacts       int       `json:"contacts"`
	Groups         int       `json:"groups"`
	Messages       int       `json:"messages"`
	MediaMessages  int       `json:"media_messages"`
	OldestMessage  time.Time `json:"oldest_message,omitzero"`
	NewestMessage  time.Time `json:"newest_message,omitzero"`
	LastImportAt   time.Time `json:"last_import_at,omitzero"`
}

type chatResponse struct {
	JID           string    `json:"jid"`
	Kind          string    `json:"kind"`
	Name          string    `json:"name,omitempty"`
	LastMessageAt time.Time `json:"last_message_at,omitzero"`
	UnreadCount   int       `json:"unread_count"`
	Archived      bool      `json:"archived"`
	MessageCount  int       `json:"message_count"`
}

type messageResponse struct {
	SourcePK    int64     `json:"source_pk"`
	ChatJID     string    `json:"chat_jid"`
	ChatName    string    `json:"chat_name,omitempty"`
	MessageID   string    `json:"message_id"`
	SenderJID   string    `json:"sender_jid,omitempty"`
	SenderName  string    `json:"sender_name,omitempty"`
	Timestamp   time.Time `json:"timestamp"`
	FromMe      bool      `json:"from_me"`
	Text        string    `json:"text,omitempty"`
	MessageType string    `json:"message_type,omitempty"`
	MediaType   string    `json:"media_type,omitempty"`
	MediaTitle  string    `json:"media_title,omitempty"`
	MediaSize   int64     `json:"media_size,omitempty"`
	Starred     bool      `json:"starred,omitempty"`
	Snippet     string    `json:"snippet,omitempty"`
	// MediaKind is image, video, or audio, or empty for a card.
	MediaKind string `json:"media_kind,omitempty"`
	// MediaSrc is a signed link for kinds the browser loads by URL. Not called
	// media_url: store.Message.MediaURL is WhatsApp's CDN address, which must
	// never leave this process.
	MediaSrc string `json:"media_src,omitempty"`
	// MediaDownload is a signed link that saves the file (serveDownload).
	MediaDownload string `json:"media_download,omitempty"`
	// MediaFormat is a document's type from its extension ("PDF"), or empty.
	MediaFormat string `json:"media_format,omitempty"`
}

func Serve(ctx context.Context, archive *store.Store, cfg Config) error {
	if cfg.Port < 0 || cfg.Port > 65535 {
		return errors.New("web port must be between 0 and 65535")
	}
	listener, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", cfg.Port))
	if err != nil {
		return fmt.Errorf("listen for web viewer: %w", err)
	}
	defer func() { _ = listener.Close() }()

	token, err := randomToken()
	if err != nil {
		return err
	}
	status, err := archive.Status(ctx)
	if err != nil {
		_ = listener.Close()
		return fmt.Errorf("read archive media roots: %w", err)
	}
	host := listener.Addr().String()
	url := "http://" + host + "/#" + token
	output := cfg.Output
	if output == nil {
		output = io.Discard
	}
	// Print the URL first: resolving a media root can block (an unmounted
	// volume, a privacy prompt).
	_, _ = fmt.Fprintf(output, "wacrawl web viewer\n%s\n\nLocal and read-only. Keep this URL private; Ctrl-C stops the server.\n", url)

	handler := NewHandler(archive, token, host, status.SourceRoot)
	handler.allowCloudMedia = cfg.AllowCloudMedia
	defer handler.close()
	if warning := handler.mediaRootWarning(); warning != "" {
		_, _ = fmt.Fprintf(output, "\n%s", warning)
	}

	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       time.Minute,
	}
	stopWatcher := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_ = server.Shutdown(shutdownCtx)
		case <-stopWatcher:
		}
	}()

	err = server.Serve(listener)
	close(stopWatcher)
	<-watcherDone
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve web viewer: %w", err)
	}
	return nil
}

func NewHandler(archive *store.Store, token, allowedHost string, sourceRoots ...string) *handler {
	h := &handler{store: archive, token: token, allowedHost: allowedHost}
	// Exists only after an import with --copy-media.
	candidates := []mediaRootCandidate{{filepath.Join(filepath.Dir(archive.Path()), "media"), true}}
	for _, root := range sourceRoots {
		trimmed := strings.TrimSpace(root)
		if trimmed == "" {
			continue
		}
		// Relative means not a path: older archives hold a "wa-store:" identity.
		if !filepath.IsAbs(trimmed) {
			h.skippedMediaRoots = append(h.skippedMediaRoots, skippedMediaRoot{trimmed, "not an absolute path", false})
			continue
		}
		candidates = append(candidates, mediaRootCandidate{trimmed, false})
	}
	for _, candidate := range candidates {
		absolute, err := filepath.Abs(candidate.path)
		if err != nil {
			h.skippedMediaRoots = append(h.skippedMediaRoots, skippedMediaRoot{candidate.path, "cannot be resolved", candidate.optional})
			continue
		}
		resolved, err := filepath.EvalSymlinks(absolute)
		if err != nil {
			h.skippedMediaRoots = append(h.skippedMediaRoots, skippedMediaRoot{absolute, "missing or unreadable", candidate.optional})
			continue
		}
		opened, err := os.OpenRoot(resolved)
		if err != nil {
			h.skippedMediaRoots = append(h.skippedMediaRoots, skippedMediaRoot{absolute, "cannot be opened", candidate.optional})
			continue
		}
		h.allowedMediaRoots = append(h.allowedMediaRoots, allowedMediaRoot{path: resolved, lexicalPath: filepath.Clean(absolute), root: opened})
	}
	return h
}

type mediaRootCandidate struct {
	path     string
	optional bool
}

// mediaRootWarning explains unusable media roots, which otherwise make every
// attachment 404 silently.
func (h *handler) mediaRootWarning() string {
	nothingReadable := len(h.allowedMediaRoots) == 0
	report := make([]skippedMediaRoot, 0, len(h.skippedMediaRoots))
	for _, skipped := range h.skippedMediaRoots {
		if skipped.optional && !nothingReadable {
			continue
		}
		report = append(report, skipped)
	}
	if len(report) == 0 {
		return ""
	}
	var b strings.Builder
	if nothingReadable {
		b.WriteString("Warning: no media directory is readable, so attachments will not load.\n")
	} else {
		b.WriteString("Warning: some media directories are unreadable; attachments stored there will not load.\n")
	}
	for _, skipped := range report {
		_, _ = fmt.Fprintf(&b, "  %s (%s)\n", skipped.path, skipped.reason)
	}
	return b.String()
}

func (h *handler) close() {
	for _, root := range h.allowedMediaRoots {
		_ = root.root.Close()
	}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	if r.Host != h.allowedHost {
		http.Error(w, "unexpected host", http.StatusMisdirectedRequest)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	switch r.URL.Path {
	case "/":
		h.serveAsset(w, "static/index.html", "text/html; charset=utf-8")
		return
	case "/app.css":
		h.serveAsset(w, "static/app.css", "text/css; charset=utf-8")
		return
	case "/app.js":
		h.serveAsset(w, "static/app.js", "text/javascript; charset=utf-8")
		return
	case "/favicon.svg", "/favicon.ico":
		h.serveAsset(w, "static/favicon.svg", "image/svg+xml")
		return
	}

	if !strings.HasPrefix(r.URL.Path, "/api/") {
		http.NotFound(w, r)
		return
	}
	// <video>, <audio>, and download links can't send the header.
	authorized := h.authorized(r) || h.signedMediaRequest(r, time.Now())
	if !authorized {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "authorization required", http.StatusUnauthorized)
		return
	}

	switch r.URL.Path {
	case "/api/status":
		h.serveStatus(w, r)
	case "/api/chats":
		h.serveChats(w, r)
	case "/api/messages":
		h.serveMessages(w, r)
	case "/api/search":
		h.serveSearch(w, r)
	case "/api/media":
		h.serveMedia(w, r)
	case "/api/download":
		h.serveDownload(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (h *handler) serveAsset(w http.ResponseWriter, name, contentType string) {
	body, err := assets.ReadFile(name)
	if err != nil {
		http.Error(w, "asset unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (h *handler) authorized(r *http.Request) bool {
	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	candidate := strings.TrimSpace(strings.TrimPrefix(header, prefix))
	if len(candidate) != len(h.token) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(h.token)) == 1
}

// mediaSignature signs one attachment's URL with the per-run token, for
// elements that can't send the Authorization header. The purpose (endpoint) is
// signed too, so a link for one endpoint can't be replayed against another.
func (h *handler) mediaSignature(purpose string, sourcePK, expiry int64) string {
	mac := hmac.New(sha256.New, []byte(h.token))
	// Delimited so no other purpose, pk, and expiry can produce the same input.
	_, _ = fmt.Fprintf(mac, "%s\x00%d\x00%d", purpose, sourcePK, expiry)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (h *handler) signedURL(purpose string, sourcePK int64, now time.Time) string {
	expiry := now.Add(mediaURLTTL).Unix()
	return fmt.Sprintf("/api/%s?pk=%d&exp=%d&sig=%s", purpose, sourcePK, expiry, h.mediaSignature(purpose, sourcePK, expiry))
}

func (h *handler) signedMediaRequest(r *http.Request, now time.Time) bool {
	purpose := strings.TrimPrefix(r.URL.Path, "/api/")
	if purpose != "media" && purpose != "download" {
		return false
	}
	query := r.URL.Query()
	sourcePK, err := strconv.ParseInt(strings.TrimSpace(query.Get("pk")), 10, 64)
	if err != nil {
		return false
	}
	expiry, err := strconv.ParseInt(strings.TrimSpace(query.Get("exp")), 10, 64)
	if err != nil || now.Unix() > expiry {
		return false
	}
	want := h.mediaSignature(purpose, sourcePK, expiry)
	got := strings.TrimSpace(query.Get("sig"))
	if len(got) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func (h *handler) serveStatus(w http.ResponseWriter, r *http.Request) {
	status, err := h.store.Status(r.Context())
	if err != nil {
		writeArchiveError(w)
		return
	}
	writeJSON(w, statusResponse{
		Chats:          status.Chats,
		UnreadChats:    status.UnreadChats,
		UnreadMessages: status.UnreadMessages,
		Contacts:       status.Contacts,
		Groups:         status.Groups,
		Messages:       status.Messages,
		MediaMessages:  status.MediaMessages,
		OldestMessage:  status.OldestMessage,
		NewestMessage:  status.NewestMessage,
		LastImportAt:   status.LastImportAt,
	})
}

func (h *handler) serveChats(w http.ResponseWriter, r *http.Request) {
	limit, ok := parseLimit(w, r, 100)
	if !ok {
		return
	}
	chats, err := h.store.ListChats(r.Context(), limit)
	if err != nil {
		writeArchiveError(w)
		return
	}
	out := make([]chatResponse, 0, len(chats))
	for _, chat := range chats {
		out = append(out, chatResponse{
			JID:           chat.JID,
			Kind:          chat.Kind,
			Name:          chat.Name,
			LastMessageAt: chat.LastMessageAt,
			UnreadCount:   chat.UnreadCount,
			Archived:      chat.Archived,
			MessageCount:  chat.MessageCount,
		})
	}
	writeJSON(w, out)
}

func (h *handler) serveMessages(w http.ResponseWriter, r *http.Request) {
	limit, ok := parseLimit(w, r, 100)
	if !ok {
		return
	}
	before, beforePK, ok := parseBefore(w, r)
	if !ok {
		return
	}
	messages, err := h.store.Messages(r.Context(), store.MessageFilter{
		ChatJID:  strings.TrimSpace(r.URL.Query().Get("chat")),
		Limit:    limit,
		Before:   before,
		BeforePK: beforePK,
	})
	if err != nil {
		writeArchiveError(w)
		return
	}
	for left, right := 0, len(messages)-1; left < right; left, right = left+1, right-1 {
		messages[left], messages[right] = messages[right], messages[left]
	}
	writeJSON(w, h.messagesForWeb(messages))
}

func (h *handler) serveSearch(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		http.Error(w, "search query required", http.StatusBadRequest)
		return
	}
	limit, ok := parseLimit(w, r, 100)
	if !ok {
		return
	}
	messages, err := h.store.Search(r.Context(), store.MessageFilter{
		Query: query,
		Limit: limit,
		// Private-use markers cannot collide with real message text, so the
		// client can turn them into highlight nodes without guessing.
		SnippetStart: snippetStartMarker,
		SnippetEnd:   snippetEndMarker,
	})
	if err != nil {
		http.Error(w, "invalid search query", http.StatusBadRequest)
		return
	}
	writeJSON(w, h.messagesForWeb(messages))
}

func (h *handler) messagesForWeb(messages []store.Message) []messageResponse {
	now := time.Now()
	out := make([]messageResponse, 0, len(messages))
	for _, message := range messages {
		kind := inlineMediaKind(message)
		// Images are fetched with the header; only streamed kinds need a URL.
		mediaSrc := ""
		if message.SourcePK > 0 && (kind == "video" || kind == "audio") {
			mediaSrc = h.signedURL("media", message.SourcePK, now)
		}
		mediaDownload := ""
		if message.SourcePK > 0 && strings.TrimSpace(message.MediaPath) != "" {
			mediaDownload = h.signedURL("download", message.SourcePK, now)
		}
		mediaFormat := ""
		if message.MediaType == "document" {
			mediaFormat = strings.ToUpper(strings.TrimPrefix(fileExtension(strings.TrimSpace(message.MediaPath)), "."))
		}
		out = append(out, messageResponse{
			SourcePK:      message.SourcePK,
			ChatJID:       message.ChatJID,
			ChatName:      message.ChatName,
			MessageID:     message.MessageID,
			SenderJID:     message.SenderJID,
			SenderName:    message.SenderName,
			Timestamp:     message.Timestamp,
			FromMe:        message.FromMe,
			Text:          message.Text,
			MessageType:   message.MessageType,
			MediaType:     message.MediaType,
			MediaTitle:    message.MediaTitle,
			MediaSize:     message.MediaSize,
			Starred:       message.Starred,
			Snippet:       message.Snippet,
			MediaKind:     kind,
			MediaSrc:      mediaSrc,
			MediaDownload: mediaDownload,
			MediaFormat:   mediaFormat,
		})
	}
	return out
}

// serveMedia serves image, video, and audio bytes from the allowed roots,
// never revealing the path.
func (h *handler) serveMedia(w http.ResponseWriter, r *http.Request) {
	message, ok := h.requestedMessage(w, r)
	if !ok {
		return
	}
	if strings.TrimSpace(message.MediaPath) == "" || inlineMediaKind(message) == "" {
		http.Error(w, "no inline preview", http.StatusNotFound)
		return
	}
	file, info, path, ok := h.openArchivedMedia(w, message)
	if !ok {
		return
	}
	defer func() { _ = file.Close() }()
	sniff := make([]byte, 512)
	n, err := io.ReadFull(file, sniff)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		http.Error(w, "media unavailable", http.StatusNotFound)
		return
	}
	contentType, ok := inlineContentType(path, sniff[:n], inlineMediaKind(message))
	if !ok {
		http.Error(w, "unsupported media type", http.StatusUnsupportedMediaType)
		return
	}
	if strings.HasPrefix(contentType, "image/") && info.Size() > maxInlineMediaBytes {
		http.Error(w, "media too large for inline preview", http.StatusRequestEntityTooLarge)
		return
	}
	w.Header().Set("Content-Type", contentType)
	// ServeContent for range requests: seeking needs them, and Safari won't play
	// without. It rewinds past the sniffed bytes.
	http.ServeContent(w, r, "", info.ModTime(), file)
}

// requestedMessage writes the error response itself when it returns false.
func (h *handler) requestedMessage(w http.ResponseWriter, r *http.Request) (store.Message, bool) {
	sourcePK, err := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("pk")), 10, 64)
	if err != nil || sourcePK <= 0 {
		http.Error(w, "pk must be a positive integer", http.StatusBadRequest)
		return store.Message{}, false
	}
	message, err := h.store.MessageBySourcePK(r.Context(), sourcePK)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return store.Message{}, false
	}
	if err != nil {
		writeArchiveError(w)
		return store.Message{}, false
	}
	return message, true
}

// openArchivedMedia writes the error response itself when it returns false.
// The caller closes the file.
func (h *handler) openArchivedMedia(w http.ResponseWriter, message store.Message) (*os.File, os.FileInfo, string, bool) {
	root, path, ok := containedMediaPath(strings.TrimSpace(message.MediaPath), h.allowedMediaRoots)
	if !ok {
		http.Error(w, "media unavailable", http.StatusNotFound)
		return nil, nil, "", false
	}
	open := openMediaFile
	if h.allowCloudMedia {
		open = openMediaFileBlocking
	}
	file, err := open(root, path)
	if err != nil {
		http.Error(w, "media unavailable", http.StatusNotFound)
		return nil, nil, "", false
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 {
		_ = file.Close()
		http.Error(w, "media unavailable", http.StatusNotFound)
		return nil, nil, "", false
	}
	// WhatsApp keeps stubs for media it has not downloaded; reading one blocks
	// until macOS materializes it, which can wedge the request indefinitely.
	// Cloud-synced archives want exactly that (AllowCloudMedia).
	if !h.allowCloudMedia && !fileMaterialized(info) {
		_ = file.Close()
		http.Error(w, "media not downloaded locally", http.StatusNotFound)
		return nil, nil, "", false
	}
	return file, info, path, true
}

// serveDownload sends files as opaque downloads (generic type, nosniff,
// attachment disposition, sandbox CSP): rendered on this origin, an HTML or
// SVG file could script the page and read its token.
func (h *handler) serveDownload(w http.ResponseWriter, r *http.Request) {
	message, ok := h.requestedMessage(w, r)
	if !ok {
		return
	}
	if strings.TrimSpace(message.MediaPath) == "" {
		http.Error(w, "no attachment", http.StatusNotFound)
		return
	}
	file, info, path, ok := h.openArchivedMedia(w, message)
	if !ok {
		return
	}
	defer func() { _ = file.Close() }()
	filename := downloadFilename(message.MediaTitle, path)
	if fileExtension(filename) == "" {
		// Unnamed and no extension: sniff the head (ServeContent rewinds).
		head := make([]byte, 512)
		n, _ := io.ReadFull(file, head)
		filename += sniffedExtension(head[:n], inlineMediaKind(message))
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	http.ServeContent(w, r, "", info.ModTime(), file)
}

// fileExtension is filepath.Ext limited to 1-8 letters or digits, so
// "Report v1.2 final" has none.
func fileExtension(name string) string {
	if ext := filepath.Ext(name); plausibleExtension.MatchString(ext) {
		return ext
	}
	return ""
}

var plausibleExtension = regexp.MustCompile(`^\.[A-Za-z0-9]{1,8}$`)

// sniffedExtension names only unambiguous signatures. Not ZIP: .docx, .xlsx,
// and .epub are ZIPs inside, and .zip would mislabel them.
func sniffedExtension(head []byte, kind string) string {
	// ISO base media says nothing about its stream; the message's kind does.
	if isoBaseMedia(head) {
		switch kind {
		case "audio":
			return ".m4a"
		case "video":
			return ".mp4"
		}
		return ""
	}
	switch http.DetectContentType(head) {
	case "application/ogg":
		return ".ogg"
	case "application/pdf":
		return ".pdf"
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "audio/mpeg":
		return ".mp3"
	case "audio/wave":
		return ".wav"
	}
	return ""
}

// whatsAppContentHash matches the base64 SHA-256 digest WhatsApp stores as the
// title of attachments that have no name of their own.
var whatsAppContentHash = regexp.MustCompile(`^[A-Za-z0-9+/]{43}=$`)

// downloadFilename uses the title, else the stored name, without separators
// or control characters, keeping the stored extension if the title has none.
func downloadFilename(title, path string) string {
	name := strings.TrimSpace(title)
	if name == "" || whatsAppContentHash.MatchString(name) {
		name = filepath.Base(path)
	}
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '/' || r == '\\' {
			return '_'
		}
		return r
	}, name)
	if strings.Trim(name, ". _") == "" {
		name = "attachment"
	}
	if fileExtension(name) == "" {
		name += strings.ToLower(fileExtension(path))
	}
	if runes := []rune(name); len(runes) > 180 {
		ext := fileExtension(name)
		name = string(runes[:180-len([]rune(ext))]) + ext
	}
	return name
}

// Go's sniffer misses QuickTime, 3GP, CAF, and AMR, and calls an .m4a
// voice note video/mp4.
var mediaContentTypes = map[string]string{
	".bmp":  "image/bmp",
	".gif":  "image/gif",
	".heic": "image/heic",
	".jpeg": "image/jpeg",
	".jpg":  "image/jpeg",
	".png":  "image/png",
	".webp": "image/webp",

	".3gp":  "video/3gpp",
	".avi":  "video/x-msvideo",
	".m4v":  "video/mp4",
	".mkv":  "video/x-matroska",
	".mov":  "video/quicktime",
	".mp4":  "video/mp4",
	".webm": "video/webm",

	".aac":  "audio/aac",
	".amr":  "audio/amr",
	".caf":  "audio/x-caf",
	".m4a":  "audio/mp4",
	".mp3":  "audio/mpeg",
	".oga":  "audio/ogg",
	".ogg":  "audio/ogg",
	".opus": "audio/ogg",
	".wav":  "audio/wav",
}

// inlineContentType picks the type to serve, or refuses. The extension wins
// for media, but the sniffer can veto (a PDF named .mp4). kind is the last
// resort for a file without an extension.
func inlineContentType(name string, sniff []byte, kind string) (string, bool) {
	sniffed := http.DetectContentType(sniff)
	if !playableContentType(sniffed) && !inconclusiveSniff(sniffed) {
		return "", false
	}
	if byExtension := mediaContentTypes[strings.ToLower(filepath.Ext(name))]; byExtension != "" {
		return byExtension, true
	}
	// Voice notes are Ogg or MP4, which sniff as generic or video; only the
	// message says audio.
	if kind == "audio" {
		switch {
		case sniffed == "application/ogg":
			return "audio/ogg", true
		case isoBaseMedia(sniff):
			return "audio/mp4", true
		}
	}
	if playableContentType(sniffed) {
		return sniffed, true
	}
	return "", false
}

// isoBaseMedia reports whether the bytes open an ISO base media file (MP4,
// M4A, QuickTime), whose first box is "ftyp" at offset 4.
func isoBaseMedia(head []byte) bool {
	return len(head) >= 12 && string(head[4:8]) == "ftyp"
}

func playableContentType(contentType string) bool {
	return mediaKindForContentType(contentType) != ""
}

// inconclusiveSniff reports bytes the sniffer couldn't place. Ogg counts: it
// can't tell an Opus voice note from other Ogg.
func inconclusiveSniff(contentType string) bool {
	return contentType == "application/octet-stream" || contentType == "application/ogg"
}

func containedMediaPath(path string, roots []allowedMediaRoot) (*os.Root, string, bool) {
	if !filepath.IsAbs(path) {
		return nil, "", false
	}
	candidate, err := filepath.Abs(path)
	if err != nil {
		return nil, "", false
	}
	for _, root := range roots {
		if !pathWithinRoot(root.lexicalPath, candidate) && !pathWithinRoot(root.path, candidate) {
			continue
		}
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			return nil, "", false
		}
		if relative, ok := relativeToRoot(root.path, resolved); ok {
			return root.root, relative, true
		}
	}
	return nil, "", false
}

func pathWithinRoot(root, candidate string) bool {
	_, ok := relativeToRoot(root, candidate)
	return ok
}

func relativeToRoot(root, candidate string) (string, bool) {
	relative, err := filepath.Rel(root, candidate)
	return relative, err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator)) && !filepath.IsAbs(relative)
}

// inlineMediaKind returns image, video, or audio, or empty for a card. The
// extension wins: WhatsApp's type strings describe the message, not the
// file (a GIF is stored as .mp4; some videos are typed "type_54").
func inlineMediaKind(message store.Message) string {
	path := strings.TrimSpace(message.MediaPath)
	if path == "" {
		// No path (never downloaded): a card, not a certain 404.
		return ""
	}
	contentType := mediaContentTypes[strings.ToLower(filepath.Ext(path))]
	if noBrowserDecoder[contentType] {
		// Decided here, before the type-string fallback below, which would
		// otherwise call it audio again.
		return ""
	}
	if kind := mediaKindForContentType(contentType); kind != "" {
		return kind
	}
	hint := strings.ToLower(message.MediaType + " " + message.MessageType)
	switch {
	case strings.Contains(hint, "sticker"):
		return "image"
	case strings.Contains(hint, "video"), strings.Contains(hint, "movie"), strings.Contains(hint, "gif"):
		return "video"
	case strings.Contains(hint, "audio"), strings.Contains(hint, "voice"), strings.Contains(hint, "ptt"):
		return "audio"
	case strings.Contains(hint, "image"), strings.Contains(hint, "photo"):
		return "image"
	}
	return ""
}

// noBrowserDecoder lists formats no browser plays. Partly supported ones
// (CAF, H.263 3GP in Safari) stay playable with an error fallback.
// canPlayType isn't reliable: Chrome answers "" for video/quicktime but
// plays .mov.
var noBrowserDecoder = map[string]bool{
	"audio/amr": true, // AMR-NB voice notes from older phones; dropped by every major browser
}

func mediaKindForContentType(contentType string) string {
	switch {
	case strings.HasPrefix(contentType, "image/"):
		return "image"
	case strings.HasPrefix(contentType, "video/"):
		return "video"
	case strings.HasPrefix(contentType, "audio/"):
		return "audio"
	}
	return ""
}

func parseBefore(w http.ResponseWriter, r *http.Request) (*time.Time, int64, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get("before"))
	rawPK := strings.TrimSpace(r.URL.Query().Get("before_pk"))
	if raw == "" {
		if rawPK != "" {
			http.Error(w, "before_pk requires before", http.StatusBadRequest)
			return nil, 0, false
		}
		return nil, 0, true
	}
	seconds, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || seconds <= 0 {
		http.Error(w, "before must be a positive unix timestamp", http.StatusBadRequest)
		return nil, 0, false
	}
	var beforePK int64
	if rawPK != "" {
		beforePK, err = strconv.ParseInt(rawPK, 10, 64)
		if err != nil || beforePK <= 0 {
			http.Error(w, "before_pk must be a positive integer", http.StatusBadRequest)
			return nil, 0, false
		}
	}
	before := time.Unix(seconds, 0).UTC()
	return &before, beforePK, true
}

func parseLimit(w http.ResponseWriter, r *http.Request, fallback int) (int, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get("limit"))
	if raw == "" {
		return fallback, true
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 || limit > 500 {
		http.Error(w, "limit must be between 1 and 500", http.StatusBadRequest)
		return 0, false
	}
	return limit, true
}

func randomToken() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("generate web viewer token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}

// setSecurityHeaders: media-src allows 'self' but not blob:, since players
// load signed URLs (images use blob URLs). Moving media to blobs needs
// media-src widened, or playback fails with no console message.
func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data: blob:; media-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		writeArchiveError(w)
	}
}

func writeArchiveError(w http.ResponseWriter) {
	http.Error(w, "archive unavailable", http.StatusInternalServerError)
}
