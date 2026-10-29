# Gemini CLI adapter notes

Working notes for anyone running Gemini CLI through Sortie's generic Agent Client Protocol adapter in `internal/agent/clientprotocol`.

Eligibility: qualified

Product conformance: qualified

The two verdicts are computed separately, and both turn on token accounting. The protocol wire itself carries no usable token figure, but a measurement source outside the protocol supplies one, which puts the protocol route level with the richest native reference. Product conformance holds too: a live run through Sortie's orchestrator with a one-token `agent.max_tokens` ceiling stopped at the ceiling and dispatched nothing after it.

## Where to get the volatile facts

The adapter records Gemini's version per session from the `initialize` handshake. The account's model set comes from Google's model-listing endpoint, which is the only real answer because it depends on the key's subscription. Read the flags off `gemini --help`.

Upstream source lives in `google-gemini/gemini-cli`. When a symptom looks like a protocol bug, treat the bundled JavaScript that the installed version ships as the source of record; the TypeScript tree and the shipped bundle can drift apart.

## Entry points

Gemini has no adapter package, no registered kind, and no adapter metadata. The operator points the generic `agent-client-protocol` kind's `agent.command` at `gemini --acp`; the older `--experimental-acp` spelling is deprecated. Everything below is how Gemini's protocol implementation meets the generic adapter. There is no place in the generic transport to special-case a Gemini quirk.

The one exception is token accounting, which lives in Gemini's own measurement source. The source recognizes a command line that names the `gemini` executable, a path to it, or the `@google/gemini-cli` package, directly or as a wrapper's argument. A command line that hides its runtime is started a second time once the handshake names it, so a wrapped Gemini CLI still reports token usage. On a local launch the source settles the session's figure ahead of the ledger. A source never applies to a remote launch, so Gemini CLI on an SSH worker is measured under the default declaration's mapping as a lower bound.

## Load-bearing capability observations

Token accounting is the one row that carries both verdicts. Session continuation works on every surface. Retry classification blocks neither verdict: its `unknown_outcome` case has no deterministic inducer on any surface, so it carries no obligation.

On the protocol surface, retry classification excludes the human-input case as not applicable. The consent request the asking-posture launch raised offered a refusing option, the client refused it inside the protocol, and the turn went on. That exclusion does not cover a question addressed to a person. The native surfaces have no terminal vocabulary for the human-input outcome at all.

The protocol wire has no token figure Sortie can use. The prompt result's extension block has input and output counts but no cache-read, reasoning, or tool counters, so it is only a presence signal and a lower bound. The figure that reaches the budget comes from the runtime's own telemetry and session journal. That figure carries a per-model split by each record's or message's model. Both native surfaces report token counts directly: the JSON surface per model, the streaming JSON surface in a flatter form.

- protocol turn_disposition: Observed: usable
- protocol retry_classification: Observed: usable
- protocol token_ceiling: Observed: gap
- protocol tool_server_delivery: Observed: usable
- protocol session_continuation: Observed: usable
- protocol permission_handling: Observed: usable
- native_json turn_disposition: Observed: gap
- native_json retry_classification: Observed: gap
- native_json token_ceiling: Observed: usable
- native_json session_continuation: Observed: usable
- native_stream_json turn_disposition: Observed: gap
- native_stream_json retry_classification: Observed: gap
- native_stream_json token_ceiling: Observed: usable
- native_stream_json session_continuation: Observed: usable

## Protocol-specific observations

A stop reason of `end_turn` does not mean the turn ended cleanly. This runtime produces `end_turn`, `max_turn_requests`, `max_tokens`, and `cancelled`, and never `refusal`. Loop detection reports as `max_turn_requests`. `max_tokens` comes only from the runtime's own context-overflow predictor, before the stream runs out.

The stream's real token-limit signal, and the model's safety and recitation blocks, are all folded into `end_turn` by the handler for an invalid stream. So a safety refusal and a turn that ran out of context both look like an ordinary successful `end_turn`.

A passing `limit_reached` row does not test the provider's real ceiling. The runtime's own pre-flight guard rejects the oversize prompt locally, so the row measures the client's size guard, not Gemini's context window.

`session/load` works once it succeeds, but three traps sit in front of it. Check a failing load against them before blaming the session.

The first is an upstream ordering defect. The `session/load` response can reach the wire before the replay notifications finish, although the protocol expects it after the full replay. Sortie starts watching for replayed chunks before it sends the load, and after the response it waits a bounded time for chunks still in flight. That wait is load-bearing; do not trim it.

The second is more expensive. A `session/load` sent in the same UTC minute as the `session/new` that created the session fails and permanently destroys the session's resumability, including every later load. This is an open upstream defect, confirmed live: a load in the following minute from a separate process replayed the full history, and a same-minute load failed every time. The adapter spaces a load out of that minute when the same process created the session. A load from any other process, or by hand, is still exposed.

