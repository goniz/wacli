package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/openclaw/wacli/internal/wa"
	"go.mau.fi/whatsmeow/types"
)

const (
	grokWebhookJSONBudget  = 3500
	grokWebhookJSONCeiling = 4000
	grokWebhookFailedLog   = "webhook-failed.ndjson"
	maxWebhookFilterRunes  = 256
)

var syncWebhookGrokRequestTimeout = 8 * time.Second

type grokWebhookPayload struct {
	ID            string `json:"id"`
	Chat          string `json:"chat"`
	ChatName      string `json:"chat_name,omitempty"`
	Sender        string `json:"sender,omitempty"`
	SenderName    string `json:"sender_name,omitempty"`
	TS            string `json:"ts"`
	FromMe        bool   `json:"from_me"`
	Text          string `json:"text,omitempty"`
	TextTruncated bool   `json:"text_truncated,omitempty"`
	MediaType     string `json:"media_type,omitempty"`
	MediaFilename string `json:"media_filename,omitempty"`
	MediaMIME     string `json:"media_mime,omitempty"`
}

func CompileWebhookFilter(raw string) (*regexp.Regexp, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if utf8.RuneCountInString(raw) > maxWebhookFilterRunes {
		return nil, fmt.Errorf("--webhook-filter is too long (max %d characters)", maxWebhookFilterRunes)
	}
	re, err := regexp.Compile(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid --webhook-filter: %w", err)
	}
	return re, nil
}

func (a *App) prepareSyncWebhook(ctx context.Context, opts *SyncOptions) error {
	if opts == nil || !syncWebhookEnabled(*opts) {
		return nil
	}
	if opts.WebhookAuth.IsGrok() {
		opts.WebhookEvents = SyncWebhookEventSet{SyncWebhookEventMessage: true}
		if strings.TrimSpace(opts.WebhookChat) == "" && len(opts.WebhookChatJIDs) == 0 {
			return fmt.Errorf("--webhook-chat is required with --webhook-auth grok")
		}
	}
	if strings.TrimSpace(opts.WebhookChat) == "" {
		return nil
	}
	jids, err := a.ResolveWebhookChatJIDs(ctx, opts.WebhookChat)
	if err != nil {
		return err
	}
	opts.WebhookChatJIDs = jids
	return nil
}

