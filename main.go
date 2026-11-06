// immich-mcp is a dependency-free MCP stdio server exposing a small, read-heavy
// slice of the Immich REST API to Odysseus. Configuration via environment:
//
//	IMMICH_URL     base URL, e.g. http://host.docker.internal:7214
//	IMMICH_API_KEY user API key (x-api-key header)
//
// Protocol: newline-delimited JSON-RPC 2.0 on stdin/stdout (MCP stdio
// transport). Logs go to stderr; stdout carries only protocol frames.
package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const serverVersion = "1.0.0"
const maxTextChars = 15000

var httpClient = &http.Client{Timeout: 20 * time.Second}

var baseURL = envOr("IMMICH_URL", "http://127.0.0.1:7214")
var apiKey = os.Getenv("IMMICH_API_KEY")

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return strings.TrimRight(v, "/")
	}
	return fallback
}

// ---------- JSON-RPC plumbing ----------

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type toolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

var out = bufio.NewWriter(os.Stdout)

func respond(id json.RawMessage, result any) {
	frame, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "result": result})
	out.Write(frame)
	out.WriteByte('\n')
	out.Flush()
}

func respondError(id json.RawMessage, code int, message string) {
	frame, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "error": map[string]any{"code": code, "message": message}})
	out.Write(frame)
	out.WriteByte('\n')
	out.Flush()
}

func hasID(id json.RawMessage) bool {
	return len(id) != 0 && string(id) != "null"
}

// ---------- Immich HTTP helpers ----------

func apiGet(path string, query map[string]string) (int, []byte, error) {
	u, err := url.Parse(baseURL + path)
	if err != nil {
		return 0, nil, err
	}
	if len(query) > 0 {
		q := u.Query()
		for k, v := range query {
			q.Set(k, v)
		}
		u.RawQuery = q.Encode()
	}
	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}

func apiPost(path string, payload any) (int, []byte, error) {
	buf, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequest(http.MethodPost, baseURL+path, bytes.NewReader(buf))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}

func clip(s string) string {
	if len(s) > maxTextChars {
		return s[:maxTextChars] + fmt.Sprintf("\n...(truncated, %d chars total)", len(s))
	}
	return s
}

func okText(body []byte) map[string]any {
	return map[string]any{"content": []textContent{{Type: "text", Text: clip(string(body))}}}
}

func errText(format string, args ...any) map[string]any {
	return map[string]any{
		"content": []textContent{{Type: "text", Text: fmt.Sprintf(format, args...)}},
		"isError": true,
	}
}

func requireKey() map[string]any {
	if apiKey == "" {
		return errText("IMMICH_API_KEY is not set")
	}
	return nil
}

// ---------- tools ----------

type handler func(args map[string]any) map[string]any