The third is the credential path. `session/load` needs an authentication type recorded in the runtime's own settings. With the API key only in the environment and no such setting, the load fails. If a load fails on a session this same process just created and drove correctly, check this first.

The handshake advertises no `sessionCapabilities` object, so Sortie never closes a session here through the protocol.

## Usage limits

No usage window reaches the protocol as data. A `usage_update` carries context-window use, the `_meta.quota` block on a prompt result carries token counts despite its name, and a rate-limit failure arrives as an error with a message and no data, so its error code is the only structured member. The runtime does fetch its per-model quota from Google's Code Assist service, so Sortie asks that service when a turn ends, with the runtime's own Google sign-in. The declaration is selected by the handshake name `gemini-cli`.

An error with code `429` ends the turn `usage_limit` on the `agent-client-protocol` kind and holds the configured model, because the runtime's quota buckets are per model. The runtime sets the code from the provider's HTTP status after its own retries, which can run for several minutes, so the refusal arrives late. A capacity refusal arrives as `503` and a quota hit during chat compression as `500`; both stay `response_error`. The pulled buckets supply the reset: with `agent-client-protocol.model` set, only that model's exhausted bucket counts, so a `429` after the retries of a per-minute throttle finds an unexhausted bucket and keeps no reset. Without a configured model the refused model is unknown, because a session on the `auto` model routes a request to another model than the session shows, so the earliest exhausted bucket of any model counts. An API-key or Vertex session pulls no bucket, so its refusal carries no reset and retries on the exponential backoff.

When both `GOOGLE_GENAI_USE_GCA` and `GOOGLE_CLOUD_ACCESS_TOKEN` are set in the runtime's environment, the runtime uses that access token before any store, with no expiry, and so does the pull. For a remote session the two come from the variables the launch forwards, else from the remote shell. Otherwise the pull reads the sign-in from the store the runtime itself reads, in the runtime's own order, and takes from it the access token and its expiry in Unix milliseconds. The rest of the bytes are dropped. Where that store sits depends on `GEMINI_FORCE_ENCRYPTED_FILE_STORAGE`:

- Not `true`: the plain file `oauth_creds.json` under `.gemini` in `GEMINI_CLI_HOME`, else in the home directory. It holds `access_token` and `expiry_date`.
- `true`: the OS keychain first, then the encrypted file `gemini-credentials.json` in the same directory, then the plain file the runtime migrates away from. The first store that holds a sign-in wins.

The keychain is skipped, as the runtime skips it, when `GEMINI_FORCE_FILE_STORAGE` is `true` or `WSL_DISTRO_NAME`, `WSLENV`, or `WSL_INTEROP` is set. A keychain entry and an encrypted-file entry both hold the JSON `{"token": {"accessToken": "...", "expiresAt": <Unix ms>, ...}}`. The keychain entry is service `gemini-cli-oauth`, account `main-account`; the encrypted file keeps the same pair under `{"gemini-cli-oauth": {"main-account": "<that JSON as a string>"}}`.

The keychain is read with the host's own tool, on the machine that runs the agent: `secret-tool lookup service gemini-cli-oauth account main-account` for libsecret on Linux, `security find-generic-password -s gemini-cli-oauth -a main-account -w` on macOS, and the Credential Manager entry `gemini-cli-oauth/main-account` (a generic credential, secret stored as UTF-8) on Windows. A host without the tool, a locked keychain, and a missing entry all mean the keychain holds no sign-in. On macOS the first read of an entry that another program created can raise the system's access prompt; the pull's time bound ends the wait.

The encrypted file is `<iv>:<tag>:<ciphertext>`, each part hex, sealed with AES-256-GCM (12-byte or 16-byte IV, 16-byte tag). The key is `scrypt("gemini-cli-oauth", "<host name>-<user name>-gemini-cli", N=16384, r=8, p=1)` at 32 bytes. The host name and user name belong to the machine that runs the agent, not to Sortie's, so a remote session reads them there (`uname -n` and `id -un`). The plaintext is JSON.

A local session reads on this host and a remote session over its SSH launch. For a remote session the settings above come from the variables the launch forwards, else from the remote shell, which is where the runtime inherits them. Sortie reads these stores and never writes or refreshes them. The runtime confirms a working keychain by writing a probe entry, which the pull does not do, so a keychain that holds no entry sends the pull on to the encrypted file where the runtime would stop.

An expiry at or before the current time means no request and no window; a session signed in with Google refreshes its store itself while it works. When no store holds a sign-in, the session's pulls end.

