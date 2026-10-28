// Package legacyimport imports the metadata of the pre-SQLite PoC
// (.system/state.json) into the SQLite database .system/withalbum.db without
// losing anything, and refuses to do so when the input is inconsistent.
//
// Run in dry-run mode only reads: it parses and validates state.json, re-hashes
// every original in .system/pool and checks the previews, then reports. In
// apply mode it additionally
//
//  1. copies state.json to state.json.bak-<UTC> (fsynced),
//  2. builds .system/withalbum.db.importing: schema and all rows in one
//     transaction, including a legacy_imports row with the SHA-256 of the
//     state.json bytes,
//  3. reopens that file and compares every row with state.json,
//  4. renames it to withalbum.db (directory fsynced), and
//  5. renames state.json to state.json.migrated-<UTC>.
//
// state.json, its backup and the pool/preview files are never modified or
// deleted. A crash at any point is recovered by running apply again: before
// step 4 the temporary file is discarded and the import starts over; after
// step 4 the database already records the import and only step 5 is redone.
package legacyimport

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

// The PoC state.json format (server/main.go up to M1, internal/meta/jsonstore).
type legacyFolder struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
}

type legacyAsset struct {
	ID           string `json:"id"`
	Filename     string `json:"filename"`
	MimeType     string `json:"mime_type"`
	Bytes        int64  `json:"bytes"`
	PreviewReady bool   `json:"preview_ready"`
	UploadedAt   string `json:"uploaded_at"`
}

type legacyLink struct {
	AssetID          string `json:"asset_id"`
	UploaderDeviceID string `json:"uploader_device_id"`
	AddedAt          string `json:"added_at"`
}

type legacyEvent struct {
	Seq              int64  `json:"seq"`
	Kind             string `json:"kind"`
	AssetID          string `json:"asset_id"`
	FolderID         string `json:"folder_id"`
	UploaderDeviceID string `json:"uploader_device_id"`
	OccurredAt       string `json:"occurred_at"`
}

// legacyState uses pointers so that a missing top-level key is detected
// instead of being read as an empty map.
type legacyState struct {
	Folders *map[string]legacyFolder          `json:"folders"`
	Assets  *map[string]legacyAsset           `json:"assets"`
	Links   *map[string]map[string]legacyLink `json:"links"`
	Events  *map[string][]legacyEvent         `json:"events"`
	NextSeq *map[string]int64                 `json:"next_seq"`
}

// state is a parsed state.json.
type state struct {
	Folders map[string]legacyFolder
	Assets  map[string]legacyAsset
	Links   map[string]map[string]legacyLink
	Events  map[string][]legacyEvent
	NextSeq map[string]int64
}

// The only event kind the PoC ever wrote.
const eventAssetAdded = "asset_added"

var shaPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func isSHA(s string) bool { return shaPattern.MatchString(s) }