var tools = []toolDef{
	{
		Name:        "search_photos",
		Description: "Smart (natural-language) search across the Immich library. Returns matching assets with ids, filenames, dates and types.",
		InputSchema: objSchema(map[string]any{
			"query": strProp("Natural-language query, e.g. 'wedding beach sunset'"),
			"size":  intProp("Max results 1-100", 1, 100),
		}, []string{"query"}),
	},
	{
		Name:        "list_albums",
		Description: "List Immich albums (id, name, asset count, shared status).",
		InputSchema: objSchema(map[string]any{
			"shared": boolProp("If true, only shared albums; if false, only owned. Omit for all."),
		}, nil),
	},
	{
		Name:        "album_assets",
		Description: "Get one album by id, including its asset list (ids, filenames, dates).",
		InputSchema: objSchema(map[string]any{
			"albumId": strProp("Album UUID"),
		}, []string{"albumId"}),
	},
	{
		Name:        "asset_info",
		Description: "Full metadata for one asset by id: EXIF, dimensions, dates, people, tags, location.",
		InputSchema: objSchema(map[string]any{
			"assetId": strProp("Asset UUID"),
		}, []string{"assetId"}),
	},
	{
		Name:        "search_people",
		Description: "Find faces in the facial-recognition library by name. Returns each match with its person id, name and photo count. Use the id with person_photos.",
		InputSchema: objSchema(map[string]any{
			"query": strProp("Name or name fragment"),
		}, []string{"query"}),
	},
	{
		Name:        "person_photos",
		Description: "Photos and videos of people: pass one person id/name or several via people for photos containing ALL of them. Returns one page of assets (id, filename, date, type, dimensions).",
		InputSchema: objSchema(map[string]any{
			"person": strProp("One person UUID or full name"),
			"people": peopleProp(),
			"size":   intProp("Results per page 1-100", 1, 100),
			"page":   intProp("Page number from 1", 1, 1000000),
		}, []string{"person"}),
	},
	{
		Name:        "show_photo",
		Description: "Display one photo or video still to the user in chat: pass an asset id from person_photos, search_photos or album_assets. The image renders inline; prefer preview size, thumbnail for a small quick view.",
		InputSchema: objSchema(map[string]any{
			"assetId": strProp("Asset UUID"),
			"size":    strProp("thumbnail or preview, default preview"),
		}, []string{"assetId"}),
	},
	{
		Name:        "photo_search",
		Description: "Filtered library search: person id or name, date range, favorites flag, media type. Dates like 2026-10-01 (time optional). Returns projected asset rows with ids for show_photo.",
		InputSchema: objSchema(map[string]any{
			"person":      strProp("One person UUID or name. Omit for anyone."),
			"people":      peopleProp(),
			"takenAfter":  strProp("Only photos taken at or after this date, e.g. 2026-04-01"),
			"takenBefore": strProp("Only photos taken before this date"),
			"isFavorite":  boolProp("True for favorites only, false for non-favorites. Omit for both."),
			"type":        strProp("IMAGE or VIDEO. Omit for both."),
			"size":        intProp("Results per page 1-100", 1, 100),
			"page":        intProp("Page number from 1", 1, 1000000),
		}, nil),
	},
	{
		Name:        "recent_photos",
		Description: "Latest library photos first, optionally scoped to one person id or name. Returns projected asset rows with ids for show_photo.",
		InputSchema: objSchema(map[string]any{
			"person": strProp("Person UUID or name. Omit for the whole library."),
			"size":   intProp("How many 1-100", 1, 100),
		}, nil),
	},
	{
		Name:        "favorite_photo",
		Description: "Favorite or unfavorite one asset by id. Reversible.",
		InputSchema: objSchema(map[string]any{
			"assetId":  strProp("Asset UUID"),
			"favorite": boolProp("True to favorite (default), false to unfavorite."),
		}, []string{"assetId"}),
	},
	{
		Name:        "name_person",
		Description: "Name a face: pass a person id from search_people (also accepts the current name). Use for the 19 unnamed faces.",
		InputSchema: objSchema(map[string]any{
			"person": strProp("Person UUID"),
			"name":   strProp("Full name to assign"),
		}, []string{"person", "name"}),
	},
	{
		Name:        "random_memory",
		Description: "Draw one random photo as a memory lane moment, optionally from one person's photos. Returns the asset row; call show_photo to display it.",
		InputSchema: objSchema(map[string]any{
			"person": strProp("Person UUID or name. Omit for the whole library."),
		}, nil),
	},
	{
		Name:        "album_build",
		Description: "Create an album and fill it in one step. Gather asset ids from person_photos, photo_search or search_photos first, then pass them here.",
		InputSchema: objSchema(map[string]any{
			"name":     strProp("Album title"),
			"assetIds": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Up to 200 asset UUIDs"},
		}, []string{"name", "assetIds"}),
	},
	{
		Name:        "server_stats",
		Description: "Library-wide usage statistics (photos, videos, storage by user). Requires an admin API key.",
		InputSchema: objSchema(map[string]any{}, nil),
	},
}

