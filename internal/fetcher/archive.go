package fetcher

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	iaSearchURL  = "https://archive.org/advancedsearch.php"
	iaMetaURL    = "https://archive.org/metadata"
	iaDownloadURL = "https://archive.org/download"
	userAgent    = "ghost-radigo/0.1 (archival radio stream)"
	pageSize     = 50
	maxPageStart = 10000
)

// preferredFormats lists formats in descending order of preference.
// Audio formats are tried first. Video formats (MPEG4, h.264) are included
// at lower priority so the processor can extract audio via ffmpeg -vn from
// movie items such as Flash cartoons that have no separate audio track.
var preferredFormats = []string{
	"VBR MP3",
	"128Kbps MP3",
	"64Kbps MP3",
	"MP3",
	"Ogg Vorbis",
	// Video containers — ffmpeg extracts audio with -vn
	"512Kb MPEG4",
	"MPEG4",
	"h.264",
}

// searchQueries rotates across collections and subjects.
// Focused on spoken-word content: voice, dialogue, narration, comedy, news.
// Music-heavy collections (78rpm, etree) are excluded in favour of speech.
//
// Flash cartoon collections (Strongbad, Homestar Runner) are included via
// mediatype:movies queries; video files are handled by the processor's ffmpeg
// pipeline which discards the video track and keeps the audio.
//
// The base query in search() appends NOT access-restricted:1 to every entry.
var searchQueries = []string{
	// Spoken-word audio collections
	// NOTE: collection:radio is intentionally excluded — it contains modern
	// 1-hour broadcast recordings (BBC, BFBS, NPR) whose large file sizes
	// cause ffmpeg range-seek timeouts that starve the clip pool.
	`collection:oldtimeradio`,
	`collection:librivox`,
	`collection:opensource_audio`,
	`collection:audio_bookspoetry`,

	// Speech-forward subject searches
	`mediatype:audio subject:"spoken word"`,
	`mediatype:audio subject:"field recording"`,
	`mediatype:audio subject:speech`,
	`mediatype:audio subject:lecture`,
	`mediatype:audio subject:interview`,
	`mediatype:audio subject:comedy year:[1930 TO 1980]`,
	`mediatype:audio subject:news year:[1930 TO 1980]`,
	`mediatype:audio subject:gospel year:[1930 TO 1970]`,
	`mediatype:audio subject:drama year:[1930 TO 1970]`,

	// Flash cartoons and animation — dialogue-rich video content
	// SWF originals are mediatype:software and unplayable; target movies
	// derivatives which have MP4 files ffmpeg can decode.
	`mediatype:movies subject:"strong bad"`,
	`mediatype:movies subject:"homestar runner"`,
	`mediatype:movies subject:"flash cartoon"`,
	`mediatype:movies subject:"flash animation"`,
	`collection:softwarelibrary_flash_animation mediatype:movies`,
}

// searchDoc is a single result from the Archive.org search API.
type searchDoc struct {
	Identifier string          `json:"identifier"`
	Title      string          `json:"title"`
	Year       json.RawMessage `json:"year"`   // string, int, or absent
	MediaType  string          `json:"mediatype"`
	Format     json.RawMessage `json:"format"` // string or []string
}

// iaFile is a single file entry from the Archive.org metadata API.
type iaFile struct {
	Name   string `json:"name"`
	Format string `json:"format"`
	Length string `json:"length"` // duration in seconds; may be empty or "0"
}

