// Package watchdog tells an outside service that the monitor is alive, so
// that service can alert when it stops hearing from it. A monitor cannot
// report its own death: if the node it runs on fails, so does every alert
// it would have sent.
package watchdog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"
)

// Options configures Run.
type Options struct {
	// URL is requested on every interval. It usually identifies the
	// account, so it is treated as a secret and never logged.
	URL      string
	Interval time.Duration
	Client   *http.Client
	Logger   *slog.Logger
	// OnPing, if not nil, is told whether each ping succeeded.
	OnPing func(ok bool)
}

// Validate reports whether url can be pinged, without quoting it.
func Validate(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("must be an absolute http or https URL")
	}
	return nil
}

// Run pings at once and then every interval until ctx is done.
func Run(ctx context.Context, opts Options) {
	ticker := time.NewTicker(opts.Interval)
	defer ticker.Stop()
	for {
		err := ping(ctx, opts)
		if opts.OnPing != nil && ctx.Err() == nil {
			opts.OnPing(err == nil)
		}
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			opts.Logger.Warn("watchdog ping failed", "err", err)
		default:
			opts.Logger.Debug("watchdog ping sent")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func ping(ctx context.Context, opts Options) error {
	ctx, cancel := context.WithTimeout(ctx, min(opts.Interval, 10*time.Second))
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, opts.URL, nil)
	if err != nil {
		return errors.New("building the request failed")
	}
	resp, err := opts.Client.Do(req)
	if err != nil {
		// A *url.Error quotes the URL; keep only what went wrong.
		if urlErr, ok := errors.AsType[*url.Error](err); ok {
			return urlErr.Err
		}
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}
