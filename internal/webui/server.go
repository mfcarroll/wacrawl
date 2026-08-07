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
	"net"
	"net/http"
	"os"
	"path/filepath"
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
	// viewer falls back to the metadata card instead of inlining bytes. It
	// applies to images alone: the browser decodes those whole, while video and
	// audio arrive in ranges, where a 3 GB file costs no more than a 3 MB one.
	maxInlineMediaBytes = 25 << 20

	// mediaURLTTL bounds how long a signed media URL stays valid. The signing
	// key is the per-run token, so links already die with the server; the expiry
	// limits one that leaks out of a page left open for a long time.
	mediaURLTTL = 12 * time.Hour
)

type Config struct {
	Port   int
	Output io.Writer
	// AllowCloudMedia serves media whose bytes are held by a dataless-file
	// provider, materializing it on demand instead of reporting it missing.
	// Off by default: WhatsApp Desktop leaves stubs for media it never
	// downloaded, and blocking on those would wedge a request. Turn it on when
	// the archive's media lives in a cloud-synced folder.
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

// skippedMediaRoot records a configured media directory the viewer could not
// use, so startup can say so instead of leaving every attachment to 404.
type skippedMediaRoot struct {
	path   string
	reason string
	// optional marks a root whose absence is normal, so it is only worth
	// mentioning when it leaves the viewer with nothing readable at all.
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
	// MediaKind is image, video, or audio for attachments the viewer can render
	// inline, and empty for everything else. Classifying server-side keeps the
	// client from offering a player for something /api/media would refuse.
	MediaKind string `json:"media_kind,omitempty"`
	// MediaURL is a signed link to the bytes, present only for the kinds the
	// browser loads by URL rather than by fetch. See mediaSignature.
	MediaURL string `json:"media_url,omitempty"`
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
	// Print the URL before touching any media root. Resolving one can be slow or
	// can block outright — a network volume, or a directory macOS gates behind a
	// privacy prompt — and the address has to reach the operator regardless.
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
	// The archive's own media directory only exists once an import ran with
	// --copy-media, so its absence is ordinary rather than a fault.
	candidates := []mediaRootCandidate{{filepath.Join(filepath.Dir(archive.Path()), "media"), true}}
	for _, root := range sourceRoots {
		trimmed := strings.TrimSpace(root)
		if trimmed == "" {
			continue
		}
		// Source roots are stored canonical, so anything relative is not a path
		// at all: older archives can hold a "wa-store:" identity here instead.
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
			// For a configured source root, much the most common cause is that
			// the archive was indexed against a volume that is not mounted now.
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

// mediaRootWarning describes unusable media roots for the operator, or returns
// empty when everything resolved. Without it an archive indexed against an
// unmounted volume looks perfectly healthy — chats and search work, and every
// single attachment 404s with nothing said anywhere about why.
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
	// Media is the one endpoint a signed URL can reach without the header, because
	// <video> and <audio> cannot send one.
	authorized := h.authorized(r) || (r.URL.Path == "/api/media" && h.signedMediaRequest(r, time.Now()))
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

// mediaSignature authenticates a single media URL. A <video> or <audio> element
// loads its source itself and cannot attach the Authorization header the rest of
// the API uses, so each playable attachment gets its own link signed with the
// per-run token as key. Nothing durable lands in the DOM: a captured link grants
// one attachment until it expires, not the archive.
func (h *handler) mediaSignature(sourcePK, expiry int64) string {
	mac := hmac.New(sha256.New, []byte(h.token))
	// Delimited so no other pk and expiry pair can produce the same input.
	_, _ = fmt.Fprintf(mac, "media\x00%d\x00%d", sourcePK, expiry)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (h *handler) signedMediaURL(sourcePK int64, now time.Time) string {
	expiry := now.Add(mediaURLTTL).Unix()
	return fmt.Sprintf("/api/media?pk=%d&exp=%d&sig=%s", sourcePK, expiry, h.mediaSignature(sourcePK, expiry))
}

func (h *handler) signedMediaRequest(r *http.Request, now time.Time) bool {
	query := r.URL.Query()
	sourcePK, err := strconv.ParseInt(strings.TrimSpace(query.Get("pk")), 10, 64)
	if err != nil {
		return false
	}
	expiry, err := strconv.ParseInt(strings.TrimSpace(query.Get("exp")), 10, 64)
	if err != nil || now.Unix() > expiry {
		return false
	}
	want := h.mediaSignature(sourcePK, expiry)
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
		// Images are fetched with the bearer token and shown from a blob, so only
		// the streamed kinds need a URL the element can load on its own.
		mediaURL := ""
		if message.SourcePK > 0 && (kind == "video" || kind == "audio") {
			mediaURL = h.signedMediaURL(message.SourcePK, now)
		}
		out = append(out, messageResponse{
			SourcePK:    message.SourcePK,
			ChatJID:     message.ChatJID,
			ChatName:    message.ChatName,
			MessageID:   message.MessageID,
			SenderJID:   message.SenderJID,
			SenderName:  message.SenderName,
			Timestamp:   message.Timestamp,
			FromMe:      message.FromMe,
			Text:        message.Text,
			MessageType: message.MessageType,
			MediaType:   message.MediaType,
			MediaTitle:  message.MediaTitle,
			MediaSize:   message.MediaSize,
			Starred:     message.Starred,
			Snippet:     message.Snippet,
			MediaKind:   kind,
			MediaURL:    mediaURL,
		})
	}
	return out
}

// serveMedia streams archived media bytes for one message so the viewer can
// show photos, stickers, and GIFs inline and play video and voice notes. It
// reads local files referenced by configured archive or source roots, serves
// only image, video, and audio content, and never reveals the underlying path.
func (h *handler) serveMedia(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimSpace(r.URL.Query().Get("pk"))
	sourcePK, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || sourcePK <= 0 {
		http.Error(w, "pk must be a positive integer", http.StatusBadRequest)
		return
	}
	message, err := h.store.MessageBySourcePK(r.Context(), sourcePK)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		writeArchiveError(w)
		return
	}
	path := strings.TrimSpace(message.MediaPath)
	kind := inlineMediaKind(message)
	if path == "" || kind == "" {
		http.Error(w, "no inline preview", http.StatusNotFound)
		return
	}
	root, path, ok := containedMediaPath(path, h.allowedMediaRoots)
	if !ok {
		http.Error(w, "media unavailable", http.StatusNotFound)
		return
	}
	open := openMediaFile
	if h.allowCloudMedia {
		open = openMediaFileBlocking
	}
	file, err := open(root, path)
	if err != nil {
		http.Error(w, "media unavailable", http.StatusNotFound)
		return
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 {
		http.Error(w, "media unavailable", http.StatusNotFound)
		return
	}
	// WhatsApp keeps stubs for media it has not downloaded; reading one blocks
	// until macOS materializes it, which can wedge the request indefinitely.
	// Cloud-synced archives are the deliberate exception: there a placeholder is
	// the normal resting state and materializing it is exactly what is wanted.
	if !h.allowCloudMedia && !fileMaterialized(info) {
		http.Error(w, "media not downloaded locally", http.StatusNotFound)
		return
	}
	sniff := make([]byte, 512)
	n, err := io.ReadFull(file, sniff)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		http.Error(w, "media unavailable", http.StatusNotFound)
		return
	}
	contentType, ok := inlineContentType(path, sniff[:n])
	if !ok {
		http.Error(w, "unsupported media type", http.StatusUnsupportedMediaType)
		return
	}
	if strings.HasPrefix(contentType, "image/") && info.Size() > maxInlineMediaBytes {
		http.Error(w, "media too large for inline preview", http.StatusRequestEntityTooLarge)
		return
	}
	w.Header().Set("Content-Type", contentType)
	// ServeContent rather than io.Copy: video needs ranged requests to seek, and
	// Safari refuses to play a source that cannot serve them at all. It rewinds
	// the file itself, so the bytes read for sniffing above do not matter.
	http.ServeContent(w, r, "", info.ModTime(), file)
}

// mediaContentTypes names the types browsers need for the formats WhatsApp
// actually stores. Sniffing alone will not do: Go recognises no QuickTime, 3GP,
// CAF, or AMR signature and reports all of them as application/octet-stream, and
// it reports an .m4a voice note as video/mp4 because the container is shared.
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

// inlineContentType decides what to serve an attachment as, or refuses it. The
// extension is the better signal for media and wins where it is known, but the
// bytes keep a veto: a text file or a PDF named .mp4 is refused rather than
// streamed, because the sniffer positively identifies those.
func inlineContentType(name string, sniff []byte) (string, bool) {
	sniffed := http.DetectContentType(sniff)
	if !playableContentType(sniffed) && !inconclusiveSniff(sniffed) {
		return "", false
	}
	if byExtension := mediaContentTypes[strings.ToLower(filepath.Ext(name))]; byExtension != "" {
		return byExtension, true
	}
	if playableContentType(sniffed) {
		return sniffed, true
	}
	return "", false
}

func playableContentType(contentType string) bool {
	return mediaKindForContentType(contentType) != ""
}

// inconclusiveSniff reports whether the sniffer failed to place the bytes, in
// which case the extension is the only signal left. Ogg counts: it is the
// container for WhatsApp's Opus voice notes, but the sniffer cannot tell an
// audio stream inside it from any other and answers application/ogg.
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

// inlineMediaKind classifies an attachment into the family the viewer can render
// inline — image, video, or audio — and returns empty for everything else, which
// stays a metadata card.
//
// The stored file extension decides wherever it is known, for the same reason
// the served content type comes from it: WhatsApp's type strings describe what a
// message meant, not what the file is. Measured against a real Desktop store, an
// animated GIF is typed "gif" in both fields yet stored as an .mp4 and has to
// play in a video element, and some messages carry an opaque type like "type_54"
// over an ordinary .mp4. The type strings remain the fallback for anything whose
// extension is unfamiliar.
func inlineMediaKind(message store.Message) string {
	path := strings.TrimSpace(message.MediaPath)
	if path == "" {
		// Nothing can be served without a path, and most of an archive's media
		// rows have none: WhatsApp records the message but never downloaded the
		// file. Reporting no kind sends those straight to a card instead of
		// letting the viewer chase thousands of certain 404s while scrolling.
		return ""
	}
	if kind := mediaKindForContentType(mediaContentTypes[strings.ToLower(filepath.Ext(path))]); kind != "" {
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

// setSecurityHeaders locks the page down to what the viewer actually uses.
// Note that media-src allows 'self' but not blob:, because players load signed
// URLs directly. Images do go through blob URLs, which is why img-src lists it.
// Anything moving media onto the blob path has to widen media-src to match, or
// playback fails as an opaque MediaError with no console message.
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
