-- Synthetic Claude Code traffic for the storage benchmark (bench.sh).
--
-- Each session is a growing conversation: request N resends the shared system
-- prompt plus every earlier message, so consecutive requests of one session are
-- near-identical, as in real traffic. Eight sessions run at a time, so in time
-- order a session's previous request sits about eight rows back.
--
-- Text is word salad drawn from the server's own setting descriptions, which keeps
-- natural word frequencies without long verbatim repeats that would flatter the
-- time-ordered baseline. Message texts contain JSON-escaped line breaks like real
-- bodies. Session 17 mentions a rare needle at turn 5, so every later request of
-- that session contains it.

CREATE DATABASE IF NOT EXISTS bench;
DROP TABLE IF EXISTS bench.src;
CREATE TABLE bench.src
(
    session_id String,
    machine LowCardinality(String),
    started_at DateTime64(3, 'UTC'),
    client_request String,
    upstream_request String,
    response String
)
ENGINE = MergeTree
ORDER BY tuple();

INSERT INTO bench.src
WITH
    (SELECT arrayFilter(w -> length(w) > 1, flatten(groupArray(splitByNonAlpha(description)))) FROM system.settings) AS words,
    length(words) AS nw,
    (s, j) -> concat(
        '{"role":"', if(j % 2 = 0, 'user', 'assistant'), '","content":[{"type":"',
        if(j % 2 = 0, 'tool_result","tool_use_id":"toolu_', 'text","id":"msg_'), hex(cityHash64(s, j)), '","text":"',
        arrayStringConcat(arrayMap(k -> concat(words[1 + cityHash64(s, j, k) % nw], if(cityHash64(k, s, j) % 13 = 0, '\\n', '')),
                                   range(300 + cityHash64(j, s) % 2000)), ' '),
        if(s = 17 AND j = 5, ' the needle xylophonequartz42 is here', ''),
        '"}]}') AS msg,
    concat('[{"type":"text","text":"', arrayStringConcat(arrayMap(k -> words[1 + cityHash64(424242, k) % nw], range(6000)), ' '), '"}]') AS prefix
SELECT
    concat('sess-', toString(s)) AS session_id,
    ['bigbook', 'workbook', 'beebook'][1 + s % 3] AS machine,
    toDateTime64('2026-10-01 00:00:00', 3, 'UTC')
        + toIntervalSecond(intDiv(s, 8) * 14400 + t * 90 + (s % 8) * 11 + cityHash64(s, t) % 30) AS started_at,
    concat('{"model":"claude-opus-4-5","max_tokens":32000,"stream":true,"system":', prefix,
           ',"messages":[', arrayStringConcat(arrayMap(j -> msg(s, j), range(t + 1)), ','),
           '],"metadata":{"user_id":"user_abc_session_', session_id, '"}}') AS client_request,
    replaceOne(client_request, '"model":"claude-opus-4-5"', '"model":"claude-opus-4-5-20251101"') AS upstream_request,
    arrayStringConcat(arrayMap(w -> concat('event: content_block_delta\ndata: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"', w, ' "}}\n\n'),
        arrayMap(k -> words[1 + cityHash64(s, t + 1, k) % nw], range(200 + cityHash64(t, s) % 800))), '') AS response
FROM numbers({sessions:UInt32}) AS a
CROSS JOIN numbers(40) AS b
ARRAY JOIN [a.number] AS s, [b.number] AS t
SETTINGS max_block_size = 64;
