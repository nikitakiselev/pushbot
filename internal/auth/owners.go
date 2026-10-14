package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/nikitakiselev/pushbot/internal/state"
)

// ErrOwnerNotFound is permanent: retrying or falling back to the cache won't help.
var ErrOwnerNotFound = errors.New("no such GitHub user or organization")

// OwnerResolver looks up numeric account IDs via the GitHub REST API.
type OwnerResolver struct {
	APIBase string       // default https://api.github.com
	Client  *http.Client // default http.DefaultClient with a 10s timeout
	Token   string       // optional; raises the API rate limit
}

// Resolve returns the numeric ID of a user or organization login.
func (r *OwnerResolver) Resolve(ctx context.Context, owner string) (string, error) {
	base := r.APIBase
	if base == "" {
		base = "https://api.github.com"
	}
	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/users/"+url.PathEscape(owner), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if r.Token != "" {
		req.Header.Set("Authorization", "Bearer "+r.Token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("%s: %w", owner, ErrOwnerNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: GitHub API returned %s", owner, resp.Status)
	}
	var body struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.ID == 0 {
		return "", fmt.Errorf("%s: unexpected GitHub API response", owner)
	}
	return strconv.FormatInt(body.ID, 10), nil
}

// OwnerIDs resolves every owner, retrying transient failures until retryFor has
// passed. On success the result is saved to cacheFile; if GitHub stays
// unreachable, cached IDs are used. It fails when an owner does not exist or
// is missing from the cache.
func OwnerIDs(ctx context.Context, r *OwnerResolver, owners []string, cacheFile string, retryFor time.Duration, logf func(string, ...any)) (map[string]string, error) {
	deadline := time.Now().Add(retryFor)
	delay := time.Second
	for {
		ids, err := resolveAll(ctx, r, owners)
		if err == nil {
			if err := state.SaveOwners(cacheFile, ids); err != nil {
				logf("cannot save owner ID cache: %v", err)
			}
			return ids, nil
		}
		if errors.Is(err, ErrOwnerNotFound) {
			return nil, err
		}
		if time.Now().Add(delay).After(deadline) {
			logf("cannot resolve owner IDs, using cache: %v", err)
			return fromCache(cacheFile, owners, err)
		}
		logf("cannot resolve owner IDs, retrying in %s: %v", delay, err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
		delay = min(delay*2, 15*time.Second)
	}
}

func resolveAll(ctx context.Context, r *OwnerResolver, owners []string) (map[string]string, error) {
	ids := map[string]string{}
	for _, o := range owners {
		id, err := r.Resolve(ctx, o)
		if err != nil {
			return nil, err
		}
		ids[o] = id
	}
	return ids, nil
}

func fromCache(cacheFile string, owners []string, cause error) (map[string]string, error) {
	cached, err := state.LoadOwners(cacheFile)
	if err != nil {
		return nil, err
	}
	ids := map[string]string{}
	for _, o := range owners {
		id, ok := cached[o]
		if !ok {
			return nil, fmt.Errorf("owner %s is not cached and GitHub is unreachable: %w", o, cause)
		}
		ids[o] = id
	}
	return ids, nil
}
