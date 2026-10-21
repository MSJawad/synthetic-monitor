package alert

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Notifier delivers notifications.
type Notifier interface {
	// Notify delivers n. An error wrapped with Permanent means retrying
	// cannot succeed; any other error is retried.
	Notify(ctx context.Context, n Notification) error
}

type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// Permanent marks err as a delivery failure that retrying cannot fix, such
// as a rejected credential.
func Permanent(err error) error { return permanentError{err} }

// IsPermanent reports whether err was marked with Permanent.
func IsPermanent(err error) bool {
	var p permanentError
	return errors.As(err, &p)
}

// LogNotifier logs notifications instead of sending them, for running
// without a notification service.
type LogNotifier struct {
	Logger *slog.Logger
}

// Notify logs n.
func (l LogNotifier) Notify(ctx context.Context, n Notification) error {
	title, body, _, _ := render(n)
	level := slog.LevelInfo
	if n.Kind == KindAlert {
		level = slog.LevelWarn
	}
	l.Logger.Log(ctx, level, "notification", "kind", n.Kind, "check", n.Check, "title", title, "body", body)
	return nil
}

// Ntfy publishes notifications to an ntfy topic (https://ntfy.sh). On the
// public ntfy.sh server anyone who knows a topic's name can read it, so the
// name should be long and random, and kept secret.
type Ntfy struct {
	topicURL string
	token    string
	client   *http.Client
}

// NewNtfy returns a notifier publishing to topicURL, such as
// https://ntfy.sh/my-secret-topic. token, if set, is sent as a bearer
// token, for servers or topics that require one.
func NewNtfy(topicURL, token string) (*Ntfy, error) {
	u, err := url.Parse(topicURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || strings.Trim(u.Path, "/") == "" {
		return nil, errors.New("ntfy URL must be an http or https URL ending in a topic, such as https://ntfy.sh/my-topic")
	}
	return &Ntfy{topicURL: topicURL, token: token, client: &http.Client{Timeout: 30 * time.Second}}, nil
}

// Notify publishes n.
func (nt *Ntfy) Notify(ctx context.Context, n Notification) error {
	title, body, priority, tags := render(n)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, nt.topicURL, strings.NewReader(body))
	if err != nil {
		return Permanent(err)
	}
	req.Header.Set("Title", title)
	req.Header.Set("Priority", priority)
	req.Header.Set("Tags", tags)
	if nt.token != "" {
		req.Header.Set("Authorization", "Bearer "+nt.token)
	}
	resp, err := nt.client.Do(req)
	if err != nil {
		return fmt.Errorf("ntfy: %w", err)
	}
	defer resp.Body.Close()
	detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode <= 299:
		return nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return fmt.Errorf("ntfy: %s: %s", resp.Status, strings.TrimSpace(string(detail)))
	default:
		// Other client errors (a bad token, a malformed topic) will fail
		// the same way every time.
		return Permanent(fmt.Errorf("ntfy rejected the notification: %s: %s", resp.Status, strings.TrimSpace(string(detail))))
	}
}

// render returns a notification's title, body, ntfy priority and ntfy tags,
// which ntfy shows as emoji.
func render(n Notification) (title, body, priority, tags string) {
	switch n.Kind {
	case KindAlert:
		return n.Check + " is failing",
			fmt.Sprintf("%d probes in a row have failed, the first at %s.\nLatest failure (%s): %s",
				n.Probes, stamp(n.Since), n.Reason, n.Error),
			"high", "rotating_light"
	case KindRecovery:
		return n.Check + " recovered",
			fmt.Sprintf("Succeeding again since %s, after %s of failures that began at %s.",
				stamp(n.Until), n.Until.Sub(n.Since).Round(time.Second), stamp(n.Since)),
			"default", "white_check_mark"
	case KindRemoved:
		return n.Check + " is no longer monitored",
			fmt.Sprintf("It was removed from the config while failing (since %s), so no recovery notice will follow.",
				stamp(n.Since)),
			"default", "wastebasket"
	}
	return n.Check + ": " + string(n.Kind), "", "default", ""
}

func stamp(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05 MST")
}