var handlers = map[string]handler{
	"search_photos":  hSearchPhotos,
	"list_albums":    hListAlbums,
	"album_assets":   hAlbumAssets,
	"asset_info":     hAssetInfo,
	"server_stats":   hServerStats,
	"show_photo":     hShowPhoto,
	"photo_search":   hPhotoSearch,
	"recent_photos":  hRecentPhotos,
	"favorite_photo": hFavoritePhoto,
	"name_person":    hNamePerson,
	"random_memory":  hRandomMemory,
	"album_build":    hAlbumBuild,
	"search_people":  hSearchPeople,
	"person_photos":  hPersonPhotos,
}

func strArg(args map[string]any, key string) string {
	v, _ := args[key].(string)
	return v
}

func intArg(args map[string]any, key string, def int) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func hSearchPhotos(args map[string]any) map[string]any {
	if e := requireKey(); e != nil {
		return e
	}
	size := intArg(args, "size", 20)
	if size < 1 {
		size = 1
	}
	if size > 100 {
		size = 100
	}
	status, body, err := apiPost("/api/search/smart", map[string]any{"query": strArg(args, "query"), "size": size})
	if err != nil {
		return errText("search request failed: %v", err)
	}
	if status >= 400 {
		return errText("Immich HTTP %d: %s", status, clip(string(body)))
	}
	return okText(body)
}

func hListAlbums(args map[string]any) map[string]any {
	if e := requireKey(); e != nil {
		return e
	}
	q := map[string]string{}
	if v, present := args["shared"].(bool); present {
		q["shared"] = strconv.FormatBool(v)
	}
	status, body, err := apiGet("/api/albums", q)
	if err != nil {
		return errText("list albums failed: %v", err)
	}
	if status >= 400 {
		return errText("Immich HTTP %d: %s", status, clip(string(body)))
	}
	return okText(body)
}

func hAlbumAssets(args map[string]any) map[string]any {
	if e := requireKey(); e != nil {
		return e
	}
	id := strArg(args, "albumId")
	if id == "" {
		return errText("albumId is required")
	}
	status, body, err := apiGet("/api/albums/"+url.PathEscape(id), nil)
	if err != nil {
		return errText("album lookup failed: %v", err)
	}
	if status >= 400 {
		return errText("Immich HTTP %d: %s", status, clip(string(body)))
	}
	return okText(body)
}

func hAssetInfo(args map[string]any) map[string]any {
	if e := requireKey(); e != nil {
		return e
	}
	id := strArg(args, "assetId")
	if id == "" {
		return errText("assetId is required")
	}
	status, body, err := apiGet("/api/assets/"+url.PathEscape(id), nil)
	if err != nil {
		return errText("asset lookup failed: %v", err)
	}
	if status >= 400 {
		return errText("Immich HTTP %d: %s", status, clip(string(body)))
	}
	return okText(body)
}

func hServerStats(_ map[string]any) map[string]any {
	if e := requireKey(); e != nil {
		return e
	}
	status, body, err := apiGet("/api/server/statistics", nil)
	if err != nil {
		return errText("statistics lookup failed: %v", err)
	}
	if status >= 400 {
		return errText("Immich HTTP %d: %s", status, clip(string(body)))
	}
	return okText(body)
}

