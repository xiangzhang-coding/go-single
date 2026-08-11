# Demo Frontend

`web/` contains independent Vite themes. `faire/` is the first theme and talks to the Go API through `/api`.

```bash
cd web/faire
bun install
bun run dev
```

The development server proxies `/api` and `/ws` to `http://127.0.0.1:8080` by default. Set `VITE_API_BASE` and `VITE_WS_BASE` for a separately deployed frontend.
