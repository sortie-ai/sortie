import type {
  AcknowledgeResponse,
  CheckNowResponse,
  IssueDetailResponse,
  IssueGroupsResponse,
  PricesResponse,
  RefreshResponse,
  Report,
  RetryNowResponse,
  RunsResponse,
  StateResponse,
} from "@/api/types.gen";

const API_ROOT = "/api/v1/";

/** Signals a failed API call; `status` is 0 when no response arrived. */
export class ApiError extends Error {
  readonly status: number;
  readonly code: string | null;
  readonly serverMessage: string | null;

  constructor(status: number, code: string | null, serverMessage: string | null) {
    super(serverMessage ?? (status === 0 ? "No response" : `HTTP ${status}`));
    this.name = "ApiError";
    this.status = status;
    this.code = code;
    this.serverMessage = serverMessage;
  }
}

async function errorFrom(response: Response): Promise<ApiError> {
  try {
    const body: unknown = await response.json();
    const envelope = (body as { error?: { code?: unknown; message?: unknown } } | null)?.error;
    return new ApiError(
      response.status,
      typeof envelope?.code === "string" ? envelope.code : null,
      typeof envelope?.message === "string" ? envelope.message : null,
    );
  } catch {
    return new ApiError(response.status, null, null);
  }
}

async function send(path: string, init: RequestInit): Promise<Response> {
  try {
    return await fetch(`${API_ROOT}${path}`, init);
  } catch (error) {
    if (init.signal?.aborted) {
      throw error;
    }
    throw new ApiError(0, null, null);
  }
}

async function getJson<T>(path: string, signal?: AbortSignal): Promise<T> {
  const response = await send(path, { signal });
  if (!response.ok) {
    throw await errorFrom(response);
  }
  return (await response.json()) as T;
}

async function postJson<T, S extends number>(
  path: string,
  accepted: readonly S[],
): Promise<{ status: S; body: T }> {
  const response = await send(path, { method: "POST" });
  const status = accepted.find((candidate) => candidate === response.status);
  if (status === undefined) {
    throw await errorFrom(response);
  }
  return { status, body: (await response.json()) as T };
}

function withQuery(path: string, query: Record<string, string | number | undefined>): string {
  const params = new URLSearchParams();
  for (const [name, value] of Object.entries(query)) {
    if (value !== undefined) {
      params.set(name, String(value));
    }
  }
  const text = params.toString();
  return text === "" ? path : `${path}?${text}`;
}

export function getState(signal?: AbortSignal): Promise<StateResponse> {
  return getJson<StateResponse>("state", signal);
}

export function getIssue(identifier: string, signal?: AbortSignal): Promise<IssueDetailResponse> {
  return getJson<IssueDetailResponse>(encodeURIComponent(identifier), signal);
}

/** The History filters as the API takes them; an absent member is no filter. */
export interface HistoryFilter {
  issue?: string;
  rule?: string;
  status?: string;
  stage?: string;
}

export function getRuns(
  q: {
    issue_id?: string;
    rule?: string;
    status?: string;
    stage?: string;
    ended_since?: string;
    limit?: number;
    cursor?: string;
  },
  signal?: AbortSignal,
): Promise<RunsResponse> {
  return getJson<RunsResponse>(withQuery("runs", q), signal);
}

export function getIssueGroups(
  q: HistoryFilter & { q?: string; cursor?: string; limit?: number },
  signal?: AbortSignal,
): Promise<IssueGroupsResponse> {
  return getJson<IssueGroupsResponse>(withQuery("runs/issues", { ...q }), signal);
}

/** Fetches the run statistics report; without `since` it covers every run. */
export function getStats(q: { since?: string }, signal?: AbortSignal): Promise<Report> {
  return getJson<Report>(withQuery("stats", q), signal);
}

export function getPrices(signal?: AbortSignal): Promise<PricesResponse> {
  return getJson<PricesResponse>("prices", signal);
}

/** Requests a poll and reconcile; `202` queued it, `409` is the server's refusal. */
export function postRefresh(): Promise<{ status: 202 | 409; body: RefreshResponse }> {
  return postJson<RefreshResponse, 202 | 409>("refresh", [202, 409]);
}

/** Acknowledges an attention item; `409` is the server's refusal, with its own wording. */
export function postAcknowledge(
  id: string,
): Promise<{ status: 200 | 409; body: AcknowledgeResponse }> {
  return postJson<AcknowledgeResponse, 200 | 409>(
    `attention/${encodeURIComponent(id)}/acknowledge`,
    [200, 409],
  );
}

/** Starts a scheduled retry now; `409` is the server's refusal, with its own wording. */
export function postRetryNow(
  identifier: string,
): Promise<{ status: 202 | 409; body: RetryNowResponse }> {
  return postJson<RetryNowResponse, 202 | 409>(
    `retries/${encodeURIComponent(identifier)}/run`,
    [202, 409],
  );
}

/** Checks a pending pull request reaction now; `409` is the server's refusal, with its own wording. */
export function postCheckNow(
  identifier: string,
  kind: string,
): Promise<{ status: 202 | 409; body: CheckNowResponse }> {
  return postJson<CheckNowResponse, 202 | 409>(
    `reactions/${encodeURIComponent(identifier)}/${encodeURIComponent(kind)}/check`,
    [202, 409],
  );
}