type person struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func getPeople() ([]person, error) {
	status, body, err := apiGet("/api/people", nil)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, fmt.Errorf("Immich HTTP %d: %s", status, firstChars(string(body), 200))
	}
	var envelope struct {
		People []person `json:"people"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, err
	}
	return envelope.People, nil
}

func firstChars(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func personAssetCount(id string) int {
	status, body, err := apiGet("/api/people/"+url.PathEscape(id)+"/statistics", nil)
	if err != nil || status >= 400 {
		return -1
	}
	var st struct {
		Assets int `json:"assets"`
	}
	if err := json.Unmarshal(body, &st); err != nil {
		return -1
	}
	return st.Assets
}

// resolvePerson accepts a UUID (verified live) or a name. Names match exact
// case-insensitive first, then unique substring; ambiguous or missing names
// return a descriptive error listing the candidates.
func resolvePerson(ref string) (string, string, map[string]any) {
	if e := requireKey(); e != nil {
		return "", "", e
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", "", errText("person is required")
	}
	if status, body, err := apiGet("/api/people/"+url.PathEscape(ref), nil); err == nil && status < 400 {
		var p person
		if json.Unmarshal(body, &p) == nil && p.ID != "" {
			return p.ID, p.Name, nil
		}
	}
	people, err := getPeople()
	if err != nil {
		return "", "", errText("people lookup failed: %v", err)
	}
	lower := strings.ToLower(ref)
	for _, p := range people {
		if strings.ToLower(p.Name) == lower && p.Name != "" {
			return p.ID, p.Name, nil
		}
	}
	var matches []person
	for _, p := range people {
		if p.Name != "" && strings.Contains(strings.ToLower(p.Name), lower) {
			matches = append(matches, p)
		}
	}
	if len(matches) == 1 {
		return matches[0].ID, matches[0].Name, nil
	}
	if len(matches) == 0 {
		return "", "", errText("no face named %q", ref)
	}
	names := make([]string, 0, len(matches))
	for _, m := range matches {
		names = append(names, m.Name+" ("+m.ID+")")
	}
	return "", "", errText("ambiguous name %q, candidates: %s", ref, strings.Join(names, "; "))
}

func projectPersonAssets(body []byte) []byte {
	var envelope struct {
		Assets struct {
			Total int              `json:"total"`
			Items []map[string]any `json:"items"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return body
	}
	keep := []string{"id", "originalFileName", "type", "localDateTime", "fileCreatedAt", "width", "height", "isFavorite", "duration"}
	items := make([]map[string]any, 0, len(envelope.Assets.Items))
	for _, it := range envelope.Assets.Items {
		row := map[string]any{}
		for _, k := range keep {
			if v, present := it[k]; present {
				row[k] = v
			}
		}
		items = append(items, row)
	}
	compact, err := json.Marshal(map[string]any{"total": envelope.Assets.Total, "items": items})
	if err != nil {
		return body
	}
	return compact
}

func hSearchPeople(args map[string]any) map[string]any {
	if e := requireKey(); e != nil {
		return e
	}
	q := strings.ToLower(strings.TrimSpace(strArg(args, "query")))
	if q == "" {
		return errText("query is required")
	}
	people, err := getPeople()
	if err != nil {
		return errText("people lookup failed: %v", err)
	}
	type hit struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Photos int    `json:"photos"`
	}
	hits := []hit{}
	for _, p := range people {
		if p.Name != "" && strings.Contains(strings.ToLower(p.Name), q) {
			hits = append(hits, hit{ID: p.ID, Name: p.Name, Photos: personAssetCount(p.ID)})
		}
	}
	buf, _ := json.Marshal(map[string]any{"matches": hits})
	return okText(buf)
}

// resolvePeople gathers person ids from the "person" (single id or name)
// and/or "people" (array of ids or names) arguments, deduplicated.
func resolvePeople(args map[string]any) ([]string, []string, map[string]any) {
	var refs []string
	if ref := strings.TrimSpace(strArg(args, "person")); ref != "" {
		refs = append(refs, ref)
	}
	refs = append(refs, toStringSlice(args["people"])...)
	seen := map[string]bool{}
	var ids, names []string
	for _, ref := range refs {
		if v := strings.TrimSpace(ref); v != "" && !seen[v] {
			seen[v] = true
			id, name, errMap := resolvePerson(v)
			if errMap != nil {
				return nil, nil, errMap
			}
			ids = append(ids, id)
			if name == "" {
				name = id
			}
			names = append(names, name)
		}
	}
	if len(ids) == 0 {
		return nil, nil, errText("person or people is required")
	}
	return ids, names, nil
}

func peopleProp() map[string]any {
	return map[string]any{
		"type":        "array",
		"items":       map[string]any{"type": "string"},
		"description": "Several person UUIDs or names for AND matching (photos containing ALL of them), e.g. [\"Mia\", \"Noah\"]",
	}
}

