package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// KimiCredentials carries the fields the Kimi datasource upstream requires:
// the bearer token plus the device identifier the upstream associates with
// the logged-in cliproxy session.
type KimiCredentials struct {
	AccessToken string
	DeviceID    string
	Expired     time.Time
}

// kimiAuthFile is the on-disk shape kept by cliproxy's keeper.
// The full file has more fields; we read only what the upstream needs.
type kimiAuthFile struct {
	AccessToken string `json:"access_token"`
	DeviceID    string `json:"device_id"`
	Disabled    bool   `json:"disabled"`
	Expired     string `json:"expired"` // RFC3339 with trailing Z
}

var (
	errNoAuthMatch  = errors.New("no enabled kimi auth file matched the glob")
	errNoAccessTok  = errors.New("kimi auth file has no access_token")
	errAuthFileRead = errors.New("failed to read kimi auth file")
)

// credsCache memoizes the parsed credential per (pattern, file) pair and
// invalidates when the file's mtime changes. cliproxy's keeper rotates the
// file on refresh; mtime is the cheapest reliable invalidation signal.
type credsCache struct {
	mu        sync.Mutex
	pattern   string
	path      string
	mtime     time.Time
	creds     KimiCredentials
	retrieved time.Time
}

var globalCredsCache credsCache

// LoadKimiCredentials reads (mtime-cached) the first enabled kimi auth file
// matched by pattern and returns its credentials. Errors when no enabled,
// parseable file exists or the file lacks an access_token.
func LoadKimiCredentials(pattern string) (KimiCredentials, error) {
	globalCredsCache.mu.Lock()
	defer globalCredsCache.mu.Unlock()
	return loadLocked(pattern, false)
}

// ReloadKimiCredentials ignores the cache and always re-reads. Used when the
// upstream returns 401 (cliproxy may have just refreshed the file).
func ReloadKimiCredentials(pattern string) (KimiCredentials, error) {
	globalCredsCache.mu.Lock()
	defer globalCredsCache.mu.Unlock()
	return loadLocked(pattern, true)
}

// loadLocked must be called with globalCredsCache.mu held.
func loadLocked(pattern string, force bool) (KimiCredentials, error) {
	path, err := firstEnabledAuthFile(pattern)
	if err != nil {
		return KimiCredentials{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return KimiCredentials{}, fmt.Errorf("%w: %v", errAuthFileRead, err)
	}

	cacheHit := !force &&
		globalCredsCache.pattern == pattern &&
		globalCredsCache.path == path &&
		globalCredsCache.mtime.Equal(info.ModTime()) &&
		globalCredsCache.creds.AccessToken != ""
	if cacheHit {
		return globalCredsCache.creds, nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return KimiCredentials{}, fmt.Errorf("%w: %v", errAuthFileRead, err)
	}
	var f kimiAuthFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return KimiCredentials{}, fmt.Errorf("%w: %v", errAuthFileRead, err)
	}
	if f.AccessToken == "" {
		return KimiCredentials{}, errNoAccessTok
	}
	creds := KimiCredentials{
		AccessToken: f.AccessToken,
		DeviceID:    f.DeviceID,
		Expired:     parseExpired(f.Expired),
	}

	globalCredsCache.pattern = pattern
	globalCredsCache.path = path
	globalCredsCache.mtime = info.ModTime()
	globalCredsCache.creds = creds
	globalCredsCache.retrieved = time.Now()
	return creds, nil
}

// firstEnabledAuthFile returns the path of the highest-priority matching
// file that is a) an existing regular file, b) parses as JSON, c) does not
// have disabled:true. Errors only when no candidate qualifies.
func firstEnabledAuthFile(pattern string) (string, error) {
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return "", fmt.Errorf("%w: bad pattern %q: %v", errNoAuthMatch, pattern, err)
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("%w: %q matched no files", errNoAuthMatch, pattern)
	}
	for _, p := range matches {
		info, err := os.Stat(p)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var f kimiAuthFile
		if err := json.Unmarshal(raw, &f); err != nil {
			continue
		}
		if f.Disabled {
			continue
		}
		return p, nil
	}
	return "", fmt.Errorf("%w: %q matched no enabled files", errNoAuthMatch, pattern)
}

// parseExpired parses RFC3339 / RFC3339Nano; zero time on failure.
func parseExpired(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// resetCredsCacheForTest clears the credential cache; used by unit tests only.
func resetCredsCacheForTest() {
	globalCredsCache.mu.Lock()
	defer globalCredsCache.mu.Unlock()
	globalCredsCache.pattern = ""
	globalCredsCache.path = ""
	globalCredsCache.mtime = time.Time{}
	globalCredsCache.creds = KimiCredentials{}
	globalCredsCache.retrieved = time.Time{}
}
