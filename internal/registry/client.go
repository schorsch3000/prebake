package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Client talks to the registry HTTP API of the write listener. It is what
// pruning uses to list and untag; it works against any registry that allows
// tag deletion.
type Client struct {
	Base string // e.g. "http://127.0.0.1:5001"
	HTTP *http.Client
}

const manifestAccept = "application/vnd.oci.image.manifest.v1+json," +
	"application/vnd.oci.image.index.v1+json," +
	"application/vnd.docker.distribution.manifest.v2+json," +
	"application/vnd.docker.distribution.manifest.list.v2+json"

func (c *Client) do(ctx context.Context, method, path string, hdr map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.Base, "/")+path, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	h := c.HTTP
	if h == nil {
		h = http.DefaultClient
	}
	return h.Do(req)
}

func (c *Client) getJSON(ctx context.Context, path string, v any) error {
	resp, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("GET %s: %s: %s", path, resp.Status, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// Repositories lists all repositories, following the registry's pagination.
func (c *Client) Repositories(ctx context.Context) ([]string, error) {
	var all []string
	path := "/v2/_catalog?n=100"
	for path != "" {
		resp, err := c.do(ctx, http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}
		var body struct{ Repositories []string }
		err = func() error {
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("GET %s: %s", path, resp.Status)
			}
			return json.NewDecoder(resp.Body).Decode(&body)
		}()
		if err != nil {
			return nil, err
		}
		all = append(all, body.Repositories...)
		path = nextLink(resp.Header.Get("Link"))
	}
	return all, nil
}

// nextLink extracts the target of a `<url>; rel="next"` Link header.
func nextLink(h string) string {
	if h == "" || !strings.Contains(h, `rel="next"`) {
		return ""
	}
	start, end := strings.Index(h, "<"), strings.Index(h, ">")
	if start < 0 || end < start {
		return ""
	}
	return h[start+1 : end]
}

// Tags lists the tags of a repository; a repository without tags yields none.
func (c *Client) Tags(ctx context.Context, repo string) ([]string, error) {
	resp, err := c.do(ctx, http.MethodGet, "/v2/"+repo+"/tags/list", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tags of %s: %s", repo, resp.Status)
	}
	var body struct{ Tags []string }
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	return body.Tags, nil
}

// Digest returns the manifest digest a tag points to.
func (c *Client) Digest(ctx context.Context, repo, tag string) (string, error) {
	resp, err := c.do(ctx, http.MethodHead, "/v2/"+repo+"/manifests/"+url.PathEscape(tag), map[string]string{"Accept": manifestAccept})
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("digest of %s:%s: %s", repo, tag, resp.Status)
	}
	d := resp.Header.Get("Docker-Content-Digest")
	if d == "" {
		return "", fmt.Errorf("digest of %s:%s: registry sent no Docker-Content-Digest", repo, tag)
	}
	return d, nil
}

// Untag removes a tag. The manifest stays until GC removes it (when nothing
// else references it), so tags sharing one digest cannot take each other down.
func (c *Client) Untag(ctx context.Context, repo, tag string) error {
	resp, err := c.do(ctx, http.MethodDelete, "/v2/"+repo+"/manifests/"+url.PathEscape(tag), nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("untag %s:%s: %s", repo, tag, resp.Status)
	}
	return nil
}
