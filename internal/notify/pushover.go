package notify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Pushover limits (https://pushover.net/api).
const (
	maxTitle   = 250
	maxMessage = 1024
	// PushoverURL is the message API endpoint.
	PushoverURL = "https://api.pushover.net/1/messages.json"
)

// Pushover delivers events through the Pushover API.
type Pushover struct {
	Token, User string
	URL         string       // default PushoverURL; tests point it at a local server
	HTTP        *http.Client // default: 10s timeout
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func (p *Pushover) Notify(ctx context.Context, e Event) error {
	endpoint := p.URL
	if endpoint == "" {
		endpoint = PushoverURL
	}
	c := p.HTTP
	if c == nil {
		c = &http.Client{Timeout: 10 * time.Second}
	}
	msg := e.Message
	if msg == "" {
		msg = e.Title // Pushover rejects an empty message
	}
	form := url.Values{
		"token":    {p.Token},
		"user":     {p.User},
		"title":    {"prebake: " + clip(e.Title, maxTitle-len("prebake: "))},
		"message":  {clip(msg, maxMessage)},
		"priority": {strconv.Itoa(e.Priority)},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if resp.StatusCode != http.StatusOK {
		// the body names the problem, e.g. {"user":"invalid","status":0}; it never echoes the token
		return fmt.Errorf("pushover: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

// FromEnv builds the Pushover notifier from PUSHOVER_TOKEN and PUSHOVER_USER;
// either may instead be given as <NAME>_FILE (Docker secrets). With neither set
// it returns Nop and enabled=false; with only one it is an error.
func FromEnv(getenv func(string) string) (n Notifier, enabled bool, err error) {
	token, err := secret(getenv, "PUSHOVER_TOKEN")
	if err != nil {
		return nil, false, err
	}
	user, err := secret(getenv, "PUSHOVER_USER")
	if err != nil {
		return nil, false, err
	}
	switch {
	case token == "" && user == "":
		return Nop{}, false, nil
	case token == "" || user == "":
		return nil, false, errors.New("PUSHOVER_TOKEN and PUSHOVER_USER must be set together")
	}
	return &Pushover{Token: token, User: user}, true, nil
}

func secret(getenv func(string) string, name string) (string, error) {
	if path := getenv(name + "_FILE"); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("%s_FILE: %w", name, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return strings.TrimSpace(getenv(name)), nil
}
