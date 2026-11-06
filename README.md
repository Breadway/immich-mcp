# immich-mcp

Dependency-free MCP stdio server (Go stdlib only) exposing a read-heavy slice
of the Immich REST API to Odysseus.

## Build

```sh
go build -o immich-mcp .
```

## Env

- `IMMICH_URL` - base URL (default `http://127.0.0.1:7214`)
- `IMMICH_API_KEY` - user API key, sent as the `x-api-key` header

## Tools

`search_photos`, `list_albums`, `album_assets`, `asset_info`,
`search_people`, `person_photos`, `server_stats` (admin key needed for stats), `show_photo`.

`search_people` matches facial-recognition names (exact, then unique
substring); `person_photos` takes a person id or full name plus `size`/`page`
and returns projected asset rows.

## Hestia wiring

The binary is mounted read-only into the Odysseus container at
`/opt/host-mcp/immich-mcp` (see `docker-compose.override.yml` in
`/mnt/media/llm/odysseus/src`) and registered in the `mcp_servers` table
(transport `stdio`). From inside the container the URL must be
`http://host.docker.internal:7214`.