func (a *App) ResolveWebhookChatJIDs(ctx context.Context, chat string) ([]string, error) {
	chat = strings.TrimSpace(chat)
	if chat == "" {
		return nil, nil
	}
	jid, err := wa.ParseUserOrJID(chat)
	if err != nil {
		return nil, err
	}
	jid = canonicalJID(jid)
	jids := []types.JID{jid}
	if a.wa != nil {
		switch jid.Server {
		case types.DefaultUserServer:
			jids = append(jids, canonicalJID(a.wa.ResolvePNToLID(ctx, jid)))
		case types.HiddenUserServer:
			jids = append(jids, canonicalJID(a.wa.ResolveLIDToPN(ctx, jid)))
		}
	}
	out := make([]string, 0, len(jids))
	seen := make(map[string]struct{}, len(jids))
	for _, item := range jids {
		if item.IsEmpty() {
			continue
		}
		s := item.String()
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out, nil
}

func (a *App) shouldEnqueueWebhookMessage(ctx context.Context, opts SyncOptions, pm wa.ParsedMessage) bool {
	if strings.TrimSpace(pm.ID) == "" {
		return false
	}
	if opts.WebhookAuth.IsGrok() && !opts.WebhookIncludeFromMe && pm.FromMe {
		return false
	}
	if !a.webhookChatAllowed(ctx, opts, pm) {
		return false
	}
	if opts.WebhookFilter != nil && !opts.WebhookFilter.MatchString(webhookFilterHaystack(pm)) {
		return false
	}
	return true
}

func (a *App) webhookChatAllowed(ctx context.Context, opts SyncOptions, pm wa.ParsedMessage) bool {
	if len(opts.WebhookChatJIDs) == 0 {
		return true
	}
	candidates := []string{
		canonicalJIDString(pm.Chat),
		canonicalJIDString(a.canonicalWebhookJID(ctx, pm.Chat)),
	}
	for _, candidate := range candidates {
		for _, want := range opts.WebhookChatJIDs {
			if candidate != "" && candidate == want {
				return true
			}
		}
	}
	return false
}

func webhookFilterHaystack(pm wa.ParsedMessage) string {
	parts := []string{pm.Text}
	if pm.Media != nil {
		parts = append(parts, pm.Media.Caption, pm.Media.Filename)
	}
	return strings.Join(parts, "\n")
}

func (a *App) liveMessageExists(ctx context.Context, pm wa.ParsedMessage) bool {
	if a == nil || a.db == nil || strings.TrimSpace(pm.ID) == "" || pm.Chat.IsEmpty() {
		return false
	}
	chat := canonicalJIDString(a.canonicalStoreJID(ctx, pm.Chat))
	if chat == "" {
		return false
	}
	_, err := a.db.GetMessage(chat, pm.ID)
	return err == nil
}

func (a *App) newGrokWebhookPayload(ctx context.Context, pm wa.ParsedMessage) grokWebhookPayload {
	pm = a.canonicalWebhookMessage(ctx, pm)
	payload := grokWebhookPayload{
		ID:     pm.ID,
		Chat:   canonicalJIDString(pm.Chat),
		Sender: strings.TrimSpace(pm.SenderJID),
		TS:     pm.Timestamp.UTC().Format(time.RFC3339),
		FromMe: pm.FromMe,
		Text:   grokWebhookText(pm),
	}
	if pm.FromMe {
		payload.SenderName = "me"
	} else if name := strings.TrimSpace(pm.PushName); name != "" && name != "-" {
		payload.SenderName = name
	}
	if payload.Chat != "" && a.db != nil {
		if chat, err := a.db.GetChat(payload.Chat); err == nil {
			payload.ChatName = strings.TrimSpace(chat.Name)
		}
	}
	if pm.Media != nil {
		payload.MediaType = strings.TrimSpace(pm.Media.Type)
		payload.MediaFilename = strings.TrimSpace(pm.Media.Filename)
		payload.MediaMIME = strings.TrimSpace(pm.Media.MimeType)
	}
	return payload
}

func grokWebhookText(pm wa.ParsedMessage) string {
	if text := strings.TrimSpace(pm.Text); text != "" {
		return text
	}
	if pm.Media == nil {
		return ""
	}
	if caption := strings.TrimSpace(pm.Media.Caption); caption != "" {
		return caption
	}
	return strings.TrimSpace(pm.Media.Filename)
}

func fitGrokWebhookJSON(p grokWebhookPayload) ([]byte, error) {
	data, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("marshal webhook payload: %w", err)
	}
	if len(data) <= grokWebhookJSONBudget {
		return data, nil
	}

	runes := []rune(p.Text)
	p.TextTruncated = true
	lo, hi := 0, len(runes)
	var best []byte
	for lo <= hi {
		mid := (lo + hi) / 2
		p.Text = string(runes[:mid])
		data, err = json.Marshal(p)
		if err != nil {
			return nil, fmt.Errorf("marshal webhook payload: %w", err)
		}
		if len(data) <= grokWebhookJSONBudget {
			best = data
			lo = mid + 1
			continue
		}
		hi = mid - 1
	}
	if best != nil {
		return best, nil
	}

	p.Text = ""
	data, err = json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("marshal webhook payload: %w", err)
	}
	if len(data) > grokWebhookJSONCeiling {
		return nil, fmt.Errorf("webhook payload exceeds %d bytes", grokWebhookJSONCeiling)
	}
	return data, nil
}

func webhookHTTPSuccess(opts SyncOptions, statusCode int) bool {
	if opts.WebhookAuth.IsGrok() {
		return statusCode == 200
	}
	return statusCode >= 200 && statusCode < 300
}

func (a *App) recordGrokWebhookFailure(opts SyncOptions, payload []byte) {
	if a == nil || !opts.WebhookAuth.IsGrok() || len(payload) == 0 {
		return
	}
	path := filepath.Join(a.StoreDir(), grokWebhookFailedLog)
	if err := appendPrivateFile(path, append(payload, '\n')); err != nil {
		a.emitWarning(
			"sync_webhook_failed_log",
			fmt.Sprintf("warning: failed to write webhook fallback log: %v", err),
			map[string]any{"error": err.Error()},
		)
	}
}

func appendPrivateFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open private file: %w", err)
	}
	chmodErr := f.Chmod(0o600)
	var writeErr error
	if chmodErr == nil {
		n, err := f.Write(data)
		if err == nil && n != len(data) {
			err = errors.New("short write")
		}
		writeErr = err
	}
	closeErr := f.Close()
	if chmodErr != nil {
		return fmt.Errorf("chmod private file: %w", chmodErr)
	}
	if writeErr != nil {
		return fmt.Errorf("write private file: %w", writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close private file: %w", closeErr)
	}
	return nil
}
