// The sub-path the app is mounted under, read from the <base href> the server
// injects. Lets one binary run at "/" or under "/prober" with no rebuild.
export const BASE = new URL(document.baseURI).pathname

// apiURL builds an absolute API path under the mount point.
export function apiURL(path: string): string {
  const base = BASE.endsWith('/') ? BASE.slice(0, -1) : BASE
  return `${base}/api/${path.replace(/^\//, '')}`
}
