package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/wa"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func TestParseSyncWebhookAuth(t *testing.T) {
	hmac, err := ParseSyncWebhookAuth("")
	if err != nil || hmac != SyncWebhookAuthHMAC {
		t.Fatalf("empty auth = %q, %v; want hmac", hmac, err)
	}
	grok, err := ParseSyncWebhookAuth(" GROK ")
	if err != nil || grok != SyncWebhookAuthGrok || !grok.IsGrok() {
		t.Fatalf("grok auth = %q, %v", grok, err)
	}
	if _, err := ParseSyncWebhookAuth("bearer"); err == nil {
		t.Fatal("expected error for unknown auth")
	}
}

func TestCompileWebhookFilter(t *testing.T) {
	re, err := CompileWebhookFilter("")
	if err != nil || re != nil {
		t.Fatalf("empty filter = %v, %v; want nil", re, err)
	}
	re, err = CompileWebhookFilter("invoice|urgent")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !re.MatchString("see invoice 12") {
		t.Fatal("expected match")
	}
	if _, err := CompileWebhookFilter("("); err == nil {
		t.Fatal("expected invalid pattern error")
	}
	if _, err := CompileWebhookFilter(strings.Repeat("a", maxWebhookFilterRunes+1)); err == nil {
		t.Fatal("expected overlong pattern error")
	}
}

func TestFitGrokWebhookJSONStaysUnderBudget(t *testing.T) {
	text := strings.Repeat("invoice 你好 ", 800)
	payload, err := fitGrokWebhookJSON(grokWebhookPayload{
		ID:     "3EB0ABCDEF0123456789",
		Chat:   "120363012345678901@g.us",
		Sender: "15551234567@s.whatsapp.net",
		TS:     "2026-09-02T12:34:56Z",
		Text:   text,
	})
	if err != nil {
		t.Fatalf("fit: %v", err)
	}
	if len(payload) > grokWebhookJSONBudget {
		t.Fatalf("payload len %d exceeds budget %d", len(payload), grokWebhookJSONBudget)
	}
	if len(payload) > grokWebhookJSONCeiling {
		t.Fatalf("payload len %d exceeds ceiling %d", len(payload), grokWebhookJSONCeiling)
	}
	var got grokWebhookPayload
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.TextTruncated {
		t.Fatal("expected text_truncated")
	}
	if got.Text == "" || got.Text == text {
		t.Fatalf("truncated text = %q", got.Text)
	}
	if strings.Contains(string(payload), "MediaKey") || strings.Contains(string(payload), "DirectPath") {
		t.Fatalf("payload leaked media internals: %s", payload)
	}
}

func TestGrokWebhookPayloadOmitsMediaSecrets(t *testing.T) {
	a := newTestApp(t)
	pm := wa.ParsedMessage{
		Chat:      types.JID{User: "15551234567", Server: types.DefaultUserServer},
		ID:        "m-media",
		SenderJID: "15551234567@s.whatsapp.net",
		Timestamp: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC),
		Text:      "see attached",
		Media: &wa.Media{
			Type:       "document",
			Filename:   "invoice.pdf",
			MimeType:   "application/pdf",
			DirectPath: "/secret/path",
			MediaKey:   []byte("MEDIAKEYSECRET"),
			FileSHA256: []byte("FILESHASECRET"),
		},
	}
	payload, err := fitGrokWebhookJSON(a.newGrokWebhookPayload(context.Background(), pm))
	if err != nil {
		t.Fatalf("fit: %v", err)
	}
	body := string(payload)
	for _, leak := range []string{"MediaKey", "DirectPath", "FileSHA256", "MEDIAKEYSECRET", "FILESHASECRET", "/secret/path"} {
		if strings.Contains(body, leak) {
			t.Fatalf("payload leaked %q: %s", leak, body)
		}
	}
	if !strings.Contains(body, `"media_type":"document"`) || !strings.Contains(body, `"media_filename":"invoice.pdf"`) {
		t.Fatalf("missing media metadata: %s", body)
	}
}

