-- request_tail: the part of client_request that is new in this request, so history
-- search can skip client_request. Safe to run repeatedly (IF NOT EXISTS).
--
-- Why: a client resends the whole conversation with every request, so client_request
-- grows with each turn and the table repeats each message hundreds of times (275 GiB
-- uncompressed after one week). A substring search over a week of it ran past the
-- reader's 60-second query limit. Every message of a conversation is either new
-- input in exactly one request (the typed prompt, tool results) or part of exactly
-- one response (the assistant's turn), so request_tail plus response hold the whole
-- conversation once: about 1.7 GiB a week instead of 275 GiB.
--
-- What it holds: the raw JSON of the messages after the last assistant turn,
-- one per line. For Claude and Chat Completions (messages) and Gemini (contents) that
-- is every message after the last assistant (Gemini: model) message, a Gemini turn
-- without a role counting as the user's; for OpenAI Responses (input) every item after
-- the last assistant message or role-less item of the assistant's (reasoning,
-- function_call, and similar; *_output items, the tool results, count as input). An
-- item with an unknown role, or with neither role nor type, counts as input. Bodies
-- that nest their turns elsewhere, such as gemini-cli's request.contents, give an
-- empty value; the fleet sends Claude and Codex traffic only. An image-generation
-- request (openai-image) keeps its prompt string. A plain-string input is kept whole. The system prompt and
-- tool definitions are left out: they repeat in every request and match every search.
-- Base64 payloads of 256 characters or more under data, image_url, file_data, or url
-- (screenshots, files) are emptied; they are most of the bytes and hold no text.
-- A body that is not valid JSON (cut at max-body-bytes, marked truncated) yields an
-- empty value. History a session starts with is not new, so it is not kept: a forked
-- subagent's copy of its parent's conversation (the parent's own rows hold it) or the
-- earlier turns of a resumed session logged under a new ID.
--
-- MATERIALIZED: ClickHouse computes the value on insert (7 to 30 ms of CPU per row,
-- mostly JSON parsing), so the plugin and its row format do not change, and SELECT *
-- and the plugin's inserts leave it out. Rows written before this column existed
-- compute it on every read until the one-time backfill rewrites them; see README.md
-- ("Backfill request_tail").
--
-- ADD COLUMN IF NOT EXISTS never changes an existing column, so rerunning install.sh
-- after editing the expression below does nothing on a database that already has
-- it: apply the edit with ALTER TABLE ... MODIFY COLUMN, then MATERIALIZE COLUMN again.
--
-- Plain ZSTD(3) with default blocks: search scans this column, and the 16-32 MiB
-- blocks of the body columns would make every scan decompress far more than it reads.

ALTER TABLE cliproxy_log.requests
    ADD COLUMN IF NOT EXISTS request_tail String MATERIALIZED replaceRegexpAll(
        if(empty(JSONExtract(client_request, multiIf(source_format = 'openai-response', 'input', startsWith(source_format, 'gemini'), 'contents', source_format = 'openai-image', 'prompt', 'messages'), 'Array(Tuple(role String, type String))')),
            JSONExtractRaw(client_request, multiIf(source_format = 'openai-response', 'input', startsWith(source_format, 'gemini'), 'contents', source_format = 'openai-image', 'prompt', 'messages')),
            arrayStringConcat(
                arrayMap(
                    i -> JSONExtractRaw(client_request, multiIf(source_format = 'openai-response', 'input', startsWith(source_format, 'gemini'), 'contents', source_format = 'openai-image', 'prompt', 'messages'), i),
                    range(
                        toUInt64(arrayLastIndex(
                            m -> tupleElement(m, 1) IN ('assistant', 'model') OR (tupleElement(m, 1) = '' AND tupleElement(m, 2) != '' AND NOT endsWith(tupleElement(m, 2), '_output')),
                            JSONExtract(client_request, multiIf(source_format = 'openai-response', 'input', startsWith(source_format, 'gemini'), 'contents', source_format = 'openai-image', 'prompt', 'messages'), 'Array(Tuple(role String, type String))')) + 1),
                        length(JSONExtract(client_request, multiIf(source_format = 'openai-response', 'input', startsWith(source_format, 'gemini'), 'contents', source_format = 'openai-image', 'prompt', 'messages'), 'Array(Tuple(role String, type String))')) + 1)),
                '\n')),
        '"(data|image_url|file_data|url)"\\s*:\\s*"(data:[^",;]*;base64,)?[A-Za-z0-9+/=]{256,}"', '"\\1":""')
    CODEC(ZSTD(3));
