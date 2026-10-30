// HTTP client for the mdwebd backend. All paths are absolute so the <base>
// element (set for image resolution) never affects API requests.

const API_FILE_PATH = "/api/file";
const WRITE_HEADER_NAME = "X-Mdwebd";
const WRITE_HEADER_VALUE = "1";
const HTTP_CONFLICT = 409;

export interface FileResponse {
  path: string;
  content: string;
  version: string;
  size: number;
  modified: string;
  normalized: { crlf: boolean; bom: boolean };
}

export interface SaveResponse {
  path: string;
  version: string;
  size: number;
}

export interface ErrorBody {
  error: string;
  version?: string;
}

/** Thrown for any non-2xx response; `status` lets callers special-case 409. */
export class ApiError extends Error {
  readonly status: number;
  readonly currentVersion: string | undefined;

  constructor(status: number, message: string, currentVersion?: string) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.currentVersion = currentVersion;
  }

  get isConflict(): boolean {
    return this.status === HTTP_CONFLICT;
  }
}

async function readError(resp: Response): Promise<ApiError> {
  let message = `HTTP ${resp.status}`;
  let version: string | undefined;
  try {
    const body: unknown = await resp.json();
    if (typeof body === "object" && body !== null) {
      const err = body as Partial<ErrorBody>;
      if (typeof err.error === "string" && err.error !== "") message = err.error;
      if (typeof err.version === "string") version = err.version;
    }
  } catch {
    // Not JSON; keep the HTTP status text.
  }
  return new ApiError(resp.status, message, version);
}

export async function fetchFile(path: string): Promise<FileResponse> {
  const url = `${API_FILE_PATH}?path=${encodeURIComponent(path)}`;
  const resp = await fetch(url, { method: "GET", credentials: "same-origin" });
  if (!resp.ok) throw await readError(resp);
  return (await resp.json()) as FileResponse;
}

export async function saveFile(path: string, content: string, version: string): Promise<SaveResponse> {
  const resp = await fetch(API_FILE_PATH, {
    method: "PUT",
    credentials: "same-origin",
    headers: {
      "Content-Type": "application/json",
      [WRITE_HEADER_NAME]: WRITE_HEADER_VALUE,
    },
    body: JSON.stringify({ path, content, version }),
  });
  if (!resp.ok) throw await readError(resp);
  return (await resp.json()) as SaveResponse;
}