func TestGrokWebhookPostsBearerAndAutomationKeyWithoutHMAC(t *testing.T) {
	a := newTestApp(t)
	type headers struct {
		auth      string
		autoKey   string
		signature string
		body      []byte
	}
	got := make(chan headers, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("ReadAll: %v", err)
			return
		}
		got <- headers{
			auth:      r.Header.Get("Authorization"),
			autoKey:   r.Header.Get("X-Automation-Key"),
			signature: r.Header.Get("X-Wacli-Signature"),
			body:      body,
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := a.postSyncWebhookEvent(context.Background(), SyncOptions{
		WebhookURL:          srv.URL,
		WebhookSecret:       "sender-key",
		WebhookAllowPrivate: true,
		WebhookAuth:         SyncWebhookAuthGrok,
	}, syncWebhookEvent{
		Kind: SyncWebhookEventMessage,
		Message: wa.ParsedMessage{
			Chat:      types.JID{User: "15551234567", Server: types.DefaultUserServer},
			ID:        "m-grok",
			SenderJID: "15551234567@s.whatsapp.net",
			Timestamp: time.Date(2026, 9, 2, 12, 34, 56, 0, time.UTC),
			PushName:  "Ada",
			Text:      "invoice 1842 is overdue",
		},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	select {
	case h := <-got:
		if h.auth != "Bearer sender-key" {
			t.Fatalf("Authorization = %q", h.auth)
		}
		if h.autoKey != "sender-key" {
			t.Fatalf("X-Automation-Key = %q", h.autoKey)
		}
		if h.signature != "" {
			t.Fatalf("unexpected HMAC signature %q", h.signature)
		}
		if bytes.Contains(h.body, []byte(`"ChatName"`)) || bytes.Contains(h.body, []byte(`"SenderJID"`)) {
			t.Fatalf("legacy payload in grok mode: %s", h.body)
		}
		if !bytes.Contains(h.body, []byte(`"id":"m-grok"`)) || !bytes.Contains(h.body, []byte(`"text":"invoice 1842 is overdue"`)) {
			t.Fatalf("compact payload missing fields: %s", h.body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for grok webhook")
	}
}

func TestGrokWebhookRequestTimeoutIs8s(t *testing.T) {
	if syncWebhookGrokRequestTimeout != 8*time.Second {
		t.Fatalf("grok timeout = %s, want 8s", syncWebhookGrokRequestTimeout)
	}
}

func TestGrokWebhookUsesRequestTimeout(t *testing.T) {
	a := newTestApp(t)
	oldClient := syncWebhookPrivateHTTPClient
	oldTimeout := syncWebhookGrokRequestTimeout
	t.Cleanup(func() {
		syncWebhookPrivateHTTPClient = oldClient
		syncWebhookGrokRequestTimeout = oldTimeout
	})

	syncWebhookGrokRequestTimeout = 20 * time.Millisecond
	ctxErr := make(chan error, 1)
	syncWebhookPrivateHTTPClient = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			<-req.Context().Done()
			err := req.Context().Err()
			ctxErr <- err
			return nil, err
		}),
	}

	start := time.Now()
	err := a.postSyncWebhookEvent(context.Background(), SyncOptions{
		WebhookURL:          "https://example.test/hook",
		WebhookAllowPrivate: true,
		WebhookAuth:         SyncWebhookAuthGrok,
	}, syncWebhookEvent{Kind: SyncWebhookEventMessage, Message: wa.ParsedMessage{ID: "m-timeout", Chat: types.JID{User: "15551234567", Server: types.DefaultUserServer}}})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("webhook timeout took %s, want under 1s", elapsed)
	}
	select {
	case err := <-ctxErr:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("request context error = %v, want deadline exceeded", err)
		}
	case <-time.After(time.Second):
		t.Fatal("transport did not observe request context cancellation")
	}
}

