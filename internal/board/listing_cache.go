package board

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/datapointchris/prs/internal/pullrequests"
)

// listingCacheFile is one scope's cache on disk: each provider's last good listing, and the
// repos and owners each was read for.
type listingCacheFile struct {
	// Shape names the file's layout. A file whose shape differs from this prs's is ignored,
	// and the next read writes over it.
	Shape    string               `json:"shape"`
	Scope    map[string]keptScope `json:"scope"`
	Listings []keptListing        `json:"listings"`
}

// keptScope is what one provider's read covered: owner/name slugs and owners, lower case and
// sorted, so one scope always reads the same.
type keptScope struct {
	Repos  []string `json:"repos"`
	Owners []string `json:"owners"`
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

// listingCacheShape is a digest of every field's JSON name and Go type in listingCacheFile, a
// pull request's included. A change to a field the file holds changes it, so a file an
// earlier prs wrote is never read with a field missing or retyped.
var listingCacheShape = func() string {
	digest := sha256.New()
	describeShape(digest, reflect.TypeFor[listingCacheFile]())
	return hex.EncodeToString(digest.Sum(nil))[:16]
}()

// describeShape writes t's JSON shape to w. A type that marshals itself, such as time.Time,
// is its name.
func describeShape(w io.Writer, t reflect.Type) {
	if t.Implements(reflect.TypeFor[json.Marshaler]()) {
		_, _ = fmt.Fprint(w, t.String())
		return
	}
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice:
		_, _ = fmt.Fprint(w, t.Kind(), "(")
		describeShape(w, t.Elem())
		_, _ = fmt.Fprint(w, ")")
	case reflect.Map:
		_, _ = fmt.Fprint(w, "map(", t.Key(), ",")
		describeShape(w, t.Elem())
		_, _ = fmt.Fprint(w, ")")
	case reflect.Struct:
		_, _ = fmt.Fprint(w, "{")
		for i := range t.NumField() {
			if field := t.Field(i); field.IsExported() {
				_, _ = fmt.Fprint(w, field.Name, " ", field.Tag.Get("json"), ":")
				describeShape(w, field.Type)
				_, _ = fmt.Fprint(w, ";")
			}
		}
		_, _ = fmt.Fprint(w, "}")
	default:
		_, _ = fmt.Fprint(w, t.String())
	}
}

// keptListingsFor is how long a scope's file stays without a write before a write for any
// scope deletes it. A registry that gains a repo is a new scope, so each edit leaves the old
// scope's file behind.
const keptListingsFor = 30 * 24 * time.Hour

// listingCache keeps each provider's last good listing on disk, so the next start on the same
// repos draws the rows before any read returns. It costs only a recompute to lose, so it
// lives under $XDG_CACHE_HOME. Each scope has a file of its own, named for a digest of its
// repos and owners, so two boards reading different repos never write over each other, and a
// listing is drawn only for the scope it was read for.
type listingCache struct {
	dir   string
	path  string
	scope map[string]keptScope
	mu    sync.Mutex
	// kept is each provider's newest listing, by provider name: the one the file held, until
	// a read here lands.
	kept map[string]keptListing
	// failed is why the file could not be read at the start or written since, and empty once
	// a write succeeds.
	failed string
	swept  bool
}

// openListingCache reads the cache in dir for the scopes the board reads. A file that is
// missing, or has another shape, draws nothing until the first read lands.
func openListingCache(dir string, scopes map[string]pullrequests.Scope) *listingCache {
	scope := map[string]keptScope{}
	for provider, read := range scopes {
		scope[provider] = keptScopeOf(read)
	}
	raw, _ := json.Marshal(scope)
	digest := sha256.Sum256(raw)
	c := &listingCache{
		dir: dir, path: filepath.Join(dir, hex.EncodeToString(digest[:])[:16]+".json"),
		scope: scope, kept: map[string]keptListing{},
	}
	raw, err := os.ReadFile(c.path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			c.failed = "could not read the rows the last run kept: " + err.Error()
		}
		return c
	}
	var doc listingCacheFile
	if json.Unmarshal(raw, &doc) != nil || doc.Shape != listingCacheShape || !reflect.DeepEqual(doc.Scope, scope) {
		return c
	}
	for _, listing := range doc.Listings {
		c.kept[listing.Provider] = listing
	}
	return c
}

func keptScopeOf(scope pullrequests.Scope) keptScope {
	repos, owners := scope.Names()
	return keptScope{Repos: repos, Owners: owners}
}

// listing is the provider's newest kept listing.
func (c *listingCache) listing(provider string) (keptListing, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	listing, ok := c.kept[provider]
	return listing, ok
}

// fault is why the file could not be read at the start or written since, or empty.
func (c *listingCache) fault() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.failed
}

// keep writes the provider's listing, read at readAt, beside every other provider's. Two
// providers' reads land at once, so the file is written whole under the lock, and renamed
// into place, so a start reading it never sees half a file. The first write of a run deletes
// each other scope's file not written within keptListingsFor.
func (c *listingCache) keep(provider string, listing pullrequests.Listing, readAt time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.kept[provider] = keptListing{Provider: provider, ReadAt: readAt, Notes: listing.Notes, PullRequests: listing.PullRequests}
	doc := listingCacheFile{Shape: listingCacheShape, Scope: c.scope}
	for _, name := range slices.Sorted(maps.Keys(c.kept)) {
		doc.Listings = append(doc.Listings, c.kept[name])
	}
	c.failed = ""
	if err := writeWhole(c.path, doc); err != nil {
		c.failed = "could not keep these rows for the next start: " + err.Error()
		return
	}
	if !c.swept {
		c.swept = true
		sweepListings(c.dir, c.path, readAt)
	}
}

// sweepListings deletes each scope's file in dir but keep that has had no write within
// keptListingsFor of now. A file it cannot read or delete is left, since a later run can.
func sweepListings(dir, keep string, now time.Time) {
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	for _, file := range files {
		info, err := os.Stat(file)
		if err == nil && file != keep && now.Sub(info.ModTime()) > keptListingsFor {
			_ = os.Remove(file)
		}
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