func hPersonPhotos(args map[string]any) map[string]any {
	ids, names, errMap := resolvePeople(args)
	if errMap != nil {
		return errMap
	}
	size := intArg(args, "size", 20)
	if size < 1 {
		size = 1
	}
	if size > 100 {
		size = 100
	}
	page := intArg(args, "page", 1)
	if page < 1 {
		page = 1
	}
	status, body, err := apiPost("/api/search/metadata", map[string]any{"personIds": ids, "size": size, "page": page})
	if err != nil {
		return errText("photo search failed: %v", err)
	}
	if status >= 400 {
		return errText("Immich HTTP %d: %s", status, clip(string(body)))
	}
	projected := projectPersonAssets(body)
	var wrap map[string]any
	if err := json.Unmarshal(projected, &wrap); err != nil {
		return okText(body)
	}
	wrap["persons"] = names
	wrap["personIds"] = ids
	wrap["page"] = page
	buf, _ := json.Marshal(wrap)
	return okText(buf)
}

func hPhotoSearch(args map[string]any) map[string]any {
	if e := requireKey(); e != nil {
		return e
	}
	payload := map[string]any{}
	if hasPeople(args) {
		ids, _, errMap := resolvePeople(args)
		if errMap != nil {
			return errMap
		}
		payload["personIds"] = ids
	}
	if v := normDateTime(strArg(args, "takenAfter")); v != "" {
		payload["takenAfter"] = v
	}
	if v := normDateTime(strArg(args, "takenBefore")); v != "" {
		payload["takenBefore"] = v
	}
	if _, present := args["isFavorite"]; present {
		payload["isFavorite"] = boolArg(args, "isFavorite", false)
	}
	if v := strings.ToUpper(strings.TrimSpace(strArg(args, "type"))); v == "IMAGE" || v == "VIDEO" {
		payload["type"] = v
	}
	size := intArg(args, "size", 20)
	if size < 1 {
		size = 1
	}
	if size > 100 {
		size = 100
	}
	payload["size"] = size
	if page := intArg(args, "page", 1); page > 1 {
		payload["page"] = page
	}
	status, body, err := apiPost("/api/search/metadata", payload)
	if err != nil {
		return errText("photo search failed: %v", err)
	}
	if status >= 400 {
		return errText("Immich HTTP %d: %s", status, clip(string(body)))
	}
	return okText(projectPersonAssets(body))
}

func hasPeople(args map[string]any) bool {
	if strings.TrimSpace(strArg(args, "person")) != "" {
		return true
	}
	for _, raw := range toStringSlice(args["people"]) {
		if strings.TrimSpace(raw) != "" {
			return true
		}
	}
	return false
}

func apiGetBytes(path string, query map[string]string) (int, string, []byte, error) {
	u, err := url.Parse(baseURL + path)
	if err != nil {
		return 0, "", nil, err
	}
	if len(query) > 0 {
		q := u.Query()
		for k, v := range query {
			q.Set(k, v)
		}
		u.RawQuery = q.Encode()
	}
	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, "", nil, err
	}
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("Accept", "image/*")
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, "", nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 6<<20))
	if err != nil {
		return resp.StatusCode, "", nil, err
	}
	return resp.StatusCode, resp.Header.Get("Content-Type"), body, nil
}

func hShowPhoto(args map[string]any) map[string]any {
	if e := requireKey(); e != nil {
		return e
	}
	id := strArg(args, "assetId")
	if id == "" {
		return errText("assetId is required")
	}
	size := strings.ToLower(strings.TrimSpace(strArg(args, "size")))
	if size != "thumbnail" && size != "preview" {
		size = "preview"
	}
	status, ctype, body, err := apiGetBytes("/api/assets/"+url.PathEscape(id)+"/thumbnail", map[string]string{"size": size})
	if err != nil {
		return errText("photo fetch failed: %v", err)
	}
	if status >= 400 {
		return errText("Immich HTTP %d: %s", status, firstChars(string(body), 200))
	}
	if ctype == "" {
		ctype = "image/jpeg"
	}
	if !strings.HasPrefix(ctype, "image/") {
		return errText("unexpected content type %s", ctype)
	}
	return map[string]any{"content": []any{
		map[string]any{"type": "text", "text": "Photo " + id + " (" + size + ", " + ctype + ", " + strconv.Itoa(len(body)) + " bytes)"},
		map[string]any{"type": "image", "data": base64.StdEncoding.EncodeToString(body), "mimeType": ctype},
	}}
}