API-key and Vertex sessions have no Code Assist sign-in, so they normally report no window. The pull looks only at the stores, not at the session's authentication type, so a Google sign-in that other runs on the same home keep fresh makes such a session report that account's quota.

The base URL is `https://cloudcode-pa.googleapis.com`, or the `CODE_ASSIST_ENDPOINT` value in the runtime's launch environment. Both requests are POSTs to `{base}/v1internal:<method>` with `Authorization: Bearer <access_token>`. The token goes only to this host and is never logged.

The project comes from `GOOGLE_CLOUD_PROJECT` in the launch environment, then `GOOGLE_CLOUD_PROJECT_ID`, the order the runtime's own user setup uses. With neither set, `loadCodeAssist` returns it as `cloudaicompanionProject`. The body is the one that user setup sends, which for an account with no project is only the client metadata.

```json
{"metadata": {"ideType": "IDE_UNSPECIFIED", "platform": "PLATFORM_UNSPECIFIED", "pluginType": "GEMINI"}}
```

`retrieveUserQuota` takes `{"project": "<id>"}` and answers one bucket per model and token type, as the runtime's client types declare it:

```json
{"buckets": [{"modelId": "<model>", "tokenType": "<type>", "remainingFraction": 0.82, "remainingAmount": "<count>", "resetTime": "<RFC 3339>"}]}
```

`remainingFraction` runs from 0 to 1 and is the share left. A bucket without `modelId` or `remainingFraction` is skipped, as the runtime skips it, and `remainingAmount` is not read. Each bucket becomes one window named by the model, plus `/` and the token type when present, so a model with several token types shows several windows. A 403 means the account has no Code Assist access and ends the session's pulls.

## Native headless observations

Turn disposition and retry classification are gaps on both native surfaces. The cases were induced, and the surfaces reported no outcome. Cancellation cannot be told apart, because both surfaces are one-shot launches that write their terminal output only at exit. The human-input outcome has no place in either surface's terminal vocabulary. A case a surface stays silent about keeps its obligation and counts against the surface.

## Workspace trust and process boundary

Tool servers declared in `session/new` are merged over the servers in the runtime's own settings, matched by name, with the request winning. Stdio, SSE, and HTTP transports all work.

Delivery needs a trusted workspace. `--skip-trust` grants trust for the session, not just a quieter prompt: it sets the runtime's workspace-trust environment variable, which the trust check reads before any other trust source. An untrusted workspace fails closed with no signal: `session/new` succeeds, the declared servers are dropped, and nothing reports it. The runtime treats protocol mode as interactive, so the guard that rejects an untrusted workspace on the command line never fires here.

A reachable server does not mean callable tools. Because protocol mode counts as interactive, a tool call that matches no policy rule raises a permission request, and Sortie refuses every such request. The call never reaches the server, while the turn still ends normally. A tool reaches its server only when the workspace is trusted and the call is pre-authorized: by a policy rule naming the tool as `mcp_` plus the server name plus `_` plus the tool name, or by an approval mode that allows unmatched calls.

The containment boundary held under measurement: every launch ran inside the run-scoped root, no project settings applied, and every process-group member observed was the launched command or its descendant.

That last clause covers only the launch's own process group. The runtime's shell tool starts each command in a new process group of its own, so a signal to the launch group does not reach it. Sortie still finds such a child while its parent is running, by tracing it through the parent, and kills it by its own process id if it survives shutdown. A child that starts and loses its parent between two readings is never traced. So a clean cleanup row means nothing this run could attribute to itself was left running, not that the runtime left nothing behind.

Sortie's teardown leaves room for a graceful exit. For a local launch it sends the process group a catchable termination signal, closes the runtime's standard input, waits a bounded grace period, and only then force-kills the group. Gemini's history flush finishes inside that window: a session ended this way loaded, replayed, and answered from its history.

## Excluded capability cases

- retry_classification human_input: induced, and the surface reported no outcome (terminal_vocabulary_closed), so the case keeps its obligation
- retry_classification human_input: not applicable on protocol: the request offered a refusing option and was answered inside the protocol, so the turn went on and no human-input outcome arose
- retry_classification non_retryable_refusal: declared outcome_never_produced
- retry_classification unknown_outcome: no deterministic inducer on any runtime, so the case carries no obligation
- turn_disposition cancellation: induced, and the surface reported no outcome (terminal_written_at_exit_only), so the case keeps its obligation
- turn_disposition limit_reached: not induced (prompt_channel_too_small), so the case stays unmeasured on this surface
- turn_disposition runtime_failure: induced, and the surface reported no outcome (output_channel_silent_on_failure), so the case keeps its obligation
- turn_disposition runtime_refusal: declared outcome_never_produced

## Unobserved surfaces

Windows live qualification is unobserved.
