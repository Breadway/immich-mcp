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
		Name:        "server_stats",
		Description: "Library-wide usage statistics (photos, videos, storage by user). Requires an admin API key.",
		InputSchema: objSchema(map[string]any{}, nil),
	},
}

var handlers = map[string]handler{
	"search_photos": hSearchPhotos,
	"list_albums":   hListAlbums,
	"album_assets":  hAlbumAssets,
	"asset_info":    hAssetInfo,
	"server_stats":  hServerStats,
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