func apiPut(path string, payload any) (int, []byte, error) {
	buf, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequest(http.MethodPut, baseURL+path, bytes.NewReader(buf))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}

// normDateTime accepts a bare date (2026-04-01, assumed UTC midnight)
// or a full datetime and returns the full form Immich validates.
func normDateTime(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if len(v) == len("2006-01-02") {
		return v + "T00:00:00Z"
	}
	return v
}

func boolArg(args map[string]any, key string, def bool) bool {
	if v, present := args[key].(bool); present {
		return v
	}
	return def
}

func hRecentPhotos(args map[string]any) map[string]any {
	if e := requireKey(); e != nil {
		return e
	}
	payload := map[string]any{}
	if ref := strings.TrimSpace(strArg(args, "person")); ref != "" {
		id, _, errMap := resolvePerson(ref)
		if errMap != nil {
			return errMap
		}
		payload["personIds"] = []string{id}
	}
	size := intArg(args, "size", 20)
	if size < 1 {
		size = 1
	}
	if size > 100 {
		size = 100
	}
	payload["size"] = size
	status, body, err := apiPost("/api/search/metadata", payload)
	if err != nil {
		return errText("recent lookup failed: %v", err)
	}
	if status >= 400 {
		return errText("Immich HTTP %d: %s", status, clip(string(body)))
	}
	return okText(projectPersonAssets(body))
}

func hFavoritePhoto(args map[string]any) map[string]any {
	if e := requireKey(); e != nil {
		return e
	}
	id := strArg(args, "assetId")
	if id == "" {
		return errText("assetId is required")
	}
	fav := boolArg(args, "favorite", true)
	status, body, err := apiPut("/api/assets/"+url.PathEscape(id), map[string]any{"isFavorite": fav})
	if err != nil {
		return errText("favorite update failed: %v", err)
	}
	if status >= 400 {
		return errText("Immich HTTP %d: %s", status, clip(string(body)))
	}
	verb := "favorited"
	if !fav {
		verb = "unfavorited"
	}
	buf, _ := json.Marshal(map[string]any{"assetId": id, "result": verb})
	return okText(buf)
}

func hNamePerson(args map[string]any) map[string]any {
	if e := requireKey(); e != nil {
		return e
	}
	id, _, errMap := resolvePerson(strArg(args, "person"))
	if errMap != nil {
		return errMap
	}
	name := strings.TrimSpace(strArg(args, "name"))
	if name == "" {
		return errText("name is required and cannot be blank")
	}
	status, body, err := apiPut("/api/people/"+url.PathEscape(id), map[string]any{"name": name})
	if err != nil {
		return errText("rename failed: %v", err)
	}
	if status >= 400 {
		return errText("Immich HTTP %d: %s", status, clip(string(body)))
	}
	buf, _ := json.Marshal(map[string]any{"personId": id, "name": name})
	return okText(buf)
}