// parseState decodes state.json strictly: no unknown fields, no duplicate
// object keys, no trailing data and all five top-level maps present. Any of
// those would mean data the import cannot carry over faithfully.
func parseState(b []byte) (*state, error) {
	if err := checkDuplicateKeys(b); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var ls legacyState
	if err := dec.Decode(&ls); err != nil {
		return nil, fmt.Errorf("parse state.json: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("parse state.json: unexpected data after the JSON object")
	}
	var missing []string
	if ls.Folders == nil {
		missing = append(missing, "folders")
	}
	if ls.Assets == nil {
		missing = append(missing, "assets")
	}
	if ls.Links == nil {
		missing = append(missing, "links")
	}
	if ls.Events == nil {
		missing = append(missing, "events")
	}
	if ls.NextSeq == nil {
		missing = append(missing, "next_seq")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("parse state.json: missing or null top-level keys: %s (incomplete file?)", strings.Join(missing, ", "))
	}
	return &state{Folders: *ls.Folders, Assets: *ls.Assets, Links: *ls.Links, Events: *ls.Events, NextSeq: *ls.NextSeq}, nil
}

// checkDuplicateKeys walks the JSON tokens and rejects any object with a
// repeated key (encoding/json would silently keep the last one).
func checkDuplicateKeys(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var walk func(path string) error
	walk = func(path string) error {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		switch tok {
		case json.Delim('{'):
			seen := map[string]bool{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return err
				}
				key, _ := kt.(string)
				if seen[key] {
					return fmt.Errorf("duplicate key %q in %s", key, path)
				}
				seen[key] = true
				if err := walk(path + "." + key); err != nil {
					return err
				}
			}
			_, err = dec.Token() // '}'
			return err
		case json.Delim('['):
			for i := 0; dec.More(); i++ {
				if err := walk(fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
			_, err = dec.Token() // ']'
			return err
		}
		return nil
	}
	if err := walk("$"); err != nil {
		return fmt.Errorf("parse state.json: %w", err)
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// validate checks the internal consistency of a parsed state. It returns
// problems (the import must not proceed) and warnings (imported anyway,
// nothing is lost).
func (s *state) validate() (problems, warnings []string) {
	bad := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	for _, id := range sortedKeys(s.Folders) {
		f := s.Folders[id]
		if id == "" || f.ID != id {
			bad("folder %q: id field %q does not match its key", id, f.ID)
		}
		if f.Name == "" {
			bad("folder %q: empty name", id)
		}
		if f.CreatedAt == "" {
			bad("folder %q: empty created_at", id)
		}
	}
	for _, sha := range sortedKeys(s.Assets) {
		a := s.Assets[sha]
		if !isSHA(sha) {
			bad("asset %q: key is not a lowercase SHA-256 hex string", sha)
		}
		if a.ID != sha {
			bad("asset %q: id field %q does not match its key", sha, a.ID)
		}
		if a.Bytes <= 0 {
			bad("asset %s: bytes %d is not positive", sha, a.Bytes)
		}
		if a.MimeType == "" {
			bad("asset %s: empty mime_type", sha)
		}
		if a.Filename == "" {
			bad("asset %s: empty filename", sha)
		}
		if a.UploadedAt == "" {
			bad("asset %s: empty uploaded_at", sha)
		}
	}
	linked := map[string]bool{}
	for _, fid := range sortedKeys(s.Links) {
		if _, ok := s.Folders[fid]; !ok {
			bad("links: unknown folder %q", fid)
		}
		for _, sha := range sortedKeys(s.Links[fid]) {
			l := s.Links[fid][sha]
			if l.AssetID != sha {
				bad("link %s/%s: asset_id field %q does not match its key", fid, sha, l.AssetID)
			}
			if _, ok := s.Assets[sha]; !ok {
				bad("link %s/%s: unknown asset", fid, sha)
			}
			if l.AddedAt == "" {
				bad("link %s/%s: empty added_at", fid, sha)
			}
			linked[sha] = true
		}
	}
	evented := map[string]map[string]bool{} // folder -> assets with an event
	for _, fid := range sortedKeys(s.Events) {
		if _, ok := s.Folders[fid]; !ok {
			bad("events: unknown folder %q", fid)
		}
		seen := map[string]bool{}
		evented[fid] = seen
		for i, e := range s.Events[fid] {
			if want := int64(i + 1); e.Seq != want {
				bad("folder %s: event #%d has seq %d, want %d (seq must be 1..n without gaps)", fid, i+1, e.Seq, want)
			}
			if e.Kind != eventAssetAdded {
				bad("folder %s event seq %d: unsupported kind %q", fid, e.Seq, e.Kind)
			}
			if e.FolderID != fid {
				bad("folder %s event seq %d: folder_id field %q does not match", fid, e.Seq, e.FolderID)
			}
			if _, ok := s.Assets[e.AssetID]; !ok {
				bad("folder %s event seq %d: unknown asset %q", fid, e.Seq, e.AssetID)
			}
			if _, ok := s.Links[fid][e.AssetID]; !ok {
				bad("folder %s event seq %d: asset %q is not linked to the folder", fid, e.Seq, e.AssetID)
			}
			if seen[e.AssetID] {
				bad("folder %s event seq %d: second asset_added event for asset %q", fid, e.Seq, e.AssetID)
			}
			seen[e.AssetID] = true
			if e.OccurredAt == "" {
				bad("folder %s event seq %d: empty occurred_at", fid, e.Seq)
			}
		}
	}
	for _, fid := range sortedKeys(s.Folders) {
		for _, sha := range sortedKeys(s.Links[fid]) {
			if !evented[fid][sha] {
				bad("link %s/%s: no asset_added event", fid, sha)
			}
		}
		if n := int64(len(s.Events[fid])); s.NextSeq[fid] != n {
			bad("folder %s: next_seq %d does not match %d events", fid, s.NextSeq[fid], n)
		}
	}
	for _, fid := range sortedKeys(s.NextSeq) {
		if _, ok := s.Folders[fid]; !ok {
			bad("next_seq: unknown folder %q", fid)
		}
	}
	for _, sha := range sortedKeys(s.Assets) {
		if !linked[sha] {
			warnings = append(warnings, fmt.Sprintf("asset %s is not linked to any folder; it is imported unlinked", sha))
		}
	}
	return problems, warnings
}

func (s *state) linkCount() int {
	n := 0
	for _, l := range s.Links {
		n += len(l)
	}
	return n
}

func (s *state) eventCount() int {
	n := 0
	for _, e := range s.Events {
		n += len(e)
	}
	return n
}
