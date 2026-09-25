-- Refresh model_pricing to current list prices (per million tokens: input / cache read / output).
-- Rows seeded by 001 are corrected only while they still hold their seeded values, so rates a
-- user edited are kept. Lookups try longer patterns first, so the specific rows added here take
-- precedence over the general ones.

-- Claude Opus 5 was seeded at the old Opus 4 rate ($15 / $75); Sonnet 5 at the Sonnet 4 rate.
UPDATE model_pricing SET input_cost_per_m = 5.00, cached_input_cost_per_m = 0.50, output_cost_per_m = 25.00, updated_at = CURRENT_TIMESTAMP
WHERE model_pattern = 'claude-opus-5.*' AND input_cost_per_m = 15.00 AND cached_input_cost_per_m = 1.50 AND output_cost_per_m = 75.00;

UPDATE model_pricing SET input_cost_per_m = 2.00, cached_input_cost_per_m = 0.20, output_cost_per_m = 10.00, updated_at = CURRENT_TIMESTAMP
WHERE model_pattern = 'claude-sonnet-5.*' AND input_cost_per_m = 3.00 AND cached_input_cost_per_m = 0.30 AND output_cost_per_m = 15.00;

-- "claude-haiku-5" is not a model; Haiku 4.5 gets its own row below.
DELETE FROM model_pricing
WHERE model_pattern = 'claude-haiku-5.*' AND input_cost_per_m = 0.80 AND cached_input_cost_per_m = 0.08 AND output_cost_per_m = 4.00;

-- Unanchored, 'gpt-4o' also matched gpt-4o-mini and priced it as gpt-4o.
UPDATE model_pricing SET model_pattern = '^gpt-4o$', updated_at = CURRENT_TIMESTAMP
WHERE model_pattern = 'gpt-4o' AND NOT EXISTS (SELECT 1 FROM model_pricing WHERE model_pattern = '^gpt-4o$');

INSERT OR IGNORE INTO model_pricing (model_pattern, provider, tier, input_cost_per_m, cached_input_cost_per_m, output_cost_per_m)
VALUES
    ('^claude-fable-5-1.*', 'anthropic', 'premium', 10.00, 0.25, 50.00),
    ('^claude-fable-5.*', 'anthropic', 'premium', 10.00, 1.00, 50.00),
    ('^claude-opus-5-5.*', 'anthropic', 'premium', 4.00, 0.20, 20.00),
    ('^claude-opus-4-[5-8].*', 'anthropic', 'premium', 5.00, 0.50, 25.00),
    ('^claude-opus-4.*', 'anthropic', 'premium', 15.00, 1.50, 75.00),
    ('^claude-sonnet-4.*', 'anthropic', 'premium', 3.00, 0.30, 15.00),
    ('^claude-haiku-4-5.*', 'anthropic', 'budget', 1.00, 0.10, 5.00),
    ('^gemini-.*', 'gemini', 'free', 0.00, 0.00, 0.00);
