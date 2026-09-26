-- Per-key daily spend cap and calendar periods for key spend (UTC).
--
-- daily_budget_usd caps a key's spend per UTC day (0 = no daily cap). daily_spend_usd holds the
-- spend of the UTC day named in daily_spend_date ('YYYY-MM-DD'); any other date means nothing has
-- been spent today.
--
-- spend_month ('YYYY-MM') names the UTC month that current_spend_usd belongs to. Before this
-- migration current_spend_usd only ever grew, so a monthly budget, once reached, blocked the key's
-- upstream requests for good. Existing keys are stamped with the current month so the spend they
-- already have keeps counting until the month ends.
ALTER TABLE api_keys ADD COLUMN daily_budget_usd REAL NOT NULL DEFAULT 0;
ALTER TABLE api_keys ADD COLUMN daily_spend_usd REAL NOT NULL DEFAULT 0;
ALTER TABLE api_keys ADD COLUMN daily_spend_date TEXT NOT NULL DEFAULT '';
ALTER TABLE api_keys ADD COLUMN spend_month TEXT NOT NULL DEFAULT '';

UPDATE api_keys SET spend_month = strftime('%Y-%m', 'now');