func TestGrokWebhookFiltersBeforeEnqueue(t *testing.T) {
	a := newTestApp(t)
	a.wa = newFakeWA()
	chat := types.JID{User: "120363000000000000", Server: types.GroupServer}
	other := types.JID{User: "15551234567", Server: types.DefaultUserServer}
	jobs := make(chan syncWebhookEvent, 2)
	opts := SyncOptions{
		WebhookAuth:     SyncWebhookAuthGrok,
		WebhookChatJIDs: []string{chat.String()},
		WebhookFilter:   regexp.MustCompile("invoice"),
	}
	var stored atomic.Int64
	enqueue := newSyncWebhookMessageEnqueuer(a.newSyncWebhookEnqueuer(context.Background(), jobs))

	a.handleLiveSyncMessage(context.Background(), opts, grokLiveEvent(other, "m-other", "invoice 1", false), &stored, func(string, string) {}, enqueue)
	a.handleLiveSyncMessage(context.Background(), opts, grokLiveEvent(chat, "m-nomatch", "hello", false), &stored, func(string, string) {}, enqueue)
	a.handleLiveSyncMessage(context.Background(), opts, grokLiveEvent(chat, "m-me", "invoice 2", true), &stored, func(string, string) {}, enqueue)
	select {
	case evt := <-jobs:
		t.Fatalf("filtered message was enqueued: %s", evt.Message.ID)
	default:
	}

	a.handleLiveSyncMessage(context.Background(), opts, grokLiveEvent(chat, "m-match", "see invoice 9", false), &stored, func(string, string) {}, enqueue)
	select {
	case evt := <-jobs:
		if evt.Message.ID != "m-match" {
			t.Fatalf("enqueued %s, want m-match", evt.Message.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("matching message was not enqueued")
	}

	opts.WebhookIncludeFromMe = true
	a.handleLiveSyncMessage(context.Background(), opts, grokLiveEvent(chat, "m-me2", "invoice from me", true), &stored, func(string, string) {}, enqueue)
	select {
	case evt := <-jobs:
		if evt.Message.ID != "m-me2" {
			t.Fatalf("enqueued %s, want m-me2", evt.Message.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("from_me message was not enqueued with --webhook-include-from-me")
	}
}

func TestGrokWebhookSkipsExistingRows(t *testing.T) {
	a := newTestApp(t)
	a.wa = newFakeWA()
	chat := types.JID{User: "15551234567", Server: types.DefaultUserServer}
	jobs := make(chan syncWebhookEvent, 2)
	opts := SyncOptions{
		WebhookAuth:     SyncWebhookAuthGrok,
		WebhookChatJIDs: []string{chat.String()},
	}
	var stored atomic.Int64
	enqueue := newSyncWebhookMessageEnqueuer(a.newSyncWebhookEnqueuer(context.Background(), jobs))
	evt := grokLiveEvent(chat, "m-dup", "hello", false)

	a.handleLiveSyncMessage(context.Background(), opts, evt, &stored, func(string, string) {}, enqueue)
	select {
	case <-jobs:
	case <-time.After(time.Second):
		t.Fatal("first live message was not enqueued")
	}

	evt.Message.Conversation = proto.String("edited")
	a.handleLiveSyncMessage(context.Background(), opts, evt, &stored, func(string, string) {}, enqueue)
	select {
	case got := <-jobs:
		t.Fatalf("reconnect upsert enqueued %s", got.Message.ID)
	default:
	}
}

func TestGrokWebhookFailedLogIsOwnerOnlyAndOmitsSecret(t *testing.T) {
	a := newTestApp(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	secret := "super-secret-sender-key"
	err := a.postSyncWebhookEvent(context.Background(), SyncOptions{
		WebhookURL:          srv.URL,
		WebhookSecret:       secret,
		WebhookAllowPrivate: true,
		WebhookAuth:         SyncWebhookAuthGrok,
	}, syncWebhookEvent{
		Kind: SyncWebhookEventMessage,
		Message: wa.ParsedMessage{
			Chat:      types.JID{User: "15551234567", Server: types.DefaultUserServer},
			ID:        "m-fail",
			Timestamp: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC),
			Text:      "invoice failed wake",
		},
	})
	if err == nil {
		t.Fatal("expected POST failure")
	}

	path := filepath.Join(a.StoreDir(), grokWebhookFailedLog)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat fallback log: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("fallback log mode = %04o, want 0600", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fallback log: %v", err)
	}
	if bytes.Contains(data, []byte(secret)) {
		t.Fatalf("fallback log contained sender key")
	}
	if !bytes.Contains(data, []byte(`"id":"m-fail"`)) {
		t.Fatalf("fallback log missing payload: %s", data)
	}
}

func TestGrokWebhookResolvesPNAndLIDChatFilter(t *testing.T) {
	a := newTestApp(t)
	f := newFakeWA()
	a.wa = f
	pn := types.JID{User: "15551234567", Server: types.DefaultUserServer}
	lid := types.JID{User: "999123456789", Server: types.HiddenUserServer}
	f.lids[lid.ToNonAD()] = pn

	jids, err := a.ResolveWebhookChatJIDs(context.Background(), pn.String())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	seen := map[string]bool{}
	for _, id := range jids {
		seen[id] = true
	}
	if !seen[pn.String()] || !seen[lid.String()] {
		t.Fatalf("resolved %v, want PN and LID", jids)
	}

	jobs := make(chan syncWebhookEvent, 1)
	opts := SyncOptions{WebhookAuth: SyncWebhookAuthGrok, WebhookChatJIDs: jids}
	var stored atomic.Int64
	enqueue := newSyncWebhookMessageEnqueuer(a.newSyncWebhookEnqueuer(context.Background(), jobs))
	a.handleLiveSyncMessage(context.Background(), opts, grokLiveEvent(lid, "m-lid", "hi", false), &stored, func(string, string) {}, enqueue)
	select {
	case evt := <-jobs:
		if evt.Message.ID != "m-lid" {
			t.Fatalf("enqueued %s", evt.Message.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("LID chat should match PN webhook-chat")
	}
}

func TestGrokWebhookRequiresHTTP200(t *testing.T) {
	a := newTestApp(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	err := a.postSyncWebhookEvent(context.Background(), SyncOptions{
		WebhookURL:          srv.URL,
		WebhookAllowPrivate: true,
		WebhookAuth:         SyncWebhookAuthGrok,
	}, syncWebhookEvent{Kind: SyncWebhookEventMessage, Message: wa.ParsedMessage{ID: "m-204", Chat: types.JID{User: "1", Server: types.DefaultUserServer}}})
	if err == nil {
		t.Fatal("expected grok webhook to reject HTTP 204")
	}
}

func TestGrokWebhookDoesNotPostHistorySync(t *testing.T) {
	a := newTestApp(t)
	f := newFakeWA()
	a.wa = f
	chat := types.JID{User: "15551234567", Server: types.DefaultUserServer}
	jobs := make(chan syncWebhookEvent, 1)
	opts := SyncOptions{
		Mode:            SyncModeFollow,
		WebhookURL:      "https://example.test/hook",
		WebhookAuth:     SyncWebhookAuthGrok,
		WebhookChatJIDs: []string{chat.String()},
		WebhookEvents:   SyncWebhookEventSet{SyncWebhookEventMessage: true},
	}
	ctx := context.Background()
	var stored, last atomic.Int64
	handlerID := a.addSyncEventHandler(ctx, opts, &stored, &last, make(chan struct{}, 1), make(chan struct{}, 1), make(chan staleReconnectRequest, 1), func(string, string) {}, a.newSyncWebhookEnqueuer(ctx, jobs), nil, &syncPresence{}, nil)
	t.Cleanup(func() { f.RemoveEventHandler(handlerID) })

	msg := &waWeb.WebMessageInfo{
		Key: &waCommon.MessageKey{
			RemoteJID: proto.String(chat.String()),
			FromMe:    proto.Bool(false),
			ID:        proto.String("history-1"),
		},
		MessageTimestamp: proto.Uint64(uint64(time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC).Unix())),
		Message:          &waProto.Message{Conversation: proto.String("invoice from history")},
	}
	f.emit(&events.HistorySync{Data: &waHistorySync.HistorySync{
		SyncType: waHistorySync.HistorySync_FULL.Enum(),
		Conversations: []*waHistorySync.Conversation{{
			ID:       proto.String(chat.String()),
			Messages: []*waHistorySync.HistorySyncMsg{{Message: msg}},
		}},
	}})
	select {
	case evt := <-jobs:
		t.Fatalf("history sync enqueued webhook %s", evt.Message.ID)
	case <-time.After(200 * time.Millisecond):
	}
}

func grokLiveEvent(chat types.JID, id, text string, fromMe bool) *events.Message {
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{
				Chat:     chat,
				Sender:   types.JID{User: "15551234567", Server: types.DefaultUserServer},
				IsFromMe: fromMe,
				IsGroup:  chat.Server == types.GroupServer,
			},
			ID:        id,
			Timestamp: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC),
			PushName:  "Ada",
		},
		Message: &waProto.Message{Conversation: proto.String(text)},
	}
}
