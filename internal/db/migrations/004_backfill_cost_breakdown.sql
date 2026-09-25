-- Rows logged before the prompt/completion cost split carry only cost_usd. Split it in proportion
-- to the token counts.
UPDATE request_logs
SET prompt_cost_usd = ROUND(cost_usd * CAST(prompt_tokens AS REAL) / CAST(prompt_tokens + completion_tokens AS REAL), 6),
    completion_cost_usd = ROUND(cost_usd - (cost_usd * CAST(prompt_tokens AS REAL) / CAST(prompt_tokens + completion_tokens AS REAL)), 6)
WHERE cost_usd > 0
  AND prompt_cost_usd = 0.0
  AND completion_cost_usd = 0.0
  AND (prompt_tokens + completion_tokens) > 0;
