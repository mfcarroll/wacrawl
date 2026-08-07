package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/wacrawl/internal/store"
)

const (
	testHost  = "127.0.0.1:43210"
	testToken = "test-private-token"
)

func TestHandlerServesAuthenticatedArchiveViews(t *testing.T) {
	handler := testHandler(t)

	root := request(t, handler, "/", "")
	if root.Code != http.StatusOK || !strings.Contains(root.Body.String(), "wacrawl archive") {
		t.Fatalf("root status=%d body=%q", root.Code, root.Body.String())
	}
	if !strings.Contains(root.Body.String(), `rel="icon" href="/favicon.svg"`) {
		t.Fatalf("root missing favicon link: %q", root.Body.String())
	}
	if got := root.Header().Get("Content-Security-Policy"); !strings.Contains(got, "default-src 'none'") || !strings.Contains(got, "frame-ancestors 'none'") {
		t.Fatalf("content security policy = %q", got)
	}
	if got := root.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("cache control = %q", got)
	}
	for _, path := range []string{"/favicon.svg", "/favicon.ico"} {
		favicon := request(t, handler, path, "")
		if favicon.Code != http.StatusOK || favicon.Header().Get("Content-Type") != "image/svg+xml" || !strings.Contains(favicon.Body.String(), "<svg") {
			t.Fatalf("%s status=%d content-type=%q body=%q", path, favicon.Code, favicon.Header().Get("Content-Type"), favicon.Body.String())
		}
	}

	unauthorized := request(t, handler, "/api/status", "")
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.Code)
	}

	status := request(t, handler, "/api/status", testToken)
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"messages":3`) || strings.Contains(status.Body.String(), "db_path") {
		t.Fatalf("status code=%d body=%s", status.Code, status.Body.String())
	}

	chats := request(t, handler, "/api/chats?limit=10", testToken)
	if chats.Code != http.StatusOK || !strings.Contains(chats.Body.String(), `"name":"Launch Group"`) {
		t.Fatalf("chats code=%d body=%s", chats.Code, chats.Body.String())
	}

	messages := request(t, handler, "/api/messages?chat=123%40g.us&limit=10", testToken)
	if messages.Code != http.StatusOK || !strings.Contains(messages.Body.String(), `"text":"launch now"`) {
		t.Fatalf("messages code=%d body=%s", messages.Code, messages.Body.String())
	}
	if strings.Contains(messages.Body.String(), "media_path") || strings.Contains(messages.Body.String(), "private.jpg") || strings.Contains(messages.Body.String(), "media_url") {
		t.Fatalf("messages exposed private media location: %s", messages.Body.String())
	}

	search := request(t, handler, "/api/search?q=launch&limit=10", testToken)
	if search.Code != http.StatusOK || !strings.Contains(search.Body.String(), `"message_id":"m1"`) {
		t.Fatalf("search code=%d body=%s", search.Code, search.Body.String())
	}
	var results []map[string]any
	if err := json.Unmarshal(search.Body.Bytes(), &results); err != nil || len(results) == 0 {
		t.Fatalf("search json results=%#v err=%v", results, err)
	}
	snippet, _ := results[0]["snippet"].(string)
	if !strings.Contains(snippet, snippetStartMarker+"launch"+snippetEndMarker) {
		t.Fatalf("search snippet markers missing: %q", snippet)
	}
}

func TestHandlerMessagesBeforePagination(t *testing.T) {
	handler := testHandler(t)
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	cutoff := strconv.FormatInt(base.Add(-30*time.Second).Unix(), 10)
	page := request(t, handler, "/api/messages?chat=123%40g.us&limit=10&before="+cutoff, testToken)
	if page.Code != http.StatusOK {
		t.Fatalf("before page status=%d body=%s", page.Code, page.Body.String())
	}
	var messages []map[string]any
	if err := json.Unmarshal(page.Body.Bytes(), &messages); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0]["message_id"] != "m1" || messages[1]["message_id"] != "m3" {
		t.Fatalf("before page messages=%#v", messages)
	}
	if messages[0]["source_pk"] != float64(1) {
		t.Fatalf("before page source_pk=%#v", messages[0]["source_pk"])
	}

	// A composite cursor advances within the shared second: m1 and m3 have the
	// same timestamp, so paging from m3 must return only m1.
	sameSecond := strconv.FormatInt(base.Add(-time.Minute).Unix(), 10)
	page = request(t, handler, "/api/messages?chat=123%40g.us&limit=10&before="+sameSecond+"&before_pk=3", testToken)
	if page.Code != http.StatusOK {
		t.Fatalf("before_pk page status=%d body=%s", page.Code, page.Body.String())
	}
	messages = nil
	if err := json.Unmarshal(page.Body.Bytes(), &messages); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0]["message_id"] != "m1" {
		t.Fatalf("before_pk page messages=%#v", messages)
	}

	for _, invalid := range []string{"abc", "-5", "0"} {
		response := request(t, handler, "/api/messages?chat=123%40g.us&before="+invalid, testToken)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("before=%q status=%d", invalid, response.Code)
		}
	}
	response := request(t, handler, "/api/messages?chat=123%40g.us&before="+cutoff+"&before_pk=0", testToken)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("before_pk=0 status=%d", response.Code)
	}
	response = request(t, handler, "/api/messages?chat=123%40g.us&before_pk=3", testToken)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("before_pk without before status=%d", response.Code)
	}
}

func TestHandlerServesInlineMedia(t *testing.T) {
	ctx := context.Background()
	archive, err := store.Open(ctx, filepath.Join(t.TempDir(), "media.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = archive.Close() })

	dir := filepath.Join(filepath.Dir(archive.Path()), "media")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	imagePath := filepath.Join(dir, "photo.png")
	imageBytes := writeTestPNG(t, imagePath)
	textPath := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(textPath, []byte("plain text, definitely not an image"), 0o600); err != nil {
		t.Fatal(err)
	}
	sourceRoot := t.TempDir()
	sourceImagePath := filepath.Join(sourceRoot, "source-photo.png")
	sourceImageBytes := writeTestPNG(t, sourceImagePath)

	const jid = "555@s.whatsapp.net"
	now := time.Date(2026, 7, 2, 9, 0, 0, 0, time.UTC)
	err = archive.ReplaceAll(ctx, store.ImportStats{FinishedAt: now}, nil, []store.Chat{
		{JID: jid, Kind: "dm", Name: "Media Tester", LastMessageAt: now},
	}, nil, nil, []store.Message{
		{SourcePK: 1, ChatJID: jid, MessageID: "p1", Timestamp: now, MediaType: "image", MediaPath: imagePath},
		{SourcePK: 2, ChatJID: jid, MessageID: "p2", Timestamp: now, Text: "no media"},
		{SourcePK: 3, ChatJID: jid, MessageID: "p3", Timestamp: now, MediaType: "image", MediaPath: textPath},
		{SourcePK: 4, ChatJID: jid, MessageID: "p4", Timestamp: now, MediaType: "image", MediaPath: filepath.Join(dir, "missing.jpg")},
		{SourcePK: 5, ChatJID: jid, MessageID: "p5", Timestamp: now, MediaType: "image", MediaPath: sourceImagePath},
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(archive, testToken, testHost, sourceRoot)
	t.Cleanup(handler.close)

	unauthorized := request(t, handler, "/api/media?pk=1", "")
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized media status = %d", unauthorized.Code)
	}

	image := request(t, handler, "/api/media?pk=1", testToken)
	if image.Code != http.StatusOK || image.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("media status=%d content-type=%q", image.Code, image.Header().Get("Content-Type"))
	}
	if !bytes.Equal(image.Body.Bytes(), imageBytes) {
		t.Fatalf("media bytes mismatch: got %d want %d", image.Body.Len(), len(imageBytes))
	}
	sourceImage := request(t, handler, "/api/media?pk=5", testToken)
	if sourceImage.Code != http.StatusOK || !bytes.Equal(sourceImage.Body.Bytes(), sourceImageBytes) {
		t.Fatalf("source media status=%d bytes=%d want %d", sourceImage.Code, sourceImage.Body.Len(), len(sourceImageBytes))
	}

	for _, tc := range []struct {
		query string
		code  int
	}{
		{"pk=2", http.StatusNotFound},             // message without media
		{"pk=3", http.StatusUnsupportedMediaType}, // bytes are not an image
		{"pk=4", http.StatusNotFound},             // file missing on disk
		{"pk=99", http.StatusNotFound},            // unknown message
		{"pk=abc", http.StatusBadRequest},
		{"pk=0", http.StatusBadRequest},
		{"pk=", http.StatusBadRequest},
	} {
		response := request(t, handler, "/api/media?"+tc.query, testToken)
		if response.Code != tc.code {
			t.Fatalf("media %s status=%d want %d", tc.query, response.Code, tc.code)
		}
	}
}

func TestHandlerRejectsMediaOutsideAllowedRoots(t *testing.T) {
	ctx := context.Background()
	archive, err := store.Open(ctx, filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = archive.Close() })

	mediaRoot := filepath.Join(filepath.Dir(archive.Path()), "media")
	if err := os.MkdirAll(mediaRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	secretPath := filepath.Join(filepath.Dir(mediaRoot), "secret.png")
	writeTestPNG(t, secretPath)
	outsidePath := filepath.Join(t.TempDir(), "outside.png")
	writeTestPNG(t, outsidePath)
	const jid = "555@s.whatsapp.net"
	now := time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)
	messages := []store.Message{
		{SourcePK: 1, ChatJID: jid, MessageID: "outside", Timestamp: now, MediaType: "image", MediaPath: outsidePath},
		{SourcePK: 2, ChatJID: jid, MessageID: "traversal", Timestamp: now, MediaType: "image", MediaPath: mediaRoot + string(os.PathSeparator) + ".." + string(os.PathSeparator) + filepath.Base(secretPath)},
		{SourcePK: 4, ChatJID: jid, MessageID: "relative", Timestamp: now, MediaType: "image", MediaPath: "media/photo.png"},
	}
	sourcePKs := []int{1, 2, 4}
	symlinkPath := filepath.Join(mediaRoot, "linked.png")
	if err := os.Symlink(outsidePath, symlinkPath); err != nil {
		t.Logf("symlink escape case unavailable: %v", err)
	} else {
		messages = append(messages, store.Message{SourcePK: 3, ChatJID: jid, MessageID: "symlink", Timestamp: now, MediaType: "image", MediaPath: symlinkPath})
		sourcePKs = append(sourcePKs, 3)
	}
	if err := archive.ReplaceAll(ctx, store.ImportStats{FinishedAt: now}, nil, []store.Chat{
		{JID: jid, Kind: "dm", Name: "Media Tester", LastMessageAt: now},
	}, nil, nil, messages); err != nil {
		t.Fatal(err)
	}

	handler := NewHandler(archive, testToken, testHost)
	t.Cleanup(handler.close)
	for _, sourcePK := range sourcePKs {
		response := request(t, handler, "/api/media?pk="+strconv.Itoa(sourcePK), testToken)
		if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), "media unavailable") {
			t.Fatalf("media pk=%d status=%d body=%q", sourcePK, response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), "secret") || strings.Contains(response.Body.String(), "outside") {
			t.Fatalf("media pk=%d exposed path in body %q", sourcePK, response.Body.String())
		}
	}
}

func TestHandlerReportsUnusableMediaRoots(t *testing.T) {
	ctx := context.Background()
	archive, err := store.Open(ctx, filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = archive.Close() })
	usable := filepath.Join(filepath.Dir(archive.Path()), "media")
	if err := os.MkdirAll(usable, 0o700); err != nil {
		t.Fatal(err)
	}
	// The realistic case: the archive was indexed against a volume that is not
	// mounted now, which otherwise looks healthy until every attachment 404s.
	unmounted := filepath.Join(t.TempDir(), "Volumes", "Backups", "WhatsApp")

	// An archive whose media directory was never created — the normal state
	// without --copy-media — must not be reported while a source root works.
	noCopyMedia := filepath.Join(t.TempDir(), "elsewhere.db")
	sideArchive, err := store.Open(ctx, noCopyMedia)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sideArchive.Close() })
	quietWithSource := NewHandler(sideArchive, testToken, testHost, usable)
	t.Cleanup(quietWithSource.close)
	if got := quietWithSource.mediaRootWarning(); got != "" {
		t.Fatalf("warned about an absent archive media dir while a source root works: %q", got)
	}

	handler := NewHandler(archive, testToken, testHost, unmounted)
	t.Cleanup(handler.close)
	warning := handler.mediaRootWarning()
	if !strings.Contains(warning, unmounted) || !strings.Contains(warning, "missing or unreadable") {
		t.Fatalf("warning = %q, must name the unusable root and why", warning)
	}
	if strings.Contains(warning, "no media directory is readable") {
		t.Fatalf("warning overstates the problem while a root still works: %q", warning)
	}
	if len(handler.allowedMediaRoots) != 1 {
		t.Fatalf("usable roots = %d want 1", len(handler.allowedMediaRoots))
	}

	// A source root that is not a path at all: older archives store a
	// "wa-store:" identity in the same field.
	identity := NewHandler(archive, testToken, testHost, "wa-store:abc123")
	t.Cleanup(identity.close)
	if got := identity.mediaRootWarning(); !strings.Contains(got, "not an absolute path") {
		t.Fatalf("identity warning = %q", got)
	}

	// Everything resolving must stay silent, or the warning becomes noise.
	quiet := NewHandler(archive, testToken, testHost)
	t.Cleanup(quiet.close)
	if got := quiet.mediaRootWarning(); got != "" {
		t.Fatalf("warning on a healthy archive = %q", got)
	}
}

// TestInlineMediaKind covers every media_type/message_type/extension combination
// observed in a real WhatsApp Desktop store of ~127k messages, plus the cases
// that only the type strings can answer.
func TestInlineMediaKind(t *testing.T) {
	for _, tc := range []struct {
		mediaType   string
		messageType string
		path        string
		want        string
	}{
		// Observed in a real store, with the count each accounted for.
		{"image", "image", "/m/IMG.jpg", "image"},      // 2286
		{"video", "video", "/m/VID.mp4", "video"},      // 39
		{"document", "document", "/m/doc.pdf", ""},     // 28
		{"audio", "audio", "/m/PTT.opus", "audio"},     // 25
		{"gif", "gif", "/m/GIF.mp4", "video"},          // 15 — typed gif, stored mp4
		{"sticker", "sticker", "/m/STK.webp", "image"}, // 8
		{"", "type_54", "/m/VID.mp4", "video"},         // 1 — no usable type at all
		{"audio", "audio", "/m/AUD.m4a", "audio"},      // 1
		{"document", "document", "/m/book.epub", ""},   // 1
		{"document", "document", "/m/notes.txt", ""},   // 1

		// An unfamiliar extension falls back to the type strings.
		{"image", "photo", "/m/IMG.thumb", "image"},
		{"video", "video", "/m/clip.bin", "video"},
		{"audio", "ptt", "/m/note", "audio"},
		{"", "voice_message", "/m/note", "audio"},
		{"sticker", "sticker", "/m/s", "image"},
		{"", "", "/m/whatever", ""},

		// Extension wins over a type string that disagrees with it.
		{"image", "image", "/m/mislabelled.mp4", "video"},

		// No path means nothing can ever be served, whatever the type says.
		{"image", "image", "", ""},
		{"video", "video", "  ", ""},
	} {
		got := inlineMediaKind(store.Message{MediaType: tc.mediaType, MessageType: tc.messageType, MediaPath: tc.path})
		if got != tc.want {
			t.Errorf("inlineMediaKind(%q, %q, %q) = %q want %q", tc.mediaType, tc.messageType, tc.path, got, tc.want)
		}
	}
}

// Leading bytes captured from files a real encoder produced, so the interplay
// between Go's sniffer and the extension table is exercised against container
// headers rather than against a guess at what they look like.
var realHeaders = map[string][]byte{
	"3gp":  []byte("\x00\x00\x00\x1cftyp3gp4\x00\x00\x02\x003gp4isomiso2\x00\x00\x00\x08free\x00\x00\x94\x28mdat"),
	"mov":  []byte("\x00\x00\x00\x14ftypqt  \x00\x00\x02\x00qt  \x00\x00\x00\x08wide\x00\x00\x1b\x1bmdat"),
	"webm": []byte("\x1aE\xdf\xa3\x9fB\x86\x81\x01B\xf7\x81\x01B\xf2\x81\x04B\xf3\x81\x08B\x82\x84webmB\x87\x81\x02B\x85\x81\x02"),
	"caf":  []byte("caff\x00\x01\x00\x00desc\x00\x00\x00\x00\x00\x00\x00\x20\x40\xe5\x88\x80\x00\x00\x00\x00lpcm"),
	"m4a":  []byte("\x00\x00\x00\x1cftypM4A \x00\x00\x02\x00M4A isomiso2\x00\x00\x00\x08free\x00\x00\x44\xd8mdat"),
	"mp3":  []byte("ID3\x04\x00\x00\x00\x00\x00\x23TSSE\x00\x00\x00\x0f\x00\x00\x03Lavf62.12.102"),
	"opus": []byte("OggS\x00\x02\x00\x00\x00\x00\x00\x00\x00\x00\xaa\x7e\x62\x1c\x00\x00\x00\x00\xbbo\x22z\x01\x13OpusHead"),
}

func TestInlineContentType(t *testing.T) {
	html := []byte("<!DOCTYPE html><html><body>hi</body></html>")
	for _, tc := range []struct {
		name  string
		sniff []byte
		want  string
	}{
		{"clip.mp4", mp4Header, "video/mp4"},
		// QuickTime and 3GP are ISO-BMFF like mp4, but neither declares an mp4
		// brand, so the sniffer gives up and the extension decides.
		{"clip.mov", realHeaders["mov"], "video/quicktime"},
		{"clip.3gp", realHeaders["3gp"], "video/3gpp"},
		// CAF has no signature Go knows at all.
		{"voice.caf", realHeaders["caf"], "audio/x-caf"},
		// Ogg is recognised, but only as a container; it names no audio type.
		{"voice.opus", realHeaders["opus"], "audio/ogg"},
		// Shared container: the sniffer says video/mp4, but this plays as audio.
		{"voice.m4a", realHeaders["m4a"], "audio/mp4"},
		// Sniffed and claimed agree here; either answer would do.
		{"voice.mp3", realHeaders["mp3"], "audio/mpeg"},
		{"clip.webm", realHeaders["webm"], "video/webm"},
		// No extension to go on, so the sniffed type stands.
		{"photo", []byte("GIF89a...."), "image/gif"},
		// Bytes veto a lying extension.
		{"trap.mp4", html, ""},
		{"notes.txt", []byte("plain text, definitely not an image"), ""},
		{"unknown.bin", []byte{0x00, 0x01, 0x02, 0x03}, ""},
	} {
		got, ok := inlineContentType(tc.name, tc.sniff)
		if !ok {
			got = ""
		}
		if got != tc.want {
			t.Errorf("inlineContentType(%q) = %q want %q", tc.name, got, tc.want)
		}
	}
}

// mp4Header is a minimal ISO base media file ftyp box, enough for both Go's
// sniffer and this package to treat the bytes as an mp4 container.
var mp4Header = []byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom")

func TestHandlerStreamsPlayableMedia(t *testing.T) {
	ctx := context.Background()
	archive, err := store.Open(ctx, filepath.Join(t.TempDir(), "media.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = archive.Close() })

	dir := filepath.Join(filepath.Dir(archive.Path()), "media")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	videoBytes := append(append([]byte{}, mp4Header...), bytes.Repeat([]byte("video payload "), 64)...)
	videoPath := filepath.Join(dir, "clip.mp4")
	if err := os.WriteFile(videoPath, videoBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	voicePath := filepath.Join(dir, "voice.m4a")
	if err := os.WriteFile(voicePath, videoBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	docPath := filepath.Join(dir, "report.pdf")
	if err := os.WriteFile(docPath, []byte("%PDF-1.7\nnot media"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Sparse, so the oversize cases cost no real disk.
	bigVideoPath := filepath.Join(dir, "long.mp4")
	writeSparseFile(t, bigVideoPath, maxInlineMediaBytes+1)
	bigImagePath := filepath.Join(dir, "huge.png")
	writeSparseFile(t, bigImagePath, maxInlineMediaBytes+1)

	const jid = "555@s.whatsapp.net"
	now := time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC)
	err = archive.ReplaceAll(ctx, store.ImportStats{FinishedAt: now}, nil, []store.Chat{
		{JID: jid, Kind: "dm", Name: "Media Tester", LastMessageAt: now},
	}, nil, nil, []store.Message{
		{SourcePK: 1, ChatJID: jid, MessageID: "v1", Timestamp: now, MediaType: "video", MediaPath: videoPath, MediaURL: "https://cdn.invalid/private-video"},
		{SourcePK: 2, ChatJID: jid, MessageID: "a1", Timestamp: now, MediaType: "audio", MessageType: "ptt", MediaPath: voicePath},
		{SourcePK: 3, ChatJID: jid, MessageID: "d1", Timestamp: now, MediaType: "document", MediaPath: docPath},
		{SourcePK: 4, ChatJID: jid, MessageID: "v2", Timestamp: now, MediaType: "video", MediaPath: bigVideoPath},
		{SourcePK: 5, ChatJID: jid, MessageID: "i2", Timestamp: now, MediaType: "image", MediaPath: bigImagePath},
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(archive, testToken, testHost)
	t.Cleanup(handler.close)

	video := request(t, handler, "/api/media?pk=1", testToken)
	if video.Code != http.StatusOK || video.Header().Get("Content-Type") != "video/mp4" {
		t.Fatalf("video status=%d content-type=%q", video.Code, video.Header().Get("Content-Type"))
	}
	if !bytes.Equal(video.Body.Bytes(), videoBytes) {
		t.Fatalf("video bytes = %d want %d", video.Body.Len(), len(videoBytes))
	}
	if got := video.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Fatalf("accept-ranges = %q, seeking needs ranged requests", got)
	}
	if voice := request(t, handler, "/api/media?pk=2", testToken); voice.Header().Get("Content-Type") != "audio/mp4" {
		t.Fatalf("voice note content-type = %q, must not be typed as video", voice.Header().Get("Content-Type"))
	}
	if document := request(t, handler, "/api/media?pk=3", testToken); document.Code != http.StatusNotFound {
		t.Fatalf("document status = %d, documents have no inline preview", document.Code)
	}
	// The size cap guards browser decode of a whole image; ranged video is exempt.
	if big := request(t, handler, "/api/media?pk=4", testToken); big.Code != http.StatusOK {
		t.Fatalf("oversize video status = %d, streamed media is not capped", big.Code)
	}
	if big := request(t, handler, "/api/media?pk=5", testToken); big.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize image status = %d want 413", big.Code)
	}

	ranged := httptest.NewRequest(http.MethodGet, "http://"+testHost+"/api/media?pk=1", nil)
	ranged.Host = testHost
	ranged.Header.Set("Authorization", "Bearer "+testToken)
	ranged.Header.Set("Range", "bytes=10-19")
	partial := httptest.NewRecorder()
	handler.ServeHTTP(partial, ranged)
	if partial.Code != http.StatusPartialContent {
		t.Fatalf("ranged status = %d want 206", partial.Code)
	}
	if !bytes.Equal(partial.Body.Bytes(), videoBytes[10:20]) {
		t.Fatalf("ranged body = %q want %q", partial.Body.Bytes(), videoBytes[10:20])
	}
}

func TestHandlerAuthenticatesMediaBySignedURL(t *testing.T) {
	ctx := context.Background()
	archive, err := store.Open(ctx, filepath.Join(t.TempDir(), "media.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = archive.Close() })

	dir := filepath.Join(filepath.Dir(archive.Path()), "media")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	videoPath := filepath.Join(dir, "clip.mp4")
	if err := os.WriteFile(videoPath, mp4Header, 0o600); err != nil {
		t.Fatal(err)
	}
	imagePath := filepath.Join(dir, "photo.png")
	writeTestPNG(t, imagePath)

	const jid = "555@s.whatsapp.net"
	now := time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC)
	if err := archive.ReplaceAll(ctx, store.ImportStats{FinishedAt: now}, nil, []store.Chat{
		{JID: jid, Kind: "dm", Name: "Media Tester", LastMessageAt: now},
	}, nil, nil, []store.Message{
		{SourcePK: 1, ChatJID: jid, MessageID: "v1", Timestamp: now, MediaType: "video", MediaPath: videoPath},
		{SourcePK: 2, ChatJID: jid, MessageID: "i1", Timestamp: now, MediaType: "image", MediaPath: imagePath},
	}); err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(archive, testToken, testHost)
	t.Cleanup(handler.close)

	listed := request(t, handler, "/api/messages?chat="+url.QueryEscape(jid), testToken)
	if listed.Code != http.StatusOK {
		t.Fatalf("messages status = %d", listed.Code)
	}
	var messages []messageResponse
	if err := json.Unmarshal(listed.Body.Bytes(), &messages); err != nil {
		t.Fatal(err)
	}
	byPK := map[int64]messageResponse{}
	for _, message := range messages {
		byPK[message.SourcePK] = message
	}
	if got := byPK[1].MediaKind; got != "video" {
		t.Fatalf("video media_kind = %q", got)
	}
	// Images are fetched with the bearer token, so they need no signed link.
	if byPK[2].MediaKind != "image" || byPK[2].MediaSrc != "" {
		t.Fatalf("image kind=%q url=%q", byPK[2].MediaKind, byPK[2].MediaSrc)
	}
	signed := byPK[1].MediaSrc
	if signed == "" {
		t.Fatal("video message carries no signed media URL")
	}
	// The signed local link must never be confused with, or accompanied by,
	// WhatsApp's remote CDN address for the same attachment.
	if body := listed.Body.String(); strings.Contains(body, "cdn.invalid") || strings.Contains(body, `"media_url"`) || strings.Contains(body, "media_path") {
		t.Fatalf("messages response leaked a private media location: %s", body)
	}
	// The whole point: a <video> element sends no Authorization header.
	if response := request(t, handler, signed, ""); response.Code != http.StatusOK {
		t.Fatalf("signed media status = %d want 200", response.Code)
	}
	if response := request(t, handler, "/api/media?pk=1", ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned media status = %d want 401", response.Code)
	}
	for name, tampered := range map[string]string{
		"wrong signature": signed[:len(signed)-1] + flipLast(signed),
		"another message": strings.Replace(signed, "pk=1", "pk=2", 1),
		"stretched expiry": strings.Replace(signed, "exp="+strconv.FormatInt(mediaExpiry(t, signed), 10),
			"exp="+strconv.FormatInt(mediaExpiry(t, signed)+3600, 10), 1),
	} {
		if response := request(t, handler, tampered, ""); response.Code != http.StatusUnauthorized {
			t.Fatalf("%s status = %d want 401", name, response.Code)
		}
	}

	// An expired link is refused even though its signature is intact.
	expired := httptest.NewRequest(http.MethodGet, "http://"+testHost+handler.signedMediaURL(1, time.Now()), nil)
	if handler.signedMediaRequest(expired, time.Now().Add(mediaURLTTL+time.Minute)) {
		t.Fatal("expired signed media URL still accepted")
	}
}

func mediaExpiry(t *testing.T, signed string) int64 {
	t.Helper()
	parsed, err := url.Parse(signed)
	if err != nil {
		t.Fatal(err)
	}
	expiry, err := strconv.ParseInt(parsed.Query().Get("exp"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return expiry
}

func flipLast(signed string) string {
	if strings.HasSuffix(signed, "A") {
		return "B"
	}
	return "A"
}

// writeSparseFile makes a file of the given length without allocating blocks for
// it, so size-limit cases stay cheap. The bytes read back are zeros.
func writeSparseFile(t *testing.T, path string, size int64) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	if err := file.Truncate(size); err != nil {
		t.Fatal(err)
	}
}

func writeTestPNG(t *testing.T, path string) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for x := 0; x < 4; x++ {
		for y := 0; y < 4; y++ {
			img.Set(x, y, color.RGBA{R: uint8(60 * x), G: 168, B: 132, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestHandlerRejectsHostMethodsAndInvalidQueries(t *testing.T) {
	handler := testHandler(t)

	req := httptest.NewRequest(http.MethodGet, "http://malicious.invalid/api/status", nil)
	req.Host = "malicious.invalid"
	req.Header.Set("Authorization", "Bearer "+testToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusMisdirectedRequest {
		t.Fatalf("wrong host status = %d", response.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "http://"+testHost+"/api/status", nil)
	req.Host = testHost
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("post status=%d allow=%q", response.Code, response.Header().Get("Allow"))
	}

	invalidLimit := request(t, handler, "/api/chats?limit=501", testToken)
	if invalidLimit.Code != http.StatusBadRequest {
		t.Fatalf("invalid limit status = %d", invalidLimit.Code)
	}

	emptySearch := request(t, handler, "/api/search?q=", testToken)
	if emptySearch.Code != http.StatusBadRequest {
		t.Fatalf("empty search status = %d", emptySearch.Code)
	}

	notFound := request(t, handler, "/api/missing", testToken)
	if notFound.Code != http.StatusNotFound {
		t.Fatalf("api missing status = %d", notFound.Code)
	}
	notFound = request(t, handler, "/missing", "")
	if notFound.Code != http.StatusNotFound {
		t.Fatalf("static missing status = %d", notFound.Code)
	}

	wrongToken := request(t, handler, "/api/status", "test-private-tokem")
	if wrongToken.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d", wrongToken.Code)
	}
}

func TestServeLifecycleAndPrivateURL(t *testing.T) {
	archive := testArchive(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	output := &captureWriter{written: make(chan string, 1)}
	result := make(chan error, 1)
	go func() {
		result <- Serve(ctx, archive, Config{Output: output})
	}()

	var printed string
	select {
	case printed = <-output.written:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not print its private URL")
	}
	var privateURL string
	for _, field := range strings.Fields(printed) {
		if strings.HasPrefix(field, "http://") {
			privateURL = field
			break
		}
	}
	parsed, err := url.Parse(privateURL)
	if err != nil || parsed.Host == "" || parsed.Fragment == "" {
		t.Fatalf("private URL = %q parsed=%#v err=%v", privateURL, parsed, err)
	}
	if parsed.Hostname() != "127.0.0.1" {
		t.Fatalf("viewer host = %q", parsed.Hostname())
	}

	rootURL := *parsed
	rootURL.Fragment = ""
	response, err := http.Get(rootURL.String()) // #nosec G107 -- test URL is a fresh loopback listener.
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("root status = %d", response.StatusCode)
	}

	apiURL := rootURL
	apiURL.Path = "/api/status"
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, apiURL.String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+parsed.Fragment)
	response, err = http.DefaultClient.Do(req) // #nosec G704 -- test URL is derived from Serve's fresh loopback listener.
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("api status = %d", response.StatusCode)
	}

	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Serve returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not stop after context cancellation")
	}
}

func TestServeRejectsInvalidOrOccupiedPorts(t *testing.T) {
	if err := Serve(context.Background(), nil, Config{Port: -1}); err == nil || !strings.Contains(err.Error(), "between 0 and 65535") {
		t.Fatalf("invalid port error = %v", err)
	}

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	port := listener.Addr().(*net.TCPAddr).Port
	err = Serve(context.Background(), nil, Config{Port: port})
	if err == nil || !strings.Contains(err.Error(), "listen for web viewer") {
		t.Fatalf("occupied port error = %v", err)
	}
}

func TestHandlerArchiveAndAssetFailures(t *testing.T) {
	archive := testArchive(t)
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(archive, testToken, testHost)
	t.Cleanup(h.close)
	for _, path := range []string{"/api/status", "/api/chats", "/api/messages"} {
		response := request(t, h, path, testToken)
		if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), "archive unavailable") {
			t.Fatalf("%s status=%d body=%q", path, response.Code, response.Body.String())
		}
	}

	response := httptest.NewRecorder()
	(&handler{allowedHost: testHost}).serveAsset(response, "static/missing", "text/plain")
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("missing asset status = %d", response.Code)
	}

	failing := &failingResponseWriter{header: make(http.Header)}
	writeJSON(failing, map[string]string{"ok": "yes"})
	if failing.writes < 2 {
		t.Fatalf("writeJSON failure writes = %d", failing.writes)
	}
}

func testHandler(t *testing.T) http.Handler {
	t.Helper()
	h := NewHandler(testArchive(t), testToken, testHost)
	t.Cleanup(h.close)
	return h
}

func testArchive(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	archive, err := store.Open(ctx, filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = archive.Close() })
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	err = archive.ReplaceAll(ctx, store.ImportStats{FinishedAt: now}, nil, []store.Chat{
		{JID: "123@g.us", Kind: "group", Name: "Launch Group", LastMessageAt: now, UnreadCount: 1},
	}, nil, nil, []store.Message{
		{SourcePK: 1, ChatJID: "123@g.us", ChatName: "Launch Group", MessageID: "m1", SenderName: "Alice", Timestamp: now.Add(-time.Minute), Text: "launch now", MediaType: "image", MediaPath: "/private/media/private.jpg", MediaURL: "https://example.invalid/private.jpg"},
		{SourcePK: 2, ChatJID: "123@g.us", ChatName: "Launch Group", MessageID: "m2", Timestamp: now, FromMe: true, Text: "ship later"},
		{SourcePK: 3, ChatJID: "123@g.us", ChatName: "Launch Group", MessageID: "m3", SenderName: "Alice", Timestamp: now.Add(-time.Minute), Text: "launch encore"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return archive
}

func request(t *testing.T, handler http.Handler, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://"+testHost+path, nil)
	req.Host = testHost
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	return response
}

type captureWriter struct {
	written chan string
}

func (w *captureWriter) Write(body []byte) (int, error) {
	select {
	case w.written <- string(body):
	default:
	}
	return len(body), nil
}

type failingResponseWriter struct {
	header http.Header
	writes int
}

func (w *failingResponseWriter) Header() http.Header {
	return w.header
}

func (w *failingResponseWriter) Write([]byte) (int, error) {
	w.writes++
	return 0, errors.New("write failed")
}

func (w *failingResponseWriter) WriteHeader(int) {}
