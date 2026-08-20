# Demo Frontend

`web/` contains independent Vite themes. `faire/` is the first theme and talks to the Go API through `/api`.

```bash
cd web/faire
bun install
bun run dev
```

The development server proxies `/api` and `/ws` to `http://127.0.0.1:8080` by default. For a separately deployed frontend, set `VITE_API_BASE` to an absolute URL ending in `/api` and configure `VITE_WS_BASE` separately.
