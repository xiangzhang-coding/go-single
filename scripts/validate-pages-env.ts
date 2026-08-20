export function validatePagesEnvironment(apiBase: string, wsBase: string): void {
  validateEndpoint("VITE_API_BASE", apiBase, "https:", "/api");
  validateEndpoint("VITE_WS_BASE", wsBase, "wss:", "/ws");
}

function validateEndpoint(name: string, value: string, protocol: string, pathname: string): void {
  if (!value || /[\u0000-\u0020\u007f]/u.test(value)) {
    throw new Error(`${name} 包含空白或控制字符`);
  }
  let parsed: URL;
  try {
    parsed = new URL(value);
  } catch {
    throw new Error(`${name} 必须是有效绝对 URL`);
  }
  if (parsed.protocol !== protocol || !parsed.hostname || parsed.pathname !== pathname) {
    throw new Error(`${name} 必须是 ${protocol}//host${pathname}`);
  }
  if (parsed.username || parsed.password || parsed.search || parsed.hash) {
    throw new Error(`${name} 不得包含凭据、查询参数或片段`);
  }
}

if (import.meta.main) {
  try {
    validatePagesEnvironment(process.env.VITE_API_BASE ?? "", process.env.VITE_WS_BASE ?? "");
  } catch (error) {
    console.error(error instanceof Error ? error.message : error);
    process.exit(1);
  }
}
