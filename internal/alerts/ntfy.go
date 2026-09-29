package alerts

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Ntfy posts to an ntfy topic URL (https://ntfy.sh/<topic>), which pushes
// to the ntfy phone app. Same approach as go-sentinel's alerts.
type Ntfy struct {
	URL   string
	Click string // opened when the notification is tapped (the dashboard)
}

var client = &http.Client{Timeout: 10 * time.Second}

func (n Ntfy) Notify(ctx context.Context, m Message) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.URL, strings.NewReader(m.Body))
	if err != nil {
		return err
	}
	// Metadata goes in headers. Header values should be ASCII, so emoji
	// travel as Tags shortcodes, which ntfy renders as emoji.
	req.Header.Set("Title", m.Title)
	if m.Priority != "" {
		req.Header.Set("Priority", m.Priority)
	}
	if m.Tags != "" {
		req.Header.Set("Tags", m.Tags)
	}
	if n.Click != "" {
		req.Header.Set("Click", n.Click)
	}

	resp, err := client.Do(req)
	if err != nil {
		// The error text contains the URL, which is a secret: keep only the cause.
		if ue, ok := errors.AsType[*url.Error](err); ok {
			err = ue.Err
		}
		return fmt.Errorf("ntfy: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return fmt.Errorf("ntfy: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	return nil
}
