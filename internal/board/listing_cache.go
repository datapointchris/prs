package board

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/datapointchris/prs/internal/pullrequests"
)

// listingCacheFormat is the file's shape. A file in any other is ignored, and the next read
// writes over it.
const listingCacheFormat = 1

// listingCacheFile is the cache on disk: each provider's last good listing.
type listingCacheFile struct {
	Format   int           `json:"format"`
	Listings []keptListing `json:"listings"`
}

// keptListing is one provider's last good listing, and when it was read. Each pull request
// is a row of `prs list --json`, without stacked_on.
type keptListing struct {
	Provider     string                     `json:"provider"`
	ReadAt       time.Time                  `json:"read_at"`
	Notes        []string                   `json:"notes"`
	PullRequests []pullrequests.PullRequest `json:"pull_requests"`
}

func (k keptListing) listing() pullrequests.Listing {
	return pullrequests.Listing{PullRequests: k.PullRequests, Notes: k.Notes}
}

// listingCache keeps each provider's last good listing on disk, so the next start draws the
// rows before any read returns. It costs only a recompute to lose, so it lives under
// $XDG_CACHE_HOME, and a file that is missing or unreadable draws nothing until the first
// read lands.
type listingCache struct {
	path string
	mu   sync.Mutex
	// kept is each provider's newest listing, by provider name: the one the file held, cut to
	// the repos this run reads, until a read here lands.
	kept map[string]keptListing
	// fault is why the newest write failed, and empty where it wrote.
	fault string
}

// openListingCache reads the cache at path, keeping of each provider's listing only the pull
// requests its scope here covers. A listing read for another registry or other owners would
// otherwise draw repos this run never reads.
func openListingCache(path string, scopes map[string]pullrequests.Scope) *listingCache {
	c := &listingCache{path: path, kept: map[string]keptListing{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			c.fault = err.Error()
		}
		return c
	}
	var doc listingCacheFile
	if json.Unmarshal(raw, &doc) != nil || doc.Format != listingCacheFormat {
		return c
	}
	for _, listing := range doc.Listings {
		scope, read := scopes[listing.Provider]
		if !read {
			continue
		}
		listing.PullRequests = slices.DeleteFunc(listing.PullRequests, func(pr pullrequests.PullRequest) bool {
			return !scope.Covers(pr)
		})
		c.kept[listing.Provider] = listing
	}
	return c
}

// listing is the provider's newest kept listing.
func (c *listingCache) listing(provider string) (keptListing, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	listing, ok := c.kept[provider]
	return listing, ok
}

// writeFault is why the newest write failed, or empty.
func (c *listingCache) writeFault() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fault
}

// keep writes the provider's listing, read at readAt, beside every other provider's. Two
// providers' reads land at once, so the file is written whole under the lock, and renamed
// into place, so a start reading it never sees half a file.
func (c *listingCache) keep(provider string, listing pullrequests.Listing, readAt time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.kept[provider] = keptListing{Provider: provider, ReadAt: readAt, Notes: listing.Notes, PullRequests: listing.PullRequests}
	doc := listingCacheFile{Format: listingCacheFormat}
	for _, name := range slices.Sorted(maps.Keys(c.kept)) {
		doc.Listings = append(doc.Listings, c.kept[name])
	}
	c.fault = ""
	if err := writeWhole(c.path, doc); err != nil {
		c.fault = err.Error()
	}
}

// writeWhole writes doc as JSON to a file beside path, then renames it over path. The file
// holds pull requests' bodies from private repos, so only its owner reads it.
func writeWhole(path string, doc listingCacheFile) error {
	raw, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	_, err = file.Write(raw)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(file.Name(), path)
	}
	if err != nil {
		_ = os.Remove(file.Name())
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}
