package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	appPkg "github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/wa"
	"github.com/spf13/cobra"
)

const (
	envWebhookURL    = "WACLI_WEBHOOK_URL"
	envWebhookAuth   = "WACLI_WEBHOOK_AUTH"
	envWebhookSecret = "WACLI_WEBHOOK_SECRET"
	envWebhookChat   = "WACLI_WEBHOOK_CHAT"
	envWebhookFilter = "WACLI_WEBHOOK_FILTER"
)

func newSyncCmd(flags *rootFlags) *cobra.Command {
	var once bool
	var follow bool
	var idleExit time.Duration
	var maxReconnect time.Duration
	var staleThreshold time.Duration
	var presenceModeFlag string
	var downloadMedia bool
	var refreshContacts bool
	var refreshGroups bool
	var refreshChannels bool
	var webhookURL string
	var webhookSecret string
	var webhookAllowPrivate bool
	var webhookEventsFlag string
	var webhookAuthFlag string
	var webhookChat string
	var webhookFilterFlag string
	var webhookIncludeFromMe bool
	var sendSpacingFlag string
	var storage syncStorageLimitFlags

	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Sync messages (requires prior auth; never shows QR)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := flags.requireWritable(); err != nil {
				return err
			}
			storage.maxMessagesSet = cmd.Flags().Changed("max-messages")
			maxMessages, maxDBSize, err := resolveSyncStorageLimits(storage)
			if err != nil {
				return err
			}
			webhookURL = firstNonEmpty(webhookURL, os.Getenv(envWebhookURL))
			webhookSecret = firstNonEmpty(webhookSecret, os.Getenv(envWebhookSecret))
			webhookChat = firstNonEmpty(webhookChat, os.Getenv(envWebhookChat))
			webhookFilterFlag = firstNonEmpty(webhookFilterFlag, os.Getenv(envWebhookFilter))
			if !cmd.Flags().Changed("webhook-auth") {
				if env := strings.TrimSpace(os.Getenv(envWebhookAuth)); env != "" {
					webhookAuthFlag = env
				}
			}

			if webhookSecret != "" && webhookURL == "" {
				return fmt.Errorf("--webhook-secret requires --webhook")
			}
			if cmd.Flags().Changed("webhook-events") && webhookURL == "" {
				return fmt.Errorf("--webhook-events requires --webhook")
			}
			if (cmd.Flags().Changed("webhook-auth") || strings.TrimSpace(os.Getenv(envWebhookAuth)) != "") && webhookURL == "" {
				return fmt.Errorf("--webhook-auth requires --webhook")
			}
			if webhookChat != "" && webhookURL == "" {
				return fmt.Errorf("--webhook-chat requires --webhook")
			}
			if webhookFilterFlag != "" && webhookURL == "" {
				return fmt.Errorf("--webhook-filter requires --webhook")
			}
			if cmd.Flags().Changed("webhook-include-from-me") && webhookURL == "" {
				return fmt.Errorf("--webhook-include-from-me requires --webhook")
			}
			webhookAuth, err := appPkg.ParseSyncWebhookAuth(webhookAuthFlag)
			if err != nil {
				return err
			}
			webhookEvents, err := appPkg.ParseSyncWebhookEvents(webhookEventsFlag)
			if err != nil {
				return err
			}
			if webhookAuth.IsGrok() {
				if webhookURL == "" {
					return fmt.Errorf("--webhook-auth grok requires --webhook")
				}
				if webhookSecret == "" {
					return fmt.Errorf("--webhook-auth grok requires --webhook-secret")
				}
				if webhookChat == "" {
					return fmt.Errorf("--webhook-chat is required with --webhook-auth grok")
				}
				if cmd.Flags().Changed("webhook-events") && grokWebhookEventsForbidden(webhookEvents) {
					return fmt.Errorf("--webhook-auth grok only posts message events")
				}
				webhookEvents = appPkg.SyncWebhookEventSet{appPkg.SyncWebhookEventMessage: true}
			}
			webhookFilter, err := appPkg.CompileWebhookFilter(webhookFilterFlag)
			if err != nil {
				return err
			}
			if webhookChat != "" {
				if _, err := wa.ParseUserOrJID(webhookChat); err != nil {
					return fmt.Errorf("--webhook-chat: %w", err)
				}
			}
			if staleThreshold != 0 && staleThreshold < time.Second {
				return fmt.Errorf("--stale-threshold must be at least 1s, got %s", staleThreshold)
			}
			sendSpacing, err := parseSendSpacing(sendSpacingFlag)
			if err != nil {
				return err
			}
			if maxStaleThreshold := appPkg.MaxStaleThreshold(); staleThreshold >= maxStaleThreshold {
				return fmt.Errorf("--stale-threshold must be less than %s because whatsmeow auto-reconnects after that much keepalive failure, got %s", maxStaleThreshold, staleThreshold)
			}
			presenceMode, err := appPkg.ParseSyncPresenceMode(presenceModeFlag)
			if err != nil {
				return err
			}
			ctx, stop := signalContextWithEvents(out.NewEventWriter(os.Stderr, flags.events))
			defer stop()

			a, lk, err := newApp(ctx, flags, true, false)
			if err != nil {
				return err
			}
			defer closeApp(a, lk)

			if err := a.EnsureAuthed(); err != nil {
				return err
			}

			mode := appPkg.SyncModeFollow
			if once {
				mode = appPkg.SyncModeOnce
			} else if follow {
				mode = appPkg.SyncModeFollow
			} else {
				mode = appPkg.SyncModeOnce
			}

			var stopSendDelegate func()
			defer func() {
				if stopSendDelegate != nil {
					stopSendDelegate()
				}
			}()
			var afterConnect func(context.Context) error
			if mode == appPkg.SyncModeFollow {
				afterConnect = func(ctx context.Context) error {
					stop, err := startSendDelegateServer(ctx, a, sendSpacing)
					if err != nil {
						return err
					}
					stopSendDelegate = stop
					return nil
				}
			}

			res, err := a.Sync(ctx, appPkg.SyncOptions{
				Mode:                 mode,
				PresenceMode:         presenceMode,
				AllowQR:              false,
				AfterConnect:         afterConnect,
				DownloadMedia:        downloadMedia,
				RefreshContacts:      refreshContacts,
				RefreshGroups:        refreshGroups,
				RefreshChannels:      refreshChannels,
				IdleExit:             idleExit,
				MaxReconnect:         maxReconnect,
				StaleThreshold:       staleThreshold,
				MaxMessages:          maxMessages,
				MaxDBSizeBytes:       maxDBSize,
				WarnNoLimits:         true,
				WebhookURL:           webhookURL,
				WebhookSecret:        webhookSecret,
				WebhookAllowPrivate:  webhookAllowPrivate,
				WebhookEvents:        webhookEvents,
				WebhookAuth:          webhookAuth,
				WebhookChat:          webhookChat,
				WebhookFilter:        webhookFilter,
				WebhookIncludeFromMe: webhookIncludeFromMe,
			})
			if err != nil {
				return err
			}

			if flags.asJSON {
				return out.WriteJSON(os.Stdout, map[string]any{
					"synced":          true,
					"messages_stored": res.MessagesStored,
				})
			}
			fmt.Fprintf(os.Stdout, "Messages stored: %d\n", res.MessagesStored)
			return nil
		},
	}

	cmd.Flags().BoolVar(&once, "once", false, "sync until idle and exit")
	cmd.Flags().BoolVar(&follow, "follow", true, "keep syncing until Ctrl+C")
	cmd.Flags().DurationVar(&idleExit, "idle-exit", 30*time.Second, "exit after being idle (once mode)")
	cmd.Flags().DurationVar(&maxReconnect, "max-reconnect", 5*time.Minute, "give up reconnecting after this duration (0 = unlimited)")
	cmd.Flags().DurationVar(&staleThreshold, "stale-threshold", 0, "force reconnect when keepalive failures last this long in follow mode (1s-<2m20s, 0 = disabled)")
	cmd.Flags().StringVar(&presenceModeFlag, "presence-mode", string(appPkg.SyncPresenceModeNormal), "global sync presence behavior: normal or quiet")
	cmd.Flags().StringVar(&sendSpacingFlag, "send-spacing", "", "pace delegated sends in follow mode by a fixed duration or random min-max range (e.g. 2s or 500ms-5s; default: disabled)")
	cmd.Flags().BoolVar(&downloadMedia, "download-media", false, "download media in the background during sync")
	cmd.Flags().BoolVar(&refreshContacts, "refresh-contacts", false, "refresh contacts from session store into local DB")
	cmd.Flags().BoolVar(&refreshGroups, "refresh-groups", false, "refresh joined groups (live) into local DB")
	cmd.Flags().BoolVar(&refreshChannels, "refresh-channels", false, "refresh subscribed channels (live) into local DB")
	cmd.Flags().StringVar(&webhookURL, "webhook", "", "URL to POST live message JSON (or WACLI_WEBHOOK_URL)")
	cmd.Flags().StringVar(&webhookSecret, "webhook-secret", "", "HMAC secret or Grok Bot sender key; never logged (or WACLI_WEBHOOK_SECRET)")
	cmd.Flags().StringVar(&webhookAuthFlag, "webhook-auth", string(appPkg.SyncWebhookAuthHMAC), "webhook auth mode: hmac or grok (or WACLI_WEBHOOK_AUTH)")
	cmd.Flags().StringVar(&webhookChat, "webhook-chat", "", "only POST messages from this chat JID or phone (required for grok; or WACLI_WEBHOOK_CHAT)")
	cmd.Flags().StringVar(&webhookFilterFlag, "webhook-filter", "", "RE2 regexp matched against text, caption, and filename; omitted matches all (or WACLI_WEBHOOK_FILTER)")
	cmd.Flags().BoolVar(&webhookIncludeFromMe, "webhook-include-from-me", false, "in grok mode, also POST messages sent by this account")
	cmd.Flags().BoolVar(&webhookAllowPrivate, "webhook-allow-private", false, "allow webhook URLs that resolve to localhost or private networks")
	cmd.Flags().StringVar(&webhookEventsFlag, "webhook-events", string(appPkg.SyncWebhookEventMessage), "comma-separated event types to POST: message, receipt, chat_presence")
	cmd.Flags().Int64Var(&storage.maxMessages, "max-messages", 0, "maximum total messages to keep in the local DB before sync stops (0 = unlimited, or WACLI_SYNC_MAX_MESSAGES)")
	cmd.Flags().StringVar(&storage.maxDBSize, "max-db-size", "", "maximum wacli.db disk usage before sync stops, e.g. 500MB or 2GB (default: WACLI_SYNC_MAX_DB_SIZE or unlimited)")
	return cmd
}

func grokWebhookEventsForbidden(set appPkg.SyncWebhookEventSet) bool {
	if !set.Enabled(appPkg.SyncWebhookEventMessage) {
		return true
	}
	return set.Enabled(appPkg.SyncWebhookEventReceipt) || set.Enabled(appPkg.SyncWebhookEventChatPresence)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