// search queries the Archive.org full-text search API and returns docs that
// have at least one preferred audio format.
func (f *Fetcher) search(ctx context.Context, query string, start int) ([]searchDoc, error) {
	q := query + ` AND format:(MP3 OR "Ogg Vorbis") AND NOT access-restricted:1`

	params := url.Values{}
	params.Set("q", q)
	params.Set("output", "json")
	params.Set("rows", strconv.Itoa(pageSize))
	params.Set("start", strconv.Itoa(start))
	params.Add("fl[]", "identifier")
	params.Add("fl[]", "title")
	params.Add("fl[]", "year")
	params.Add("fl[]", "mediatype")
	params.Add("fl[]", "format")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, iaSearchURL+"?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		sleepCtx(ctx, rateLimitDelay)
		return nil, fmt.Errorf("rate limited (429)")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	var result struct {
		Response struct {
			Docs []searchDoc `json:"docs"`
		} `json:"response"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode search response: %w", err)
	}

	// Secondary client-side filter: confirm a preferred format is present.
	out := result.Response.Docs[:0]
	for _, doc := range result.Response.Docs {
		if docHasPreferredFormat(doc.Format) {
			out = append(out, doc)
		}
	}
	return out, nil
}

// resolveItem fetches item metadata from Archive.org, picks the best audio
// file, and returns a populated MediaItem.
func (f *Fetcher) resolveItem(ctx context.Context, doc searchDoc) (*MediaItem, error) {
	u := fmt.Sprintf("%s/%s", iaMetaURL, url.PathEscape(doc.Identifier))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		sleepCtx(ctx, rateLimitDelay)
		return nil, fmt.Errorf("rate limited (429)")
	}
	if resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("access restricted")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("metadata HTTP %d", resp.StatusCode)
	}

	var meta struct {
		Metadata struct {
			Title     string          `json:"title"`
			Year      json.RawMessage `json:"year"`
			MediaType string          `json:"mediatype"`
		} `json:"metadata"`
		Files []iaFile `json:"files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return nil, fmt.Errorf("decode metadata: %w", err)
	}

	file := pickBestFile(meta.Files)
	if file == nil {
		return nil, fmt.Errorf("no preferred audio file")
	}

	fileURL := fmt.Sprintf("%s/%s/%s",
		iaDownloadURL,
		url.PathEscape(doc.Identifier),
		url.PathEscape(file.Name),
	)

	title := meta.Metadata.Title
	if title == "" {
		title = doc.Title
	}

	yearRaw := meta.Metadata.Year
	if len(yearRaw) == 0 || string(yearRaw) == "null" {
		yearRaw = doc.Year
	}

	return &MediaItem{
		Identifier:  doc.Identifier,
		Title:       title,
		Year:        parseYear(yearRaw),
		MediaType:   meta.Metadata.MediaType,
		FileURL:     fileURL,
		Duration:    parseLength(file.Length),
		RetrievedAt: time.Now(),
	}, nil
}

// pickBestFile returns the highest-preference audio file from files,
// or nil if none match.
func pickBestFile(files []iaFile) *iaFile {
	for _, preferred := range preferredFormats {
		for i := range files {
			if strings.EqualFold(files[i].Format, preferred) {
				return &files[i]
			}
		}
	}
	return nil
}

// docHasPreferredFormat reports whether a search doc's format field contains
// at least one preferred audio format. The field may be a JSON string or array.
func docHasPreferredFormat(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	check := func(s string) bool {
		for _, pref := range preferredFormats {
			if strings.EqualFold(s, pref) {
				return true
			}
		}
		return false
	}
	var arr []string
	if json.Unmarshal(raw, &arr) == nil {
		for _, f := range arr {
			if check(f) {
				return true
			}
		}
		return false
	}
	var single string
	if json.Unmarshal(raw, &single) == nil {
		return check(single)
	}
	return false
}

// parseYear extracts a 4-digit year from a json.RawMessage that may be a
// string ("1932", "1932-01-01") or a bare number.
func parseYear(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var s string
	if json.Unmarshal(raw, &s) == nil && len(s) >= 4 {
		if y, err := strconv.Atoi(s[:4]); err == nil {
			return y
		}
	}
	var n float64
	if json.Unmarshal(raw, &n) == nil {
		return int(n)
	}
	return 0
}

// parseLength parses an Archive.org file length string (seconds as float) into float64.
func parseLength(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	d, _ := strconv.ParseFloat(s, 64)
	return d
}
