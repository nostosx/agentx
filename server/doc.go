// Package server serves an agent.Runner over HTTP: a REST-style session
// API that streams run events, adapters for the Vercel AI SDK UI message
// stream and AG-UI, and the server side of the A2A protocol.
//
// # Endpoints
//
//	POST   /sessions                      create an empty session → 201 {"id"}
//	GET    /sessions                      list sessions → {"sessions": [...]}
//	GET    /sessions/{id}                 messages and the resumable run, if any
//	DELETE /sessions/{id}                 end the session (Runner.EndSession) → 204
//	                                      ?reason= for SessionEnd hooks, ?purge=true also deletes it
//	POST   /sessions/{id}/runs            run a message (RunRequest); the session is created if needed
//	POST   /sessions/{id}/resume          resume the interrupted or failed run (ResumeRequest)
//	POST   /sessions/{id}/discard         discard the interrupted or failed run → 204
//	POST   /sessions/{id}/clear           Runner.Clear → 201 {"id"} of the new session
//	POST   /sessions/{id}/fork            Runner.Fork → 201 {"id"} of the new session
//	GET    /sessions/{id}/checkpoints     Runner.Checkpoints → {"checkpoints": [...]} (CheckpointInfo)
//	POST   /sessions/{id}/rewind          Runner.Rewind (RewindRequest) → RewindResponse
//	POST   /chat                          Vercel AI SDK useChat endpoint
//	GET    /chat/{id}/stream              useChat stream resumption; always 204
//	POST   /agui                          AG-UI agent endpoint (RunAgentInput)
//	GET    /.well-known/agent-card.json   A2A agent card
//	POST   /a2a                           A2A JSON-RPC endpoint
//
// Mount the handler under a prefix with http.StripPrefix. Request bodies
// are JSON (Content-Type: application/json) of at most WithMaxBodyBytes.
//
// The run and resume endpoints answer according to the Accept header:
//
//   - text/event-stream, or no Accept header: Server-Sent Events in the
//     protocol of WithProtocol or the protocol query parameter (agentx,
//     vercel-ui or ag-ui).
//   - application/x-ndjson or application/jsonl: one event per line, as
//     WriteJSONL writes them.
//   - application/json: the final loop.Result once the run ends, with
//     status 200 whatever the run's status.
//
// # Errors
//
// Errors are JSON bodies {"error": {"code": ..., "message": ...}}:
//
//	400 bad_request             invalid body, session ID or protocol
//	401 unauthorized            WithAuth rejected the request
//	403 forbidden               WithSessionAccess rejected the session
//	404 not_found               the session does not exist
//	409 session_busy            the session has a request in progress on this server
//	409 run_interrupted         the session waits for approvals; the body's "pending" lists them
//	409 nothing_to_resume       resume or discard of a session without such a run
//	409 conflict                session.ErrConflict: another writer saved the session
//	409 cannot_resume           the run's checkpoint is missing; discard it
//	409 cannot_rewind           agent.ErrCannotRewind: no file history, or the message is gone
//	413 payload_too_large       the body exceeds the limit
//	415 unsupported_media_type  the body is not JSON, or a file's media type is not supported
//	500 internal                anything else
//
// Hooks choose another status by returning an *Error. An error after a
// stream has started is sent in the stream: as an "error" event with an
// error record {"type":"error","code":...,"message":...} in the agentx
// protocol and JSON Lines, an "error" chunk in the UI message stream and
// RUN_ERROR in AG-UI. Its code is one of the above, or run_failed for the
// error of a failed run.
//
// # Wire format
//
// Events are loop.Event values encoded as JSON with snake_case names:
//
//	{"type":"message","run_id":"run_…","session_id":"s1","agent":"chat","turn":1,
//	 "message":{"role":"assistant","parts":[{"type":"text","text":"Hi"}]},
//	 "usage":{"input_tokens":10,"output_tokens":5}}
//
// Fields that are empty are omitted. "type" is one of the runtime
// EventType values; the other fields are those of loop.Event, whose
// JSON tags, with those of the model and runtime types they hold, define
// the format. The golden files in testdata show every event type. Over
// the wire the server leaves out Result.Messages, the whole conversation
// (GET /sessions/{id} returns it), and the run state of interrupts, which
// holds the system prompt; Result.new_messages holds the run's messages
// and Result.Err is the string "error". The arguments of a tool call in a
// delta event are a fragment of JSON text, so they are sent as a JSON
// string; concatenate the fragments of a part to get the arguments. Media
// parts, in messages and in tool results, hold their data as base64 in
// "data"; tool results also carry the tool's "metadata". A handoff event
// holds "handoff" ({"from", "to", "tool", "call_id", "input"}), and a
// result names the agent that ran last in "agent" and the guardrail that
// blocked the run in "guardrail".
//
// # Files and media
//
// User messages of every protocol may carry files: images, audio, video,
// text, PDF and Office documents (image/*, audio/*, video/*, text/*,
// application/pdf, application/msword, application/vnd.ms-excel and
// application/vnd.openxmlformats-officedocument.*), inline as data or a
// data: URL or by URL. They become image, audio, video or document parts
// by media type; whether the model takes them depends on its provider.
// Other media types are 415 unsupported_media_type.
//
// # Guardrails
//
// An answer an output guardrail blocks is not kept: the session holds
// "[response withheld by guardrail <name>]" in its place. Guardrails check
// an answer once the model has written it, so by default its text has
// already streamed; with guard.HoldOutput the runtime holds each
// response's events until it calls tools or the guardrails pass, and no
// protocol ever sends a blocked answer.
//
// # Rewind
//
// GET /sessions/{id}/checkpoints lists the session's checkpoints and
// POST /sessions/{id}/rewind {"checkpoint_id": "run_…", "mode": "both"}
// returns the session to one, as agent.Runner.Rewind does: mode "code"
// restores the files of the agent's file history, "conversation" removes
// the checkpoint's message and what followed and returns the message,
// "both" does both. A session waiting for approvals cannot be rewound.
//
// In the agentx protocol each event is a Server-Sent Event named after its
// type, with the event's sequence number in the stream as its ID:
//
//	id: 3
//	event: delta
//	data: {"type":"delta","run_id":"run_…",…}
//
// Streams send a comment line every WithHeartbeat while idle. They end
// after the run_end event, or the error that follows it when the run
// failed. Last-Event-ID is not supported: streams are not buffered.
//
// # Runs and sessions
//
// A session runs one request at a time on a server; another request for a
// busy session gets 409 session_busy instead of waiting. A new session is
// stored when its first run ends. If the client of
// a stream goes away, the run is cancelled as if its consumer stopped
// iterating, and the session is saved. Given a checkpointer
// (agent.WithCheckpointer) the run then ends as a failed run that
// POST /sessions/{id}/resume continues; the next run gives it up.
//
// A run that needs approvals ends with status interrupted. The approvals
// it waits for are in its run_end event and in GET /sessions/{id};
// resume it with {"approvals": {"<call_id>": {"approved": true}}}. The
// server remembers interrupts in memory; give it the checkpointer with
// WithCheckpointer so it can report them after a restart.
//
// Run requests may set max_turns and limits only with AllowRunOptions;
// WithRunOptions sets run options on the server side.
//
// # Vercel AI SDK
//
// POST /chat implements the UI message stream protocol v1
// (https://ai-sdk.dev/docs/ai-sdk-ui/stream-protocol) for useChat with
// the default transport (api: "/chat"). The chat ID is the session ID and
// only the last message is read, since the session holds the
// conversation. Each run is one assistant message: start, start-step and
// finish-step per turn, text-*, reasoning-* and tool-input-* chunks as
// the model streams (or whole when it does not), tool-output-available,
// tool-output-error and tool-output-denied for results, error, and finish
// with the run's status, ID and usage in messageMetadata.agentx, then
// [DONE]. A guardrail block finishes with "content-filter" and the
// guardrail in messageMetadata.agentx.guardrail, which also names the
// agent that ran last. Media in assistant messages and tool results are
// file parts (data: URLs for inline data); handoffs and answers sent back
// for not matching the output schema are data-handoff and
// data-output-retry parts. User messages take file parts. Approvals use the AI SDK's tool approval flow: a
// tool-approval-request whose approvalId is the tool call ID ends the
// stream with finishReason "tool-calls", and the next request, whose last
// message is the assistant message with approval-responded tool parts
// (for example with sendAutomaticallyWhen:
// lastAssistantMessageIsCompleteWithApprovalResponses), resumes the run.
// Tool calls of sub-agents that need approval are sent as dynamic tools.
// Not supported: the regenerate-message trigger, stream resumption, and
// events of sub-agents, which are left out.
//
// # AG-UI
//
// POST /agui implements AG-UI 1.0 events over Server-Sent Events
// (https://docs.ag-ui.com/concepts/events): RUN_STARTED, RUN_FINISHED with
// a success or interrupt outcome and usage, RUN_ERROR, STEP_STARTED and
// STEP_FINISHED per turn, TEXT_MESSAGE_*, REASONING_*, TOOL_CALL_START,
// TOOL_CALL_ARGS, TOOL_CALL_END and TOOL_CALL_RESULT, and SUBAGENT_STARTED,
// SUBAGENT_FINISHED and SUBAGENT_ERROR for sub-agents whose events are
// forwarded, and CUSTOM events named "handoff" and "output_retry". A run
// a guardrail blocked ends with RUN_ERROR code "blocked". The thread ID is
// the session ID; the last message, a user message, is the input: text,
// and image, audio, video and document parts whose source is data
// (base64), a URL or a provider file ID; a "filename" in a part's metadata
// names its file. Media in assistant messages and tool results are not
// sent, as AG-UI has no events for them. Interrupts
// (https://docs.ag-ui.com/concepts/interrupts) have reason "tool_call",
// the tool call ID as their ID and a response schema
// {"approved": boolean, "reason": string, "editedArgs": object}; answer
// them with resume entries on the same thread. Not supported: frontend
// tools, state and messages snapshots, and the binary encoding.
//
// # A2A
//
// POST /a2a implements the JSON-RPC binding of A2A 1.0
// (https://a2a-protocol.org/latest/specification/). Requests must send
// A2A-Version: 1.0; others get VersionNotSupportedError. Methods:
// SendMessage (blocking, or with returnImmediately), SendStreamingMessage,
// GetTask, ListTasks, CancelTask and SubscribeToTask. Push notification
// methods return PushNotificationNotSupportedError and
// GetExtendedAgentCard returns ExtendedAgentCardNotConfiguredError. The
// gRPC and HTTP+JSON bindings are not implemented.
//
// A message's contextId is the session ID; one is generated if it is
// missing. A message without taskId starts a task, which runs in the
// background until it completes, fails, is canceled or requires input,
// whether or not the client keeps listening. Text and data parts and
// files (raw or by URL) are accepted. Interim assistant messages with tool
// calls and handoffs are reported as working status updates; the final
// answer is the text artifact "output". A run a guardrail blocked puts the
// task in TASK_STATE_REJECTED; the task metadata's "agentx" object names
// the guardrail and the agent that ran last. A run that needs approvals puts the task in
// TASK_STATE_INPUT_REQUIRED with the requests in a data part; reply with
// the taskId and a data part {"approved": true} or
// {"approvals": {"<call_id>": {"approved": false, "reason": "..."}}}.
// Tasks are kept in memory, at most A2AConfig.MaxTasks of them. Access to
// a new context that WithSessionAccess denies is an HTTP error; tasks of
// such contexts are reported as not found.
package server