func hRandomMemory(args map[string]any) map[string]any {
	if e := requireKey(); e != nil {
		return e
	}
	payload := map[string]any{"size": 100}
	if ref := strings.TrimSpace(strArg(args, "person")); ref != "" {
		id, _, errMap := resolvePerson(ref)
		if errMap != nil {
			return errMap
		}
		payload["personIds"] = []string{id}
	}
	status, body, err := apiPost("/api/search/metadata", payload)
	if err != nil {
		return errText("memory draw failed: %v", err)
	}
	if status >= 400 {
		return errText("Immich HTTP %d: %s", status, clip(string(body)))
	}
	var envelope struct {
		Assets struct {
			Items []map[string]any `json:"items"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || len(envelope.Assets.Items) == 0 {
		return errText("no photos to draw from")
	}
	var seed [8]byte
	if _, err := rand.Read(seed[:]); err != nil {
		return errText("random draw failed: %v", err)
	}
	pick := envelope.Assets.Items[int(binary.LittleEndian.Uint64(seed[:])%uint64(len(envelope.Assets.Items)))]
	keep := []string{"id", "originalFileName", "type", "localDateTime", "fileCreatedAt", "width", "height", "isFavorite"}
	row := map[string]any{}
	for _, k := range keep {
		if v, present := pick[k]; present {
			row[k] = v
		}
	}
	row["poolSize"] = len(envelope.Assets.Items)
	buf, _ := json.Marshal(row)
	return okText(buf)
}

func hAlbumBuild(args map[string]any) map[string]any {
	if e := requireKey(); e != nil {
		return e
	}
	name := strings.TrimSpace(strArg(args, "name"))
	if name == "" {
		return errText("name is required")
	}
	var ids []string
	for _, raw := range toStringSlice(args["assetIds"]) {
		if v := strings.TrimSpace(raw); v != "" {
			ids = append(ids, v)
		}
	}
	if len(ids) == 0 {
		return errText("assetIds is required: gather ids from person_photos, photo_search or search_photos first")
	}
	if len(ids) > 200 {
		return errText("at most 200 assets per album build, got %d", len(ids))
	}
	status, body, err := apiPost("/api/albums", map[string]any{"albumName": name})
	if err != nil {
		return errText("album create failed: %v", err)
	}
	if status >= 400 {
		return errText("Immich HTTP %d: %s", status, clip(string(body)))
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &created); err != nil || created.ID == "" {
		return errText("album create returned no id")
	}
	status, body, err = apiPut("/api/albums/"+url.PathEscape(created.ID)+"/assets", map[string]any{"ids": ids})
	if err != nil {
		return errText("album fill failed (album %s exists): %v", created.ID, err)
	}
	if status >= 400 {
		return errText("album fill HTTP %d (album %s exists): %s", status, created.ID, clip(string(body)))
	}
	buf, _ := json.Marshal(map[string]any{"albumId": created.ID, "name": name, "assetsAdded": len(ids)})
	return okText(buf)
}

func toStringSlice(v any) []string {
	switch t := v.(type) {
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return t
	case string:
		if t == "" {
			return nil
		}
		return []string{t}
	}
	return nil
}

// ---------- schema helpers ----------

func objSchema(props map[string]any, required []string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func intProp(desc string, min, max int) map[string]any {
	return map[string]any{"type": "integer", "description": desc, "minimum": min, "maximum": max}
}

func boolProp(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

// ---------- dispatch ----------

func handle(req request) {
	if !hasID(req.ID) {
		return // notification: nothing to answer
	}
	switch req.Method {
	case "initialize":
		version := "2024-11-05"
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if err := json.Unmarshal(req.Params, &p); err == nil && p.ProtocolVersion != "" {
			version = p.ProtocolVersion
		}
		respond(req.ID, map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "immich-mcp", "version": serverVersion},
		})
	case "ping":
		respond(req.ID, map[string]any{})
	case "tools/list":
		respond(req.ID, map[string]any{"tools": tools})
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			respondError(req.ID, -32602, "invalid params")
			return
		}
		if p.Arguments == nil {
			p.Arguments = map[string]any{}
		}
		h, ok := handlers[p.Name]
		if !ok {
			respondError(req.ID, -32602, "unknown tool: "+p.Name)
			return
		}
		respond(req.ID, h(p.Arguments))
	default:
		respondError(req.ID, -32601, "method not found: "+req.Method)
	}
}

func main() {
	logf("immich-mcp %s up (url=%s)", serverVersion, baseURL)
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 10<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			logf("dropping undecodable frame: %v", err)
			continue
		}
		handle(req)
	}
	if err := sc.Err(); err != nil {
		logf("stdin error: %v", err)
		os.Exit(1)
	}
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}
